// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/SnowyFoxStudios/LoadWave/internal/engine"
)

func newValidateCommand(opts *options) *cobra.Command {
	var plan planFlags

	cmd := &cobra.Command{
		Use:   "validate [config.yaml]",
		Short: "Check a configuration without running it",
		Long: `Parse a configuration, resolve its scenarios and describe what it would do.

Nothing is executed and no load is generated. This is what belongs in a
pre-commit hook or a pull request check: it catches a misspelled field, a
scenario name that does not exist in this binary, or a threshold on a metric
that will never be produced — all in milliseconds, long before anyone waits
out a real run to discover it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 1 {
				path = args[0]
			}

			cfg, err := plan.load(path)
			if err != nil {
				return err
			}

			registry := opts.registry.Clone()
			if err := cfg.BuildScenarios(registry); err != nil {
				return failf("%v", err)
			}

			testPlan, err := cfg.Plan()
			if err != nil {
				return failf("%v", err)
			}
			executor, err := engine.NewExecutor(testPlan.GetLoad())
			if err != nil {
				return failf("%v", err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Configuration is valid.\n\n")

			writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(writer, "  name\t%s\n", cfg.Name)
			if cfg.BaseURL != "" {
				fmt.Fprintf(writer, "  base URL\t%s\n", cfg.BaseURL)
			}
			fmt.Fprintf(writer, "  profile\t%s\n", executor.Describe())
			fmt.Fprintf(writer, "  peak VUs\t%d\n", executor.Peak())
			if d := executor.Duration(); d > 0 {
				fmt.Fprintf(writer, "  duration\t%s\n", d)
			}
			if n := testPlan.GetLoad().GetIterations(); n > 0 {
				fmt.Fprintf(writer, "  iterations\t%d\n", n)
			}
			if rate := testPlan.GetLoad().GetMaxIterationsPerSecond(); rate > 0 {
				fmt.Fprintf(writer, "  arrival rate\t%d iterations/s\n", rate)
			}
			if cfg.WorkersPerAgent > 0 {
				fmt.Fprintf(writer, "  workers/agent\t%d\n", cfg.WorkersPerAgent)
			}

			// Shown always, including when defaulted: pacing governs how much
			// traffic the run actually produces, and leaving it implicit is
			// how somebody ends up surprised by the request rate.
			pacing := cfg.BetweenRequestsPause()
			switch {
			case pacing.IsZero():
				fmt.Fprintf(writer, "  between requests\tnone — flat out\n")
			case cfg.BetweenRequests == "":
				fmt.Fprintf(writer, "  between requests\t%s (default)\n", pacing)
			default:
				fmt.Fprintf(writer, "  between requests\t%s\n", pacing)
			}
			_ = writer.Flush()

			names := registry.Names()
			sort.Strings(names)
			fmt.Fprintf(out, "\n  scenarios (%d):\n", len(names))
			for _, name := range names {
				sc, _ := registry.Lookup(name)
				fmt.Fprintf(out, "    - %s (weight %d)", name, sc.EffectiveWeight())
				if sc.Description != "" {
					fmt.Fprintf(out, " — %s", sc.Description)
				}
				fmt.Fprintln(out)
			}

			if len(cfg.Thresholds) > 0 {
				fmt.Fprintf(out, "\n  thresholds (%d):\n", len(cfg.Thresholds))
				for _, t := range cfg.Thresholds {
					fmt.Fprintf(out, "    - %s %s %s %g\n", t.Metric, t.Stat, t.Op, t.Value)
				}
			}
			fmt.Fprintln(out)
			return nil
		},
	}

	plan.register(cmd.Flags())
	return cmd
}
