// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
	"github.com/SnowyFoxStudios/LoadWave/internal/report"
	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
)

// runOptions are the flags specific to `loadwave run`.
type runOptions struct {
	plan planFlags

	ui         bool
	uiAddr     string
	quiet      bool
	out        string
	reportPath string
	agents     int
	waitAgents time.Duration
	maxVUs     int
	resolution time.Duration
}

// stopGrace is how long the CLI keeps collecting results after asking a run to
// stop, before giving up and reporting what it has.
const stopGrace = 60 * time.Second

func newRunCommand(opts *options) *cobra.Command {
	ro := &runOptions{}

	cmd := &cobra.Command{
		Use:   "run [config.yaml]",
		Short: "Run a load test and print the results",
		Long: `Run a load test to completion.

Starts a coordinator, an agent and its worker processes in one command, runs
the test, prints a report, and exits 2 if any threshold was breached — which
is what makes it usable as a CI gate.

Add --ui to serve the live dashboard alongside the run.`,
		Example: `  # Run a configuration file
  loadwave run test.yaml

  # Quick check with no file
  loadwave run --url https://example.com --vus 50 --duration 30s

  # Ramp up, watch it live, fail the build if the p95 exceeds 500ms
  loadwave run test.yaml --ui --threshold 'http_req_duration:p95<500'`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 1 {
				path = args[0]
			}
			return ro.execute(cmd, opts, path)
		},
	}

	ro.plan.register(cmd.Flags())
	cmd.Flags().BoolVar(&ro.ui, "ui", false, "serve the live dashboard during the run")
	cmd.Flags().StringVar(&ro.uiAddr, "ui-addr", ":8088", "dashboard listen address")
	cmd.Flags().BoolVarP(&ro.quiet, "quiet", "q", false, "suppress the live progress line")
	cmd.Flags().StringVarP(&ro.out, "out", "o", "", "write the run summary as JSON to this file")
	cmd.Flags().StringVar(&ro.reportPath, "report", "",
		"write a self-contained HTML report, with charts, to this file")
	cmd.Flags().IntVar(&ro.agents, "agents", 1, "number of agents to wait for before starting")
	cmd.Flags().DurationVar(&ro.waitAgents, "wait-agents", 30*time.Second,
		"how long to wait for agents to connect")
	cmd.Flags().IntVar(&ro.maxVUs, "max-vus", 0,
		"virtual user ceiling this machine advertises (default: 1000 per core)")
	cmd.Flags().DurationVar(&ro.resolution, "resolution", time.Second,
		"metric bucket width, and therefore the chart's time resolution")

	return cmd
}

// execute loads the configuration and performs the whole run.
func (ro *runOptions) execute(cmd *cobra.Command, opts *options, path string) error {
	cfg, err := ro.plan.load(path)
	if err != nil {
		return err
	}
	if err := verifyScenarios(cfg, opts); err != nil {
		return err
	}

	// Absolute, so the dashboard can save an edit back to the right file
	// regardless of what the process's working directory happens to be by
	// the time somebody clicks Start in the browser.
	sourcePath := ""
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			sourcePath = abs
		} else {
			sourcePath = path
		}
	}
	return ro.executeConfig(cmd, opts, cfg, sourcePath)
}

// executeConfig runs an already-validated configuration.
//
// Split out from execute so that `demo` — which builds its configuration in
// memory rather than reading a file — goes through exactly the same path as a
// real run, dashboard and reporting included. sourcePath is the file cfg was
// loaded from, or empty when there isn't one.
func (ro *runOptions) executeConfig(
	cmd *cobra.Command, opts *options, cfg *scenario.Config, sourcePath string,
) error {
	ctx := cmd.Context()

	// The cluster deliberately does not inherit the signal context. An
	// interrupt should ask the run to stop gracefully and let the results
	// come back, not tear the worker processes down mid-request and report
	// a cliff of cancellations as failures.
	cluster, err := startCluster(context.WithoutCancel(ctx), clusterConfig{
		LocalAgent:    true,
		Workers:       cfg.WorkersPerAgent,
		MaxVUs:        ro.maxVUs,
		Dashboard:     ro.ui,
		DashboardAddr: ro.uiAddr,
		Resolution:    ro.resolution,
		AllowShutdown: ro.ui,
		Registry:      opts.registry,
		Logger:        loggerFor(cmd),
	})
	if err != nil {
		return err
	}
	defer cluster.stop()

	if err := cluster.awaitAgents(ctx, ro.agents, ro.waitAgents); err != nil {
		return err
	}
	if ro.ui {
		fmt.Fprint(cmd.OutOrStdout(), demoBanner(cluster.dashboardURL()))
	}

	run, err := cluster.coordinator.StartRun(cfg, sourcePath)
	if err != nil {
		return failf("%v", err)
	}

	reporter := newReporter(cmd.OutOrStdout(), ro.quiet)
	snapshot := ro.follow(ctx, cmd, cluster, run, reporter)
	reporter.final(snapshot)

	if ro.out != "" {
		if err := writeSummary(ro.out, snapshot); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  summary written to %s\n", ro.out)
	}
	if ro.reportPath != "" {
		if err := writeReport(ro.reportPath, snapshot); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  report written to %s\n", ro.reportPath)
	}
	if ro.out != "" || ro.reportPath != "" {
		fmt.Fprintln(cmd.OutOrStdout())
	}

	// The run is over, but the operator asked for a dashboard — so the
	// dashboard stays. Tearing it down the moment the last request finishes
	// would take away the results at exactly the moment somebody wants to
	// read them, download the report, or start another run.
	if ro.ui {
		ro.linger(ctx, cmd, cluster)
	}

	return verdict(snapshot, run)
}

