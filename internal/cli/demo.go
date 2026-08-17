// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
)

// newDemoCommand runs LoadWave against a target it starts itself.
//
// The point is to have something to look at within seconds of installing,
// with no service to stand up and no configuration to write. It is also the
// quickest way to check that a build works end to end.
func newDemoCommand(opts *options) *cobra.Command {
	ro := &runOptions{ui: true}
	var (
		duration time.Duration
		vus      int
		headless bool
	)

	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Run a self-contained demo against a built-in target",
		Long: `Run a demo test with the dashboard, against a target LoadWave starts itself.

Nothing to configure and nothing to install: a small HTTP server is started in
this process with a few endpoints of differing speed and a sprinkling of
errors, and a two-scenario test is run against it. It is the fastest way to see
what the dashboard shows, and a good end-to-end check of a fresh build.

The demo server is a toy. Its numbers say nothing about your hardware.`,
		Example: `  loadwave demo                       # dashboard on :8088, runs for 10 minutes
  loadwave demo --duration 2m --vus 50
  loadwave demo --headless            # no dashboard, just the report`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, shutdown, err := startDemoTarget(cmd.Context())
			if err != nil {
				return err
			}
			defer shutdown()

			cfg := demoConfig(target, vus, duration)
			if err := verifyScenarios(cfg, opts); err != nil {
				return err
			}

			ro.ui = !headless
			fmt.Fprintf(cmd.OutOrStdout(),
				"Demo target listening on %s — a toy server, not a benchmark.\n\n", target)

			return ro.executeConfig(cmd, opts, cfg)
		},
	}

	cmd.Flags().DurationVarP(&duration, "duration", "d", 10*time.Minute,
		"how long to run; long enough by default to click around the dashboard")
	cmd.Flags().IntVar(&vus, "vus", 25, "peak virtual users")
	cmd.Flags().BoolVar(&headless, "headless", false, "skip the dashboard and just print the report")
	cmd.Flags().StringVar(&ro.uiAddr, "ui-addr", ":8088", "dashboard listen address")
	cmd.Flags().BoolVarP(&ro.quiet, "quiet", "q", false, "suppress the live progress line")
	cmd.Flags().StringVar(&ro.reportPath, "report", "",
		"write a self-contained HTML report, with charts, to this file")

	// The demo's process belongs to whoever is looking at the dashboard, so
	// powering it off from there is exactly right.
	// The rest of the run machinery needs these, but the demo fixes them.
	ro.agents = 1
	ro.waitAgents = 30 * time.Second
	ro.resolution = time.Second

	return cmd
}

// demoConfig builds the test the demo runs.
func demoConfig(baseURL string, vus int, duration time.Duration) *scenario.Config {
	// A third of the time ramping up, most of it holding, a short ramp down —
	// enough shape that the charts show something other than a flat line.
	rampUp := max(5*time.Second, duration/10)
	rampDown := rampUp
	hold := duration - rampUp - rampDown
	if hold < time.Second {
		hold = time.Second
	}

	return &scenario.Config{
		Name:    "loadwave-demo",
		BaseURL: baseURL,
		Load: scenario.LoadConfig{
			Executor: scenario.ExecutorRampingVUs,
			Stages: []scenario.StageConfig{
				{Duration: scenario.Duration(rampUp), Target: vus},
				{Duration: scenario.Duration(hold), Target: vus},
				{Duration: scenario.Duration(rampDown), Target: 0},
			},
			GracefulStop: scenario.Duration(10 * time.Second),
		},
		WorkersPerAgent: 2,
		// Deliberately faster than the one-second default. The demo exists to
		// show the dashboard moving, and at the default pace twenty-five
		// virtual users produce about twenty requests a second — correct, but
		// a dull chart. A real test should leave the default alone.
		BetweenRequests: "100ms-400ms",
		Tags:            map[string]string{"demo": "true"},
		Thresholds: []scenario.Threshold{
			{Metric: "http_req_duration", Stat: "p95", Op: "<", Value: 500},
			{Metric: "http_req_failed", Stat: "rate", Op: "<", Value: 0.1},
			{Metric: "checks", Stat: "rate", Op: ">", Value: 0.9},
		},
		Scenarios: []scenario.ScenarioConfig{
			{
				Name:        "browse",
				Weight:      3,
				Description: "Lists products, then opens one of them.",
				Steps: []scenario.StepConfig{
					{
						Name:    "list products",
						Get:     "/api/products",
						Expect:  []int{http.StatusOK},
						Capture: map[string]string{"productId": "items.0.id"},
					},
					{Think: "200ms-900ms"},
					{
						Name:   "view product",
						Get:    "/api/products/${productId}",
						Expect: []int{http.StatusOK},
					},
				},
			},
			{
				Name:        "checkout",
				Weight:      1,
				Description: "The slow path, with a small error rate.",
				Steps: []scenario.StepConfig{
					{
						Name:   "create order",
						Post:   "/api/orders",
						JSON:   map[string]any{"productId": 4711, "quantity": 1},
						Expect: []int{http.StatusCreated},
					},
					{Think: "500ms-2s"},
				},
			},
		},
	}
}

