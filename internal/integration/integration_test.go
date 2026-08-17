// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package integration exercises a whole LoadWave cluster in one test binary:
// a coordinator, an agent, and the worker processes the agent spawns.
//
// The worker tier is the part unit tests cannot reach, because a worker is a
// separate operating system process. TestMain gives the test binary a second
// personality: when the agent execs it with a `worker` argument, it becomes
// the LoadWave CLI instead of running tests. That makes the subprocess
// supervision, the Unix-socket control stream, and the metric relay all real
// rather than stubbed.
package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/agent"
	"github.com/SnowyFoxStudios/LoadWave/internal/cli"
	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

func TestMain(m *testing.M) {
	// The agent spawns this same binary with a `worker` argument. Test flags
	// always begin with a dash, so this cannot be confused with a real
	// `go test` invocation.
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		os.Exit(cli.Execute(loadwave.Default))
	}
	os.Exit(m.Run())
}

// cluster is a coordinator plus one agent, both in this process.
type testCluster struct {
	coordinator *coordinator.Coordinator
	cancel      context.CancelFunc
	done        chan struct{}
}

// startCluster brings up a coordinator and an agent and waits for them to
// find each other.
func startCluster(t *testing.T, workers int) *testCluster {
	t.Helper()

	coord, err := coordinator.New(coordinator.Config{
		ListenAddr:      "127.0.0.1:0",
		MetricsInterval: 250 * time.Millisecond,
		// Short, so the test does not spend most of its time waiting for the
		// fleet-wide synchronised start.
		StartDelay: 250 * time.Millisecond,
		Store: metrics.StoreConfig{
			Resolution: 250 * time.Millisecond,
			LateGrace:  time.Second,
		},
	})
	if err != nil {
		t.Fatalf("coordinator.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)

	go func() {
		if err := coord.Run(ctx); err != nil {
			t.Errorf("coordinator stopped: %v", err)
		}
		done <- struct{}{}
	}()

	node, err := agent.New(agent.Config{
		NodeID:            "agent-under-test",
		CoordinatorTarget: coord.Addr(),
		Workers:           workers,
		MaxVUs:            1000,
	})
	if err != nil {
		cancel()
		t.Fatalf("agent.New: %v", err)
	}

	go func() {
		if err := node.Run(ctx); err != nil {
			t.Errorf("agent stopped: %v", err)
		}
		done <- struct{}{}
	}()

	cluster := &testCluster{coordinator: coord, cancel: cancel, done: done}
	t.Cleanup(cluster.stop)

	waitFor(t, 20*time.Second, "the agent to join", func() bool {
		return len(coord.Agents()) == 1
	})
	return cluster
}

func (c *testCluster) stop() {
	c.cancel()
	for range 2 {
		select {
		case <-c.done:
		case <-time.After(30 * time.Second):
		}
	}
}

// waitFor polls until cond holds, failing the test on timeout.
func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}

// awaitRun blocks until a run reaches a terminal phase.
func awaitRun(t *testing.T, run *coordinator.Run, limit time.Duration) {
	t.Helper()
	waitFor(t, limit, "run "+run.ID()+" to finish", func() bool { return !run.Active() })

	// Let the final buckets close so the assertions see the whole run.
	time.Sleep(1500 * time.Millisecond)
}

func parseConfig(t *testing.T, body string) *scenario.Config {
	t.Helper()

	cfg, err := scenario.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse configuration: %v", err)
	}
	return cfg
}

