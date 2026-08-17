// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SnowyFoxStudios/LoadWave/internal/agent"
	"github.com/SnowyFoxStudios/LoadWave/internal/worker"
)

// loggerFor returns the logger a command should hand to the components it
// starts.
func loggerFor(_ *cobra.Command) *slog.Logger { return slog.Default() }

// settleDelay is how long to wait after a run ends before reading the final
// numbers, covering one metric flush plus the store's late-arrival grace.
func (c *cluster) settleDelay() time.Duration { return 2500 * time.Millisecond }

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func newServeCommand(opts *options) *cobra.Command {
	var (
		listen        string
		uiAddr        string
		localAgent    bool
		workers       int
		maxVUs        int
		readOnly      bool
		allowShutdown bool
		origins       []string
		resolution    time.Duration
		window        time.Duration
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the coordinator and the web dashboard",
		Long: `Start a long-lived coordinator with the dashboard attached.

Runs are started from the browser or through the REST API, rather than from
the command line. A local agent is started too, so a single machine works out
of the box; pass --local-agent=false when this host should only coordinate and
the load will come from agents elsewhere.`,
		Example: `  # Everything on one machine
  loadwave serve

  # A coordinator for a fleet, generating no load itself
  loadwave serve --listen 0.0.0.0:8090 --local-agent=false`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cluster, err := startCluster(ctx, clusterConfig{
				CoordinatorAddr: listen,
				LocalAgent:      localAgent,
				Workers:         workers,
				MaxVUs:          maxVUs,
				Dashboard:       true,
				DashboardAddr:   uiAddr,
				AllowedOrigins:  origins,
				ReadOnly:        readOnly,
				AllowShutdown:   allowShutdown,
				Registry:        opts.registry,
				Resolution:      resolution,
				Window:          window,
				Logger:          loggerFor(cmd),
			})
			if err != nil {
				return err
			}
			defer cluster.stop()

			exit := "Press Ctrl-C to stop."
			if allowShutdown {
				exit = "Press Ctrl-C, or use Power off in the browser, to stop."
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"LoadWave is ready.\n\n  dashboard    %s\n  agents dial  %s\n\n%s\n",
				cluster.dashboardURL(), cluster.coordinator.Addr(), exit)

			select {
			case <-ctx.Done():
			case <-cluster.poweredOff():
				fmt.Fprintf(cmd.ErrOrStderr(), "\n%s\n", cluster.reason)
			}

			fmt.Fprintln(cmd.ErrOrStderr(), "shutting down…")
			return cluster.err()
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:8090", "address agents connect to")
	cmd.Flags().StringVar(&uiAddr, "ui-addr", ":8088", "dashboard listen address")
	cmd.Flags().BoolVar(&localAgent, "local-agent", true, "also run an agent on this machine")
	cmd.Flags().IntVarP(&workers, "workers", "w", 0,
		"worker processes for the local agent (default: one per core, less one)")
	cmd.Flags().IntVar(&maxVUs, "max-vus", 0,
		"virtual user ceiling the local agent advertises (default: 1000 per core)")
	cmd.Flags().BoolVar(&readOnly, "read-only", false,
		"serve the dashboard without allowing runs to be started or stopped")
	// Off by default here: a long-lived coordinator is usually run under a
	// supervisor, and a browser tab should not be able to take it down.
	// `loadwave run --ui` and `loadwave demo` enable it, since there the
	// process belongs to the person looking at the dashboard.
	cmd.Flags().BoolVar(&allowShutdown, "allow-shutdown", false,
		"let the dashboard's Power off control stop this process")
	cmd.Flags().StringSliceVar(&origins, "allowed-origin", nil,
		"extra browser origin allowed to open the live stream (repeatable)")
	cmd.Flags().DurationVar(&resolution, "resolution", time.Second, "metric bucket width")
	cmd.Flags().DurationVar(&window, "window", time.Hour, "how much chart history to retain")

	return cmd
}

// ---------------------------------------------------------------------------
// agent
// ---------------------------------------------------------------------------

func newAgentCommand(_ *options) *cobra.Command {
	var (
		coordinatorAddr string
		nodeID          string
		workers         int
		maxVUs          int
		labels          []string
	)

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Join a coordinator and generate load on this machine",
		Long: `Run an agent that joins a coordinator elsewhere.

The agent spawns worker processes on this host and runs whatever share of the
load the coordinator assigns it. It dials out, so it needs no inbound ports and
works from behind NAT.

The binary must be the same one the rest of the fleet is running: for a test
written with the Go SDK, the scenarios are compiled into it.`,
		Example: `  loadwave agent --coordinator loadwave.internal:8090
  loadwave agent --coordinator 10.0.0.5:8090 --workers 8 --label region=eu-west-1`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if coordinatorAddr == "" {
				return failf("--coordinator is required")
			}

			parsed, err := parseLabels(labels)
			if err != nil {
				return err
			}
			if nodeID == "" {
				nodeID = defaultNodeID()
			}

			node, err := agent.New(agent.Config{
				NodeID:            nodeID,
				CoordinatorTarget: coordinatorAddr,
				Workers:           workers,
				MaxVUs:            maxVUs,
				Labels:            parsed,
				Logger:            loggerFor(cmd),
			})
			if err != nil {
				return failf("%v", err)
			}

			if err := node.Run(cmd.Context()); err != nil {
				return failf("agent stopped: %v", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&coordinatorAddr, "coordinator", "c", "",
		"coordinator address, e.g. loadwave.internal:8090 (required)")
	cmd.Flags().StringVar(&nodeID, "node-id", "",
		"identifier for this agent (default: hostname and pid)")
	cmd.Flags().IntVarP(&workers, "workers", "w", 0,
		"worker processes to spawn (default: one per core, less one)")
	cmd.Flags().IntVar(&maxVUs, "max-vus", 0,
		"virtual user ceiling to advertise (default: 1000 per core)")
	cmd.Flags().StringSliceVar(&labels, "label", nil,
		"label advertised to the coordinator, as key=value (repeatable)")

	return cmd
}

// defaultNodeID builds a stable-enough identifier for an agent.
//
// The pid is included so that restarting an agent produces a fresh identity
// rather than colliding with the session the coordinator has not yet noticed
// is dead.
func defaultNodeID() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "agent"
	}
	return fmt.Sprintf("%s-%d", hostname, os.Getpid())
}

