// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cli implements the loadwave command line.
//
// The same command set is compiled into the stock `loadwave` binary and into
// any binary a user builds with the Go SDK, which is what lets a test written
// in Go be its own coordinator, agent and worker with no extra tooling.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/SnowyFoxStudios/LoadWave/internal/buildinfo"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// Exit codes. These are a contract with CI systems, so they are documented
// here and must not be reshuffled.
const (
	// ExitOK means the run completed and every threshold passed.
	ExitOK = 0
	// ExitError means the command could not do what was asked: a bad
	// configuration, an unreachable coordinator, no agents.
	ExitError = 1
	// ExitThresholdBreached means the run completed but a threshold failed.
	// Distinct from ExitError so a pipeline can tell "the tool broke" from
	// "the service under test was too slow".
	ExitThresholdBreached = 2
	// ExitInterrupted means the operator stopped the run.
	ExitInterrupted = 130
)

// options are the flags shared by every subcommand.
type options struct {
	logLevel  string
	logFormat string
	registry  *loadwave.Registry
}

// Execute runs the command line and returns a process exit code.
//
// It returns rather than calling os.Exit so that deferred cleanup in callers
// still runs, and so that tests can exercise commands without taking the test
// binary down with them.
func Execute(registry *loadwave.Registry) int {
	if registry == nil {
		registry = loadwave.Default
	}
	opts := &options{registry: registry}

	root := &cobra.Command{
		Use:   "loadwave",
		Short: "Distributed load testing with a live dashboard",
		Long: strings.TrimSpace(`
LoadWave generates load from a fleet of worker processes, aggregates the
results centrally, and shows them live.

A single machine needs only:

    loadwave run test.yaml

which starts a coordinator, an agent and its worker processes, runs the test,
prints a report and exits non-zero if a threshold was breached.

Add --ui to watch it happen in a browser, or spread the load across machines
by running "loadwave serve" on one host and "loadwave agent" on the others.`),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildinfo.Version(),
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return opts.configureLogging(cmd)
		},
	}

	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	root.PersistentFlags().StringVar(&opts.logLevel, "log-level", "info",
		"log verbosity: debug, info, warn or error")
	root.PersistentFlags().StringVar(&opts.logFormat, "log-format", "text",
		"log format: text or json")

	root.AddCommand(
		newDemoCommand(opts),
		newRunCommand(opts),
		newServeCommand(opts),
		newAgentCommand(opts),
		newWorkerCommand(opts),
		newValidateCommand(opts),
		newVersionCommand(),
	)

	err := root.ExecuteContext(signalContext())
	if err == nil {
		return ExitOK
	}

	var coded exitError
	if errors.As(err, &coded) {
		if coded.message != "" {
			fmt.Fprintln(os.Stderr, "loadwave:", coded.message)
		}
		return coded.code
	}

	fmt.Fprintln(os.Stderr, "loadwave:", err)
	return ExitError
}

// exitError carries a specific exit code out of a command.
type exitError struct {
	code    int
	message string
}

func (e exitError) Error() string { return e.message }

// failf returns an error that exits with the generic failure code.
func failf(format string, args ...any) error {
	return exitError{code: ExitError, message: fmt.Sprintf(format, args...)}
}

// signalContext returns a context cancelled on the first interrupt, and left
// alone after that.
//
// The second interrupt is deliberately not handled: the first asks for a
// graceful stop, and an operator who presses Ctrl-C again wants the process
// gone now, not a more polite shutdown.
func signalContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx
}

// configureLogging installs the global logger.
func (o *options) configureLogging(cmd *cobra.Command) error {
	var level slog.Level
	switch strings.ToLower(o.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return failf("unknown log level %q: expected debug, info, warn or error", o.logLevel)
	}

	// Logs go to stderr so that stdout stays clean for the run report, which
	// makes `loadwave run ... > report.txt` behave the way anyone would
	// expect.
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(o.logFormat) {
	case "json":
		handler = slog.NewJSONHandler(cmd.ErrOrStderr(), opts)
	case "text", "":
		handler = slog.NewTextHandler(cmd.ErrOrStderr(), opts)
	default:
		return failf("unknown log format %q: expected text or json", o.logFormat)
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

// newVersionCommand prints detailed build information.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Get())
			return nil
		},
	}
}
