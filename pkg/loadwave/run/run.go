// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package run turns a Go program into a complete LoadWave binary.
//
// A test written with the SDK registers its scenarios and then hands control
// here. The resulting binary is not just a test — it is the coordinator, the
// agent and the worker as well, so a distributed run needs nothing deployed
// alongside it:
//
//	package main
//
//	import (
//	    "github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
//	    "github.com/SnowyFoxStudios/LoadWave/pkg/loadwave/run"
//	)
//
//	func main() {
//	    loadwave.Register(loadwave.Scenario{Name: "browse", Run: browse})
//	    run.Main()
//	}
//
// Build it once, copy it to every load-generating host, and run `agent` there
// against a coordinator started with `serve`. Because the scenarios are
// compiled in, every host is guaranteed to be running the same test.
//
// This is a separate package from loadwave itself so that the SDK stays free
// of the CLI's dependencies: a program that only imports loadwave — a scenario
// under unit test, say — pulls in none of it.
package run

import (
	"os"

	"github.com/SnowyFoxStudios/LoadWave/internal/cli"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// Main runs the LoadWave command line against the default scenario registry
// and exits with the resulting status code.
//
// It does not return.
func Main() { os.Exit(Execute(loadwave.Default)) }

// Execute runs the command line against a specific registry and returns the
// exit code instead of exiting.
//
// Use it when the program has cleanup of its own to do, or in a test that
// needs to drive the CLI without taking the test binary down:
//
//	func main() {
//	    code := run.Execute(loadwave.Default)
//	    closeThings()
//	    os.Exit(code)
//	}
//
// Exit codes are part of the contract: 0 for a clean run, 1 if the command
// itself failed, and 2 if the run completed but breached a threshold.
func Execute(registry *loadwave.Registry) int { return cli.Execute(registry) }