// startDemoTarget runs the toy server and returns its base URL.
func startDemoTarget(ctx context.Context) (string, func(), error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, failf("start the demo target: %v", err)
	}

	server := &http.Server{
		Handler:           demoHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()

	shutdown := func() { _ = server.Close() }
	return "http://" + listener.Addr().String(), shutdown, nil
}

// demoHandler serves the endpoints the demo test exercises.
//
// The latencies and the error rate are invented, and chosen so the dashboard
// has something interesting to draw: a fast endpoint, a slower one, and a
// tail that occasionally fails.
func demoHandler() http.Handler {
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}

	// Fast and reliable.
	mux.HandleFunc("GET /api/products", func(w http.ResponseWriter, _ *http.Request) {
		sleepAround(12 * time.Millisecond)
		items := make([]map[string]any, 0, 8)
		for i := range 8 {
			items = append(items, map[string]any{
				"id":    4711 + i,
				"sku":   fmt.Sprintf("SKU-%04d", 4711+i),
				"price": 19.99 + float64(i),
			})
		}
		write(w, map[string]any{"items": items, "total": len(items)})
	})

	// Slower, and fails about one time in forty.
	mux.HandleFunc("GET /api/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		sleepAround(35 * time.Millisecond)
		if rand.IntN(40) == 0 {
			http.Error(w, "upstream unavailable", http.StatusInternalServerError)
			return
		}
		id, _ := strconv.Atoi(r.PathValue("id"))
		write(w, map[string]any{"id": id, "name": "Demo widget", "inStock": true})
	})

	// The slowest path, with a long tail and the highest error rate, so the
	// p99 chart has something to show.
	mux.HandleFunc("POST /api/orders", func(w http.ResponseWriter, _ *http.Request) {
		sleepAround(70 * time.Millisecond)
		if rand.IntN(25) == 0 {
			sleepAround(400 * time.Millisecond) // an occasional straggler
		}
		if rand.IntN(30) == 0 {
			http.Error(w, "payment declined", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
		write(w, map[string]any{"orderId": rand.IntN(1_000_000), "total": 42.5})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"service": "loadwave-demo", "status": "ok"})
	})

	return mux
}

// sleepAround pauses for roughly d, with enough spread that the percentile
// charts separate instead of drawing four identical lines.
func sleepAround(d time.Duration) {
	jitter := time.Duration(rand.Int64N(int64(d)))
	time.Sleep(d/2 + jitter)
}

// demoBanner is printed once the dashboard is up.
func demoBanner(url string) string {
	var b strings.Builder
	b.WriteString("\n  LoadWave demo running.\n\n")
	fmt.Fprintf(&b, "    dashboard   %s\n", url)
	b.WriteString("    stop        Ctrl-C (the run stops gracefully and reports)\n\n")
	return b.String()
}
