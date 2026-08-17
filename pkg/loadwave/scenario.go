// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// scenarioNamePattern constrains scenario names to something safe to use as a
// metric label, a CLI argument and a URL path segment without escaping.
var scenarioNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Scenario is a named unit of simulated user behaviour.
//
// Run is the only required field. It is called once per iteration, by every
// virtual user assigned to the scenario, until the run's load profile says to
// stop. It should represent one pass of whatever a real user would do —
// browsing a page, completing a checkout — and it should return an error when
// that pass did not succeed, since that is what drives the failure rate.
//
// The lifecycle hooks fire in this order:
//
//	Setup           once per worker process, before any VU starts
//	  OnVUStart     once per virtual user
//	    Run         repeatedly, until the profile ends
//	  OnVUStop      once per virtual user
//	Teardown        once per worker process, after all VUs have stopped
//
// Setup and Teardown run once per worker process, not once per run. A run
// spread over four processes calls Setup four times. Anything that must happen
// exactly once for the whole run — seeding a database, say — belongs outside
// the scenario.
type Scenario struct {
	// Name identifies the scenario in metrics, the CLI and the dashboard.
	// Must match [a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}.
	Name string

	// Weight is this scenario's relative share of iterations when a run
	// executes several scenarios at once. A scenario with weight 3 runs three
	// times as often as one with weight 1. Zero is treated as 1.
	Weight int

	// Description is shown in the dashboard when picking scenarios.
	Description string

	// Setup prepares process-wide state, such as authenticating a service
	// account whose token every VU will share. An error here fails the run on
	// this worker before any load is generated.
	Setup func(ctx context.Context) error

	// Teardown releases whatever Setup acquired. It is called even when the
	// run failed, and is given the plan's graceful-stop budget to finish.
	Teardown func(ctx context.Context) error

	// OnVUStart initialises per-user state — logging a distinct user in,
	// picking a row from a fixture file. Store it on the VU with VU.SetState.
	OnVUStart func(ctx context.Context, vu *VU) error

	// OnVUStop cleans up per-user state.
	OnVUStop func(ctx context.Context, vu *VU) error

	// Run executes one iteration. Returning an error marks the iteration
	// failed and increments the error metrics; it does not stop the run.
	Run func(ctx context.Context, vu *VU) error
}

// Validate reports whether the scenario is well formed.
func (s Scenario) Validate() error {
	if !scenarioNamePattern.MatchString(s.Name) {
		return fmt.Errorf("scenario name %q must match %s", s.Name, scenarioNamePattern)
	}
	if s.Run == nil {
		return fmt.Errorf("scenario %q has no Run function", s.Name)
	}
	if s.Weight < 0 {
		return fmt.Errorf("scenario %q has negative weight %d", s.Name, s.Weight)
	}
	return nil
}

// EffectiveWeight resolves the zero value to 1.
func (s Scenario) EffectiveWeight() int {
	if s.Weight <= 0 {
		return 1
	}
	return s.Weight
}

// Registry holds the scenarios a binary knows how to execute.
//
// Registration happens at startup, before any run begins, but the registry is
// still guarded by a mutex: scenarios are commonly registered from init
// functions across several files, and a data race there would be a miserable
// thing to debug.
type Registry struct {
	mu        sync.RWMutex
	scenarios map[string]Scenario
}

// NewRegistry returns an empty registry. Most programs use the package-level
// Default registry instead of constructing their own; an explicit registry is
// useful in tests, where global state between cases is a hazard.
func NewRegistry() *Registry {
	return &Registry{scenarios: make(map[string]Scenario)}
}

// Register adds a scenario, returning an error if it is invalid or if the name
// is already taken.
func (r *Registry) Register(s Scenario) error {
	if err := s.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.scenarios[s.Name]; exists {
		return fmt.Errorf("scenario %q is already registered", s.Name)
	}
	r.scenarios[s.Name] = s
	return nil
}

// MustRegister is Register, panicking on error.
//
// This is the right call from an init function or from main, where a
// duplicate or malformed scenario is a bug that should stop the program
// immediately rather than surface as a confusing empty run much later.
func (r *Registry) MustRegister(s Scenario) {
	if err := r.Register(s); err != nil {
		panic("loadwave: " + err.Error())
	}
}

// Lookup returns a scenario by name.
func (r *Registry) Lookup(name string) (Scenario, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.scenarios[name]
	return s, ok
}

// Names lists every registered scenario, sorted, so that CLI output and run
// sharding are deterministic.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.scenarios))
	for name := range r.scenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Clone returns an independent registry holding the same scenarios.
//
// Workers build one of these per run and add the run's declarative scenarios
// to the copy, so that a configuration's YAML-defined scenarios do not leak
// into the next run started against the same process.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clone := &Registry{scenarios: make(map[string]Scenario, len(r.scenarios))}
	for name, s := range r.scenarios {
		clone.scenarios[name] = s
	}
	return clone
}

// Len reports how many scenarios are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.scenarios)
}

// Default is the registry used by the package-level Register function and by
// the runner when a binary does not supply its own.
var Default = NewRegistry()

// Register adds a scenario to the default registry, panicking on error.
//
// This is the entry point for the common case:
//
//	func init() {
//	    loadwave.Register(loadwave.Scenario{
//	        Name: "browse",
//	        Run:  browse,
//	    })
//	}
func Register(s Scenario) { Default.MustRegister(s) }
