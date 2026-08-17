// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package loadwave is the public API for writing LoadWave load tests in Go.
//
// A test is a normal Go program. It registers one or more scenarios and hands
// control to the runner, which turns the binary into a complete LoadWave node:
// it can run a test standalone, act as a coordinator serving the dashboard, or
// join an existing cluster as an agent.
//
//	package main
//
//	import (
//	    "context"
//	    "net/http"
//	    "time"
//
//	    "github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
//	    "github.com/SnowyFoxStudios/LoadWave/pkg/loadwave/run"
//	)
//
//	func main() {
//	    loadwave.Register(loadwave.Scenario{
//	        Name:   "browse",
//	        Weight: 3,
//	        Run:    browse,
//	    })
//	    run.Main()
//	}
//
//	func browse(ctx context.Context, vu *loadwave.VU) error {
//	    resp, err := vu.HTTP().Get(ctx, "/api/products")
//	    if err != nil {
//	        return err
//	    }
//	    vu.Check("products ok", resp.StatusCode == http.StatusOK)
//	    vu.ThinkBetween(ctx, time.Second, 3*time.Second)
//	    return nil
//	}
//
// # Concurrency
//
// Each virtual user runs on its own goroutine and owns its VU exclusively, so
// scenario code needs no locking for anything reached through the VU. Anything
// a scenario shares between users — a package-level map, a fixture slice being
// mutated — is the scenario's own problem to synchronise.
//
// # Metrics
//
// The HTTP client records the standard metric set automatically. Scenarios can
// add their own through VU.Metrics, and should attach tags through VU.Tag
// rather than encoding values into metric names. Tag values must come from a
// small fixed set: every distinct combination becomes a time series held in
// memory on the coordinator for the length of the run.
//
// # Testing scenarios
//
// NewVU is exported so a scenario can be exercised from an ordinary Go test
// against an httptest.Server, with no coordinator, agent or worker involved.
package loadwave
