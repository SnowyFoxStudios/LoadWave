// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// planFlags are the command-line overrides for a test configuration.
//
// A configuration file is the normal way to describe a test, but a quick
// smoke check should not need one, and CI often wants to vary one number
// against a checked-in file. Both are served by the same struct: flags fill in
// for a missing file, and override one that is present.
type planFlags struct {
	url        string
	name       string
	vus        int
	duration   time.Duration
	stages     string
	iterations uint64
	rate       int
	workers    int
	gracefulSt time.Duration
	tags       []string
	scenarios  []string
	thresholds []string
	insecure   bool
	headers    []string
	timeout    time.Duration
	betweenReq string
}

// register attaches the plan flags to a command.
func (f *planFlags) register(flags *pflag.FlagSet) {
	flags.StringVarP(&f.url, "url", "u", "",
		"base URL for relative request paths")
	flags.StringVar(&f.name, "name", "",
		"name for this test, shown in the dashboard")
	flags.IntVar(&f.vus, "vus", 0,
		"number of virtual users to hold (constant-vus)")
	flags.DurationVarP(&f.duration, "duration", "d", 0,
		"how long to hold the load, e.g. 30s or 5m")
	flags.StringVar(&f.stages, "stages", "",
		"ramping profile as duration:target pairs, e.g. 30s:100,5m:100,30s:0")
	flags.Uint64Var(&f.iterations, "iterations", 0,
		"stop after this many iterations in total")
	flags.IntVar(&f.rate, "rate", 0,
		"cap iterations started per second across the whole run")
	flags.IntVarP(&f.workers, "workers", "w", 0,
		"worker processes per agent (default: one per core, less one)")
	flags.DurationVar(&f.gracefulSt, "graceful-stop", 0,
		"how long in-flight iterations may take to finish when stopping")
	flags.StringSliceVar(&f.tags, "tag", nil,
		"tag applied to every metric, as key=value (repeatable)")
	flags.StringSliceVar(&f.scenarios, "scenario", nil,
		"scenario to run, as name or name=weight (repeatable; default: all)")
	flags.StringSliceVar(&f.thresholds, "threshold", nil,
		"pass/fail assertion, e.g. http_req_duration:p95<500 (repeatable)")
	flags.BoolVar(&f.insecure, "insecure", false,
		"skip TLS certificate verification")
	flags.StringSliceVarP(&f.headers, "header", "H", nil,
		"header sent with every request, as Name: value (repeatable)")
	flags.DurationVar(&f.timeout, "timeout", 0,
		"per-request timeout")
	flags.StringVar(&f.betweenReq, "between-requests", "",
		`pause after every request, e.g. "1s" or "500ms-2s"; "0" for none (default 1s)`)
}

// load reads the configuration file if given, then applies the flags.
func (f *planFlags) load(path string) (*scenario.Config, error) {
	cfg := &scenario.Config{}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, failf("read %s: %v", path, err)
		}
		parsed, err := scenario.Parse(data)
		if err != nil {
			return nil, failf("%s: %v", path, err)
		}
		cfg = parsed
	}

	if err := f.apply(cfg); err != nil {
		return nil, err
	}

	if path == "" && cfg.Load.VUs == 0 && len(cfg.Load.Stages) == 0 {
		return nil, failf(
			"nothing to run: give a configuration file, or set --vus with --duration")
	}

	if err := cfg.Validate(); err != nil {
		return nil, failf("%v", err)
	}
	return cfg, nil
}

// apply overlays the flags onto a configuration.
func (f *planFlags) apply(cfg *scenario.Config) error {
	if f.name != "" {
		cfg.Name = f.name
	}
	if f.url != "" {
		cfg.BaseURL = f.url
	}
	if f.vus > 0 {
		cfg.Load.VUs = f.vus
	}
	if f.duration > 0 {
		cfg.Load.Duration = scenario.Duration(f.duration)
	}
	if f.iterations > 0 {
		cfg.Load.Iterations = f.iterations
	}
	if f.rate > 0 {
		cfg.Load.MaxIterationRate = f.rate
	}
	if f.workers > 0 {
		cfg.WorkersPerAgent = f.workers
	}
	if f.gracefulSt > 0 {
		cfg.Load.GracefulStop = scenario.Duration(f.gracefulSt)
	}
	if f.insecure {
		cfg.HTTP.InsecureSkipTLSVerify = true
	}
	if f.timeout > 0 {
		cfg.HTTP.Timeout = scenario.Duration(f.timeout)
	}
	if f.betweenReq != "" {
		if _, err := loadwave.ParsePause(f.betweenReq); err != nil {
			return failf("--between-requests: %v", err)
		}
		cfg.BetweenRequests = f.betweenReq
	}

	if f.stages != "" {
		stages, err := parseStages(f.stages)
		if err != nil {
			return failf("--stages: %v", err)
		}
		cfg.Load.Stages = stages
		cfg.Load.Executor = scenario.ExecutorRampingVUs
	}

	for _, raw := range f.tags {
		key, value, err := splitPair(raw, "=")
		if err != nil {
			return failf("--tag %q: %v", raw, err)
		}
		if cfg.Tags == nil {
			cfg.Tags = map[string]string{}
		}
		cfg.Tags[key] = value
	}

	for _, raw := range f.headers {
		key, value, err := splitPair(raw, ":")
		if err != nil {
			return failf("--header %q: %v", raw, err)
		}
		if cfg.HTTP.Headers == nil {
			cfg.HTTP.Headers = map[string]string{}
		}
		cfg.HTTP.Headers[key] = value
	}

	if err := f.applyScenarios(cfg); err != nil {
		return err
	}
	return f.applyThresholds(cfg)
}

