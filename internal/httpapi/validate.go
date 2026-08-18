// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"fmt"
	"net/http"

	"github.com/SnowyFoxStudios/LoadWave/internal/engine"
	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// ValidateResult is the answer to "would this configuration run?".
type ValidateResult struct {
	Valid bool `json:"valid"`

	// Error is the parser's own message, verbatim, when the configuration is
	// rejected. It carries the offending line and field, which is far more
	// use to somebody editing a form than a reworded summary would be.
	Error string `json:"error,omitempty"`

	Summary *ValidateSummary `json:"summary,omitempty"`
}

// ValidateSummary describes what the configuration would actually do.
//
// This is the same information `loadwave validate` prints. Showing it back is
// how a builder proves it understood the form the same way the runner will:
// a profile that reads "30s to 100 VUs, then 5m0s to 100 VUs" is a much
// stronger confirmation than a green tick.
type ValidateSummary struct {
	Name            string             `json:"name"`
	BaseURL         string             `json:"baseURL,omitempty"`
	Profile         string             `json:"profile"`
	PeakVUs         int                `json:"peakVUs"`
	DurationSeconds float64            `json:"durationSeconds"`
	Iterations      uint64             `json:"iterations,omitempty"`
	IterationRate   int                `json:"iterationRate,omitempty"`
	WorkersPerAgent int                `json:"workersPerAgent,omitempty"`
	BetweenRequests string             `json:"betweenRequests"`
	PacingDefaulted bool               `json:"pacingDefaulted"`
	Scenarios       []ValidateScenario `json:"scenarios"`
	Thresholds      []string           `json:"thresholds,omitempty"`
}

// ValidateScenario is one scenario as the runner would see it.
type ValidateScenario struct {
	Name        string `json:"name"`
	Weight      int    `json:"weight"`
	Description string `json:"description,omitempty"`
	// Steps is zero for a scenario compiled into the binary.
	Steps int `json:"steps"`
	// Source is "yaml" for a scenario defined by steps here, "go" for one
	// compiled in.
	Source string `json:"source"`
}

// handleValidate checks a configuration without running it.
//
// It answers 200 whether or not the configuration is valid: an invalid
// configuration is a successful answer to the question being asked, and a
// builder that revalidates on every keystroke should not be reading failure
// out of the status code.
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusOK, ValidateResult{Error: "the configuration is empty"})
		return
	}

	writeJSON(w, http.StatusOK, s.validateConfig(body))
}

// validateConfig runs a configuration through exactly the path a real run
// would take, short of starting one.
func (s *Server) validateConfig(body []byte) ValidateResult {
	cfg, err := scenario.Parse(body)
	if err != nil {
		return ValidateResult{Error: err.Error()}
	}

	// A fresh registry per check, seeded with whatever this binary has
	// compiled in, so that validating twice cannot collide on a name.
	registry := loadwave.NewRegistry()
	if s.cfg.Registry != nil {
		registry = s.cfg.Registry.Clone()
	}
	compiledIn := map[string]bool{}
	for _, name := range registry.Names() {
		compiledIn[name] = true
	}

	if err := cfg.BuildScenarios(registry); err != nil {
		return ValidateResult{Error: err.Error()}
	}
	if registry.Len() == 0 {
		return ValidateResult{Error: "no scenarios to run: add one, or set a base URL for a single-request smoke test"}
	}

	plan, err := cfg.Plan()
	if err != nil {
		return ValidateResult{Error: err.Error()}
	}
	executor, err := engine.NewExecutor(plan.GetLoad())
	if err != nil {
		return ValidateResult{Error: err.Error()}
	}

	pacing := cfg.BetweenRequestsPause()
	summary := &ValidateSummary{
		Name:            cfg.Name,
		BaseURL:         cfg.BaseURL,
		Profile:         executor.Describe(),
		PeakVUs:         executor.Peak(),
		DurationSeconds: executor.Duration().Seconds(),
		Iterations:      plan.GetLoad().GetIterations(),
		IterationRate:   int(plan.GetLoad().GetMaxIterationsPerSecond()),
		WorkersPerAgent: cfg.WorkersPerAgent,
		BetweenRequests: pacing.String(),
		PacingDefaulted: cfg.BetweenRequests == "",
	}

	steps := map[string]int{}
	described := map[string]string{}
	for _, declared := range cfg.Scenarios {
		steps[declared.Name] = len(declared.Steps)
		described[declared.Name] = declared.Description
	}

	for _, name := range registry.Names() {
		built, _ := registry.Lookup(name)
		source := "yaml"
		if compiledIn[name] {
			source = "go"
		}
		summary.Scenarios = append(summary.Scenarios, ValidateScenario{
			Name:        name,
			Weight:      built.EffectiveWeight(),
			Description: described[name],
			Steps:       steps[name],
			Source:      source,
		})
	}

	for _, t := range cfg.Thresholds {
		summary.Thresholds = append(summary.Thresholds,
			fmt.Sprintf("%s %s %s %g", t.Metric, t.Stat, t.Op, t.Value))
	}

	return ValidateResult{Valid: true, Summary: summary}
}