// parseLabels reads repeated key=value flags.
func parseLabels(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(raw))
	for _, entry := range raw {
		key, value, err := splitPair(entry, "=")
		if err != nil {
			return nil, failf("--label %q: %v", entry, err)
		}
		out[key] = value
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// worker
// ---------------------------------------------------------------------------

func newWorkerCommand(opts *options) *cobra.Command {
	var (
		socket string
		nodeID string
		maxVUs int
	)

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a worker process (started by an agent)",
		Long: `Run a single worker process.

Agents spawn this themselves; there is rarely a reason to run it by hand. It
joins its agent over a Unix socket, waits to be assigned a share of a run, and
drives that share's virtual users.`,
		// Hidden rather than removed: an operator debugging a worker that
		// will not start needs to be able to run exactly what the agent runs.
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if socket == "" {
				return failf("--agent-socket is required")
			}
			if nodeID == "" {
				return failf("--node-id is required")
			}

			node, err := worker.New(worker.Config{
				NodeID:      nodeID,
				AgentTarget: unixTarget(socket),
				Registry:    opts.registry,
				MaxVUs:      maxVUs,
				Logger:      loggerFor(cmd),
			})
			if err != nil {
				return failf("%v", err)
			}

			if err := node.Run(cmd.Context()); err != nil {
				return failf("worker stopped: %v", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&socket, "agent-socket", "", "path to the agent's control socket")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "identifier assigned by the agent")
	cmd.Flags().IntVar(&maxVUs, "max-vus", 0, "virtual user ceiling for this process")

	return cmd
}

// unixTarget turns a socket path into the gRPC target syntax.
func unixTarget(path string) string {
	if strings.Contains(path, "://") {
		return path
	}
	return "unix://" + path
}