// applyScenarios replaces the configuration's scenario selection.
func (f *planFlags) applyScenarios(cfg *scenario.Config) error {
	if len(f.scenarios) == 0 {
		return nil
	}

	selected := make([]scenario.ScenarioConfig, 0, len(f.scenarios))
	for _, raw := range f.scenarios {
		name, weight := raw, 1
		if key, value, ok := strings.Cut(raw, "="); ok {
			// Parsed at 32 bits, which is the width the wire format carries.
			// A weight past it is a typo, and saying so beside the flag that
			// has it beats narrowing it silently on the way to a plan.
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
			if err != nil || parsed <= 0 {
				return failf("--scenario %q: weight must be a positive 32-bit integer", raw)
			}
			name, weight = key, int(parsed)
		}
		name = strings.TrimSpace(name)

		// Preserve a matching declarative definition from the file, so
		// --scenario can narrow a configuration down to one of its scenarios
		// rather than only referring to compiled-in Go code.
		found := false
		for _, defined := range cfg.Scenarios {
			if defined.Name == name {
				defined.Weight = weight
				selected = append(selected, defined)
				found = true
				break
			}
		}
		if !found {
			selected = append(selected, scenario.ScenarioConfig{Name: name, Weight: weight})
		}
	}

	cfg.Scenarios = selected
	return nil
}

// applyThresholds appends command-line thresholds.
func (f *planFlags) applyThresholds(cfg *scenario.Config) error {
	for _, raw := range f.thresholds {
		threshold, err := parseThreshold(raw)
		if err != nil {
			return failf("--threshold %q: %v", raw, err)
		}
		cfg.Thresholds = append(cfg.Thresholds, threshold)
	}
	return nil
}

// parseStages reads "30s:100,5m:100,30s:0".
func parseStages(spec string) ([]scenario.StageConfig, error) {
	parts := strings.Split(spec, ",")
	stages := make([]scenario.StageConfig, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		rawDuration, rawTarget, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("stage %q must be written duration:target, e.g. 30s:100", part)
		}

		duration, err := time.ParseDuration(strings.TrimSpace(rawDuration))
		if err != nil {
			return nil, fmt.Errorf("stage %q: invalid duration: %w", part, err)
		}
		// As above: 32 bits, because that is what a stage target is on the
		// wire. Five billion VUs is a slipped keystroke, not a request.
		target, err := strconv.ParseInt(strings.TrimSpace(rawTarget), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("stage %q: target must be a whole 32-bit number", part)
		}
		if target < 0 {
			return nil, fmt.Errorf("stage %q: target cannot be negative", part)
		}

		stages = append(stages, scenario.StageConfig{
			Duration: scenario.Duration(duration),
			Target:   int(target),
		})
	}

	if len(stages) == 0 {
		return nil, errors.New("no stages given")
	}
	return stages, nil
}

// thresholdOperators are tried longest-first so that "<=" is not mistaken for
// "<" with a stray equals sign in the value.
var thresholdOperators = []string{"<=", ">=", "<", ">"}

// parseThreshold reads "http_req_duration:p95<500" or "http_req_failed:rate<0.01".
func parseThreshold(spec string) (scenario.Threshold, error) {
	metric, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return scenario.Threshold{}, errors.New("expected metric:stat<value, e.g. http_req_duration:p95<500")
	}
	metric = strings.TrimSpace(metric)
	if metric == "" {
		return scenario.Threshold{}, errors.New("metric name is empty")
	}

	for _, op := range thresholdOperators {
		stat, rawValue, found := strings.Cut(rest, op)
		if !found {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
		if err != nil {
			return scenario.Threshold{}, fmt.Errorf("value %q is not a number", rawValue)
		}
		return scenario.Threshold{
			Metric: metric,
			Stat:   strings.TrimSpace(stat),
			Op:     op,
			Value:  value,
		}, nil
	}

	return scenario.Threshold{}, errors.New("no comparison found; use one of <, <=, > or >=")
}

// splitPair splits "key<sep>value", trimming both halves.
func splitPair(raw, sep string) (string, string, error) {
	key, value, ok := strings.Cut(raw, sep)
	if !ok {
		return "", "", fmt.Errorf("expected key%svalue", sep)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", errors.New("key is empty")
	}
	return key, strings.TrimSpace(value), nil
}