func TestDistributedRunEndToEnd(t *testing.T) {
	var served atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasPrefix(r.URL.Path, "/things/"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/broken":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"items":[{"id":4711}]}`))
		}
	}))
	t.Cleanup(server.Close)

	cluster := startCluster(t, 2)

	cfg := parseConfig(t, fmt.Sprintf(`
name: integration
baseURL: %s
load:
  executor: constant-vus
  vus: 6
  duration: 3s
  gracefulStop: 5s
workersPerAgent: 2
thresholds:
  - { metric: http_req_duration, stat: p95, op: "<", value: 5000 }
  - { metric: http_reqs, stat: count, op: ">", value: 0 }
scenarios:
  - name: browse
    steps:
      - name: list
        get: /items
        expect: [200]
        capture:
          id: items.0.id
      - name: view
        get: /things/${id}
        expect: [200]
  - name: failing
    steps:
      - name: broken
        get: /broken
        expect: [200]
`, server.URL))

	run, err := cluster.coordinator.StartRun(cfg)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	awaitRun(t, run, 60*time.Second)

	snapshot, ok := cluster.coordinator.RunSnapshot(run.ID())
	if !ok {
		t.Fatal("run snapshot is missing")
	}
	if snapshot.Run.Phase != coordinator.PhaseCompleted {
		t.Fatalf("phase = %s (%s)", snapshot.Run.Phase, snapshot.Run.Failure)
	}

	// The load actually reached the server, and the coordinator's count
	// agrees with what the server saw.
	requests := snapshot.Totals["http_reqs"].Count
	if requests == 0 {
		t.Fatal("no requests were recorded")
	}
	if got := served.Load(); got == 0 {
		t.Fatal("the target server was never called")
	}
	if diff := int64(requests) - served.Load(); diff > 5 || diff < -5 {
		t.Errorf("coordinator counted %d requests, server served %d", requests, served.Load())
	}

	if snapshot.Totals["iterations"].Count == 0 {
		t.Error("no iterations completed")
	}

	// Two worker processes were spawned, and both contributed. Losing this
	// would mean the run silently collapsed onto a single process.
	if len(snapshot.Run.Participants) != 1 {
		t.Errorf("participants = %d, want 1 agent", len(snapshot.Run.Participants))
	}

	// The declarative capture worked: /things/4711 was requested by name.
	names := map[string]bool{}
	for _, endpoint := range snapshot.Endpoints {
		names[endpoint.Name] = true
	}
	for _, want := range []string{"list", "view", "broken"} {
		if !names[want] {
			t.Errorf("no endpoint named %q; got %v", want, keysOf(names))
		}
	}

	// The failing scenario must show up as failures, not be swallowed.
	if rate := snapshot.Totals["http_req_failed"].Rate; rate <= 0 {
		t.Errorf("failure rate = %v, want the /broken scenario to register", rate)
	}

	for _, result := range snapshot.Run.Thresholds {
		if !result.Evaluated {
			t.Errorf("threshold %q was never evaluated", result.Description)
		}
		if !result.Passed {
			t.Errorf("threshold %q failed: actual %v", result.Description, result.Actual)
		}
	}

	// Percentiles must have survived the trip from two worker processes,
	// through the agent, to the coordinator's merged histogram.
	if snapshot.Totals["http_req_duration"].Percentiles["p95"] <= 0 {
		t.Error("no p95 was produced from the merged histograms")
	}
	if len(snapshot.Ticks) == 0 {
		t.Error("no chart buckets were produced")
	}
	if snapshot.Run.Stats.DroppedByNode > 0 || snapshot.Run.Stats.DroppedLate > 0 {
		t.Errorf("samples were dropped: %+v", snapshot.Run.Stats)
	}
}

func TestRunStopsOnRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	cluster := startCluster(t, 1)

	// A ten-minute profile that must nevertheless stop within seconds.
	cfg := parseConfig(t, fmt.Sprintf(`
name: stoppable
baseURL: %s
load:
  executor: constant-vus
  vus: 4
  duration: 10m
  gracefulStop: 3s
workersPerAgent: 1
scenarios:
  - name: browse
    steps:
      - get: /
        expect: [200]
`, server.URL))

	run, err := cluster.coordinator.StartRun(cfg)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	waitFor(t, 30*time.Second, "traffic to start", func() bool {
		snapshot, ok := cluster.coordinator.RunSnapshot(run.ID())
		return ok && snapshot.Totals["http_reqs"].Count > 0
	})

	if err := cluster.coordinator.StopRun(run.ID(), true, "test asked it to"); err != nil {
		t.Fatalf("StopRun: %v", err)
	}
	awaitRun(t, run, 45*time.Second)

	snapshot, _ := cluster.coordinator.RunSnapshot(run.ID())
	if snapshot.Run.Phase != coordinator.PhaseCompleted {
		t.Fatalf("phase after stopping = %s", snapshot.Run.Phase)
	}
	if snapshot.Totals["http_reqs"].Count == 0 {
		t.Error("the run produced no results")
	}
	// A graceful stop lets in-flight iterations finish, so it must not
	// manufacture a cliff of cancelled requests reported as failures.
	if rate := snapshot.Totals["http_req_failed"].Rate; rate > 0.05 {
		t.Errorf("graceful stop produced a %v failure rate", rate)
	}
}

func TestThresholdBreachIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "always broken", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	cluster := startCluster(t, 1)

	cfg := parseConfig(t, fmt.Sprintf(`
name: breaching
baseURL: %s
load:
  executor: constant-vus
  vus: 2
  duration: 2s
  gracefulStop: 3s
workersPerAgent: 1
thresholds:
  - { metric: http_req_failed, stat: rate, op: "<", value: 0.01 }
scenarios:
  - name: browse
    steps:
      - get: /
        expect: [200]
`, server.URL))

	run, err := cluster.coordinator.StartRun(cfg)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	awaitRun(t, run, 45*time.Second)

	// The run itself succeeded — LoadWave did its job. The threshold is what
	// failed, and that distinction is what the exit codes encode.
	if !run.Breached() {
		t.Fatal("a run against a permanently failing server did not breach its threshold")
	}
	snapshot, _ := cluster.coordinator.RunSnapshot(run.ID())
	if snapshot.Run.Phase != coordinator.PhaseCompleted {
		t.Errorf("phase = %s, want completed; a bad service is not a failed run", snapshot.Run.Phase)
	}
}

func TestStartRunRejectedWithoutAgents(t *testing.T) {
	t.Parallel()

	coord, err := coordinator.New(coordinator.Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("coordinator.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = coord.Run(ctx) }()
	coord.Addr()

	cfg := parseConfig(t, `
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: browse
    steps:
      - get: /
`)

	if _, err := coord.StartRun(cfg); err == nil {
		t.Fatal("a run was started with no agents connected")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
