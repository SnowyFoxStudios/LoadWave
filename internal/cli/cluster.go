// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/agent"
	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
	"github.com/SnowyFoxStudios/LoadWave/internal/httpapi"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// clusterConfig describes an in-process LoadWave cluster.
type clusterConfig struct {
	// CoordinatorAddr is where agents connect. "127.0.0.1:0" keeps a
	// single-machine run off the network entirely.
	CoordinatorAddr string

	// LocalAgent starts an agent in this process, which then spawns the
	// worker processes that generate the load.
	LocalAgent bool
	Workers    int
	MaxVUs     int

	// Dashboard serves the web UI and REST API.
	Dashboard      bool
	DashboardAddr  string
	AllowedOrigins []string
	ReadOnly       bool

	// AllowShutdown exposes the dashboard's power-off control.
	AllowShutdown bool

	Resolution time.Duration
	Window     time.Duration

	// Registry is the scenarios compiled into this binary, so the dashboard's
	// validation can tell a missing Go scenario from a misspelled one.
	Registry *loadwave.Registry

	Logger *slog.Logger
}

// cluster owns the goroutines running an in-process coordinator, agent and
// dashboard.
type cluster struct {
	coordinator *coordinator.Coordinator
	dashboard   *httpapi.Server
	agent       *agent.Agent

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// errs collects the first fatal error from any component.
	errs chan error

	// off is closed when the dashboard asks the process to shut down.
	off     chan struct{}
	offOnce sync.Once
	reason  string
}

// startCluster brings up the requested components and returns once the
// coordinator is accepting connections.
func startCluster(ctx context.Context, cfg clusterConfig) (*cluster, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CoordinatorAddr == "" {
		cfg.CoordinatorAddr = "127.0.0.1:0"
	}

	coord, err := coordinator.New(coordinator.Config{
		ListenAddr: cfg.CoordinatorAddr,
		Logger:     cfg.Logger,
		Store: metrics.StoreConfig{
			Resolution: cfg.Resolution,
			Window:     cfg.Window,
		},
	})
	if err != nil {
		return nil, failf("create coordinator: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	c := &cluster{
		coordinator: coord,
		cancel:      cancel,
		errs:        make(chan error, 3),
		off:         make(chan struct{}),
	}

	c.spawn(func() error { return coord.Run(runCtx) })

	// Blocks until the listener is bound, so everything below can rely on
	// having a real address rather than the ":0" that was asked for.
	coordAddr := coord.Addr()

	if cfg.Dashboard {
		var onShutdown func(string)
		if cfg.AllowShutdown {
			onShutdown = c.powerOff
		}

		dashboard, err := httpapi.New(httpapi.Config{
			Addr:           cfg.DashboardAddr,
			Coordinator:    coord,
			Logger:         cfg.Logger,
			AllowedOrigins: cfg.AllowedOrigins,
			ReadOnly:       cfg.ReadOnly,
			OnShutdown:     onShutdown,
			Registry:       cfg.Registry,
		})
		if err != nil {
			cancel()
			return nil, failf("create dashboard: %v", err)
		}
		c.dashboard = dashboard
		c.spawn(func() error { return dashboard.Run(runCtx) })
	}

	if cfg.LocalAgent {
		local, err := agent.New(agent.Config{
			NodeID:            "local",
			CoordinatorTarget: coordAddr,
			Workers:           cfg.Workers,
			MaxVUs:            cfg.MaxVUs,
			Logger:            cfg.Logger,
			Labels:            map[string]string{"local": "true"},
		})
		if err != nil {
			cancel()
			return nil, failf("create local agent: %v", err)
		}
		c.agent = local
		c.spawn(func() error { return local.Run(runCtx) })
	}

	return c, nil
}

// spawn runs a component, recording the first error it reports.
func (c *cluster) spawn(fn func() error) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := fn(); err != nil {
			select {
			case c.errs <- err:
			default:
			}
		}
	}()
}

// awaitAgents blocks until at least `want` agents have joined.
func (c *cluster) awaitAgents(ctx context.Context, want int, timeout time.Duration) error {
	if want <= 0 {
		return nil
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if len(c.coordinator.Agents()) >= want {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-c.errs:
			return err
		case <-deadline:
			return failf("no agent connected within %s", timeout)
		case <-ticker.C:
		}
	}
}

// powerOff signals that the process should exit. Safe to call more than once.
func (c *cluster) powerOff(reason string) {
	c.offOnce.Do(func() {
		c.reason = reason
		close(c.off)
	})
}

// poweredOff is closed when the dashboard asks the process to shut down.
func (c *cluster) poweredOff() <-chan struct{} { return c.off }

// err returns the first component error, if any has been reported.
func (c *cluster) err() error {
	select {
	case err := <-c.errs:
		return err
	default:
		return nil
	}
}

// stop shuts the cluster down and waits for its goroutines.
func (c *cluster) stop() {
	c.cancel()

	// Bounded, because a wedged worker process must not leave the CLI hanging
	// after it has already printed the report.
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
}

// shutdownGrace bounds how long the cluster is given to unwind.
const shutdownGrace = 45 * time.Second

// dashboardURL returns the browsable dashboard address, or an empty string.
func (c *cluster) dashboardURL() string {
	if c.dashboard == nil {
		return ""
	}
	return c.dashboard.URL()
}