// linger keeps the coordinator and dashboard up after a scripted run ends.
//
// It returns when the operator asks to shut down, either with Ctrl-C or with
// the dashboard's power-off control. An interrupt that already arrived during
// the run counts: pressing Ctrl-C means "I am done", so the report prints and
// the process exits rather than waiting to be told twice.
func (ro *runOptions) linger(ctx context.Context, cmd *cobra.Command, cluster *cluster) {
	if ctx.Err() != nil {
		return
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "  The run has finished, but LoadWave is still up.\n\n")
	fmt.Fprintf(out, "    dashboard   %s\n", cluster.dashboardURL())
	fmt.Fprintf(out, "    from here   start another run, or download the report\n")
	fmt.Fprintf(out, "    to exit     Ctrl-C, or Power off in the browser\n\n")

	select {
	case <-ctx.Done():
	case <-cluster.poweredOff():
		fmt.Fprintf(cmd.ErrOrStderr(), "%s; shutting down\n", cluster.reason)
	}
}

// follow renders live progress until the run reaches a terminal phase, and
// returns the final snapshot.
func (ro *runOptions) follow(
	ctx context.Context, cmd *cobra.Command, cluster *cluster,
	run *coordinator.Run, reporter *reporter,
) coordinator.Snapshot {
	sub := cluster.coordinator.Subscribe()
	defer sub.Close()

	// Nulled out once handled, so the loop does not spin on an already-closed
	// channel while it waits for the run to wind down.
	interrupted := ctx.Done()
	var giveUp <-chan time.Time

	for run.Active() {
		select {
		case <-interrupted:
			interrupted = nil
			giveUp = time.After(stopGrace)
			reporter.endProgress()
			fmt.Fprintln(cmd.ErrOrStderr(), "\ninterrupted; stopping gracefully…")
			_ = cluster.coordinator.StopRun(run.ID(), true, "interrupted by operator")

		case <-giveUp:
			reporter.endProgress()
			fmt.Fprintln(cmd.ErrOrStderr(), "run did not stop in time; reporting what we have")
			return finalSnapshot(cluster, run)

		case <-sub.Done():
			return finalSnapshot(cluster, run)

		case update := <-sub.Updates():
			if update.Type != coordinator.UpdateTick || len(update.Ticks) == 0 {
				continue
			}
			reporter.progress(update.Ticks[len(update.Ticks)-1], update.Run)
		}
	}

	reporter.endProgress()
	return finalSnapshot(cluster, run)
}

// finalSnapshot collects the run's complete state once it has settled.
//
// The brief pause lets the last metric buckets close: workers flush on a
// wall-clock boundary and the store holds a bucket open for a grace period, so
// reporting the instant the phase flips would consistently lose the final
// second of every run.
func finalSnapshot(cluster *cluster, run *coordinator.Run) coordinator.Snapshot {
	time.Sleep(cluster.settleDelay())

	snapshot, ok := cluster.coordinator.RunSnapshot(run.ID())
	if !ok {
		return cluster.coordinator.Snapshot()
	}
	return snapshot
}

// verdict maps the run's outcome onto an exit code.
func verdict(snapshot coordinator.Snapshot, run *coordinator.Run) error {
	if snapshot.Run == nil {
		return failf("the run produced no results")
	}

	switch {
	case snapshot.Run.Phase == coordinator.PhaseFailed:
		return exitError{code: ExitError, message: "run failed: " + snapshot.Run.Failure}
	case run.Breached():
		return exitError{code: ExitThresholdBreached, message: "one or more thresholds were breached"}
	default:
		return nil
	}
}

// verifyScenarios checks the configuration names scenarios this binary can
// actually execute, before any infrastructure is started.
//
// Catching it here turns a confusing mid-run failure on a remote worker into
// an immediate, local error message naming the scenario that is missing.
func verifyScenarios(cfg *scenario.Config, opts *options) error {
	probe := opts.registry.Clone()
	if err := cfg.BuildScenarios(probe); err != nil {
		return failf("%v", err)
	}
	if probe.Len() == 0 {
		return failf("no scenarios to run: define them in the configuration's `scenarios:` " +
			"section, or register them in Go with loadwave.Register")
	}
	return nil
}

// writeReport persists the run as a self-contained HTML file.
//
// Written through a buffer so that a template failure leaves no file at all,
// rather than a half-written one that opens and looks plausible.
func writeReport(path string, snapshot coordinator.Snapshot) error {
	data, err := report.Build(snapshot, time.Now())
	if err != nil {
		return failf("build report: %v", err)
	}

	var body bytes.Buffer
	if err := report.Render(&body, data); err != nil {
		return failf("render report: %v", err)
	}
	if err := os.WriteFile(path, body.Bytes(), 0o644); err != nil {
		return failf("write %s: %v", path, err)
	}
	return nil
}

// writeSummary persists the run report as JSON.
func writeSummary(path string, snapshot coordinator.Snapshot) error {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return failf("encode summary: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return failf("write %s: %v", path, err)
	}
	return nil
}
