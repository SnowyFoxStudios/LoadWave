// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package scenario reads LoadWave's YAML configuration and turns it into the
// two things the rest of the system needs: a wire-format test plan, and — for
// tests written declaratively rather than in Go — a set of runnable scenarios.
package scenario

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"google.golang.org/protobuf/types/known/durationpb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// Duration is a time.Duration that reads from YAML as "30s" or "5m".
type Duration time.Duration

// UnmarshalYAML implements yaml.BytesUnmarshaler.
func (d *Duration) UnmarshalYAML(data []byte) error {
	var text string
	if err := yaml.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\": %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		*d = 0
		return nil
	}

	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML implements yaml.InterfaceMarshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is a complete LoadWave test file.
type Config struct {
	// Name identifies the test in the dashboard and in result files.
	Name string `yaml:"name,omitempty"`

	// BaseURL is prefixed to every relative request path.
	BaseURL string `yaml:"baseURL,omitempty"`

	Load       LoadConfig        `yaml:"load"`
	HTTP       HTTPConfig        `yaml:"http"`
	Tags       map[string]string `yaml:"tags,omitempty"`
	Thresholds []Threshold       `yaml:"thresholds,omitempty"`

	// WorkersPerAgent is how many worker processes each agent should spawn.
	// Zero lets each agent decide from its own core count.
	WorkersPerAgent int `yaml:"workersPerAgent,omitempty"`

	// BetweenRequests pauses after every request, whatever its outcome: a
	// duration such as "1s", or a range such as "500ms-2s".
	//
	// This is the run's pacing floor, and it is why a scenario with no think
	// time of its own does not loop as fast as the network allows — or, when
	// its request fails instantly, as fast as the CPU allows. Empty applies
	// one second. Set it to "0" for a throughput test, where flat out is the
	// point.
	//
	// Individual steps override it, including down to none.
	BetweenRequests string `yaml:"betweenRequests,omitempty"`

	// Scenarios lists what to run. An entry with steps is a declarative
	// scenario defined here; an entry with only a name and weight refers to
	// a scenario compiled into the binary through the Go SDK. Leaving the
	// list empty runs every scenario the binary has registered.
	Scenarios []ScenarioConfig `yaml:"scenarios,omitempty"`
}

// LoadConfig describes the shape of the load over time.
type LoadConfig struct {
	// Executor is "constant-vus" or "ramping-vus". Empty defaults to
	// constant-vus.
	Executor string `yaml:"executor"`

	VUs      int      `yaml:"vus"`
	Duration Duration `yaml:"duration"`

	Stages []StageConfig `yaml:"stages,omitempty"`

	// MaxIterationRate caps iterations started per second across the whole
	// run. Zero leaves the VU count as the only control.
	MaxIterationRate int `yaml:"maxIterationRate"`

	// Iterations stops the run after this many iterations in total.
	Iterations uint64 `yaml:"iterations"`

	// GracefulStop is how long in-flight iterations get to finish.
	GracefulStop Duration `yaml:"gracefulStop"`
}

// StageConfig is one leg of a ramping profile.
type StageConfig struct {
	Duration Duration `yaml:"duration"`
	Target   int      `yaml:"target"`
}

// HTTPConfig mirrors the tunable parts of loadwave.HTTPOptions.
type HTTPConfig struct {
	Timeout               Duration          `yaml:"timeout"`
	Headers               map[string]string `yaml:"headers,omitempty"`
	UserAgent             string            `yaml:"userAgent"`
	InsecureSkipTLSVerify bool              `yaml:"insecureSkipTLSVerify"`
	MaxIdleConnsPerHost   int               `yaml:"maxIdleConnsPerHost"`
	DisableKeepAlives     bool              `yaml:"disableKeepAlives"`
	DisableCompression    bool              `yaml:"disableCompression"`
	FollowRedirects       bool              `yaml:"followRedirects"`
	MaxRedirects          int               `yaml:"maxRedirects"`
	IsolatePerVU          bool              `yaml:"isolatePerVU"`
	DiscardBody           bool              `yaml:"discardBody"`
	MaxBodyBytes          int64             `yaml:"maxBodyBytes"`
	Proxy                 string            `yaml:"proxy"`
}

// Threshold is a pass/fail assertion evaluated when the run ends.
type Threshold struct {
	Metric      string  `yaml:"metric"`
	Stat        string  `yaml:"stat"`
	Op          string  `yaml:"op"`
	Value       float64 `yaml:"value"`
	AbortOnFail bool    `yaml:"abortOnFail"`
}

// Parse reads a configuration from YAML.
//
// Unknown fields are rejected rather than ignored. A silently misspelled key
// in a load test is a costly kind of bug: the run appears to work and quietly
// measures something other than what was asked for.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalWithOptions(data, &cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parse configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks the configuration for internal consistency.
func (c *Config) Validate() error {
	if c.Name == "" {
		c.Name = "loadwave"
	}
	if err := c.Load.validate(); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(c.Scenarios))
	for i := range c.Scenarios {
		s := &c.Scenarios[i]
		if err := s.validate(); err != nil {
			return fmt.Errorf("scenario %d: %w", i+1, err)
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("scenario %q is defined more than once", s.Name)
		}
		seen[s.Name] = struct{}{}
	}

	if c.BetweenRequests != "" {
		if _, err := loadwave.ParsePause(c.BetweenRequests); err != nil {
			return fmt.Errorf("betweenRequests: %w", err)
		}
	}

	for i, t := range c.Thresholds {
		if _, err := t.stat(); err != nil {
			return fmt.Errorf("threshold %d: %w", i+1, err)
		}
		if _, err := t.op(); err != nil {
			return fmt.Errorf("threshold %d: %w", i+1, err)
		}
		if t.Metric == "" {
			return fmt.Errorf("threshold %d: metric is required", i+1)
		}
	}
	return nil
}

// Executor names accepted in configuration.
const (
	ExecutorConstantVUs = "constant-vus"
	ExecutorRampingVUs  = "ramping-vus"
)

func (l *LoadConfig) validate() error {
	switch l.Executor {
	case "":
		l.Executor = ExecutorConstantVUs
	case ExecutorConstantVUs, ExecutorRampingVUs:
	default:
		return fmt.Errorf("unknown executor %q, expected %q or %q",
			l.Executor, ExecutorConstantVUs, ExecutorRampingVUs)
	}

	switch l.Executor {
	case ExecutorConstantVUs:
		if l.VUs <= 0 {
			l.VUs = 1
		}
		if l.Duration <= 0 && l.Iterations == 0 {
			return errors.New("constant-vus needs either a duration or an iterations count")
		}
		if len(l.Stages) > 0 {
			return fmt.Errorf("stages are only meaningful for the %q executor", ExecutorRampingVUs)
		}
	case ExecutorRampingVUs:
		if len(l.Stages) == 0 {
			return errors.New("ramping-vus needs at least one stage")
		}
		peak := 0
		for i, s := range l.Stages {
			if s.Duration <= 0 {
				return fmt.Errorf("stage %d needs a positive duration", i+1)
			}
			if s.Target < 0 {
				return fmt.Errorf("stage %d has a negative target", i+1)
			}
			peak = max(peak, s.Target)
		}
		if peak == 0 {
			return errors.New("ramping-vus stages never rise above zero VUs")
		}
	}

	if l.MaxIterationRate < 0 {
		return errors.New("maxIterationRate cannot be negative")
	}
	return nil
}

// asUint32 narrows a configured count to the width the wire format uses.
//
// These arrive as platform ints, from YAML or from a flag parsed with Atoi, so
// a value past the wire type's range would wrap silently: a stage target of
// five billion becomes 705,032,704, which is plausible enough that nobody
// would question it. Refused rather than clamped — neither the number asked
// for nor the one it wrapped to is what the author meant, and a load test that
// quietly runs a different profile than the one written down is worse than one
// that will not start.
func asUint32(field string, v int) (uint32, error) {
	if v < 0 {
		return 0, fmt.Errorf("%s cannot be negative, got %d", field, v)
	}
	// Compared in uint64 rather than against an int constant, which would not
	// compile on a 32-bit platform.
	if uint64(v) > math.MaxUint32 {
		return 0, fmt.Errorf("%s is too large: %d, the most that can be asked for is %d",
			field, v, uint32(math.MaxUint32))
	}
	return uint32(v), nil
}

// Plan converts the configuration into the wire-format test plan the
// coordinator distributes.
func (c *Config) Plan() (*loadwavev1.TestPlan, error) {
	vus, err := asUint32("vus", c.Load.VUs)
	if err != nil {
		return nil, err
	}
	rate, err := asUint32("maxIterationRate", c.Load.MaxIterationRate)
	if err != nil {
		return nil, err
	}
	workers, err := asUint32("workersPerAgent", c.WorkersPerAgent)
	if err != nil {
		return nil, err
	}

	load := &loadwavev1.LoadProfile{
		Vus:                    vus,
		Duration:               durationpb.New(c.Load.Duration.Std()),
		MaxIterationsPerSecond: rate,
		Iterations:             c.Load.Iterations,
		GracefulStop:           durationpb.New(c.Load.GracefulStop.Std()),
	}

	switch c.Load.Executor {
	case ExecutorRampingVUs:
		load.Executor = loadwavev1.ExecutorType_EXECUTOR_TYPE_RAMPING_VUS
		for i, s := range c.Load.Stages {
			target, err := asUint32(fmt.Sprintf("stage %d target", i+1), s.Target)
			if err != nil {
				return nil, err
			}
			load.Stages = append(load.Stages, &loadwavev1.Stage{
				Duration: durationpb.New(s.Duration.Std()),
				Target:   target,
			})
		}
	default:
		load.Executor = loadwavev1.ExecutorType_EXECUTOR_TYPE_CONSTANT_VUS
	}

	plan := &loadwavev1.TestPlan{
		Name:            c.Name,
		BaseUrl:         c.BaseURL,
		Load:            load,
		Tags:            c.Tags,
		WorkersPerAgent: workers,
	}

	for _, s := range c.Scenarios {
		weight, err := asUint32(fmt.Sprintf("scenario %q weight", s.Name), max(1, s.Weight))
		if err != nil {
			return nil, err
		}
		plan.Scenarios = append(plan.Scenarios, &loadwavev1.ScenarioRef{
			Name:   s.Name,
			Weight: weight,
		})
	}

	for _, t := range c.Thresholds {
		stat, err := t.stat()
		if err != nil {
			return nil, err
		}
		op, err := t.op()
		if err != nil {
			return nil, err
		}
		plan.Thresholds = append(plan.Thresholds, &loadwavev1.Threshold{
			Metric:      t.Metric,
			Stat:        stat,
			Op:          op,
			Value:       t.Value,
			AbortOnFail: t.AbortOnFail,
		})
	}

	// The whole configuration travels with the plan so that a worker on
	// another host can reconstruct the HTTP client settings and any
	// declaratively-defined scenarios without the coordinator having to
	// mirror that schema into protobuf.
	encoded, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode configuration: %w", err)
	}
	plan.ConfigYaml = encoded

	return plan, nil
}

// FromPlan recovers the configuration a plan was built from.
//
// Workers use this to rebuild the HTTP settings and declarative scenarios. A
// plan with no embedded configuration — one assembled programmatically rather
// than from a file — yields a configuration carrying just the base URL, which
// is enough for a run whose scenarios are all compiled into the binary.
func FromPlan(plan *loadwavev1.TestPlan) (*Config, error) {
	if len(plan.GetConfigYaml()) == 0 {
		return &Config{Name: plan.GetName(), BaseURL: plan.GetBaseUrl()}, nil
	}

	cfg, err := Parse(plan.GetConfigYaml())
	if err != nil {
		return nil, fmt.Errorf("recover configuration from plan: %w", err)
	}
	return cfg, nil
}

// BetweenRequestsPause resolves the run's pacing, default included.
//
// Every caller must go through this rather than reading the raw field. An
// empty field means "use the default", and treating it as the zero Pause
// reports — and applies — flat out, which is the opposite of what an
// unconfigured run should do.
func (c *Config) BetweenRequestsPause() loadwave.Pause {
	if c.BetweenRequests == "" {
		return loadwave.NewPause(loadwave.DefaultBetweenRequests)
	}

	pause, err := loadwave.ParsePause(c.BetweenRequests)
	if err != nil {
		// Validate rejects this, so it cannot happen; the default is still
		// the right answer if it somehow slips through, because the
		// alternative is an unpaced run.
		return loadwave.NewPause(loadwave.DefaultBetweenRequests)
	}
	return pause
}

// HTTPOptions renders the HTTP settings for the SDK's client factory.
func (c *Config) HTTPOptions() loadwave.HTTPOptions {
	headers := make(map[string][]string, len(c.HTTP.Headers))
	for k, v := range c.HTTP.Headers {
		headers[k] = []string{v}
	}

	options := loadwave.HTTPOptions{
		BaseURL:               c.BaseURL,
		Timeout:               c.HTTP.Timeout.Std(),
		Headers:               headers,
		UserAgent:             c.HTTP.UserAgent,
		InsecureSkipTLSVerify: c.HTTP.InsecureSkipTLSVerify,
		MaxIdleConnsPerHost:   c.HTTP.MaxIdleConnsPerHost,
		DisableKeepAlives:     c.HTTP.DisableKeepAlives,
		DisableCompression:    c.HTTP.DisableCompression,
		FollowRedirects:       c.HTTP.FollowRedirects,
		MaxRedirects:          c.HTTP.MaxRedirects,
		IsolatePerVU:          c.HTTP.IsolatePerVU,
		DiscardBody:           c.HTTP.DiscardBody,
		MaxBodyBytes:          c.HTTP.MaxBodyBytes,
		Proxy:                 c.HTTP.Proxy,
	}

	// Resolved here rather than left to the factory's defaults, so that what
	// this reports and what the run does cannot disagree.
	pause := c.BetweenRequestsPause()
	options.BetweenRequests = pause
	options.NoBetweenRequests = pause.IsZero()

	return options
}

// statNames maps configuration spellings onto the wire enum.
var statNames = map[string]loadwavev1.ThresholdStat{
	"count": loadwavev1.ThresholdStat_THRESHOLD_STAT_COUNT,
	"rate":  loadwavev1.ThresholdStat_THRESHOLD_STAT_RATE,
	"avg":   loadwavev1.ThresholdStat_THRESHOLD_STAT_AVG,
	"mean":  loadwavev1.ThresholdStat_THRESHOLD_STAT_AVG,
	"min":   loadwavev1.ThresholdStat_THRESHOLD_STAT_MIN,
	"max":   loadwavev1.ThresholdStat_THRESHOLD_STAT_MAX,
	"p50":   loadwavev1.ThresholdStat_THRESHOLD_STAT_P50,
	"med":   loadwavev1.ThresholdStat_THRESHOLD_STAT_P50,
	"p90":   loadwavev1.ThresholdStat_THRESHOLD_STAT_P90,
	"p95":   loadwavev1.ThresholdStat_THRESHOLD_STAT_P95,
	"p99":   loadwavev1.ThresholdStat_THRESHOLD_STAT_P99,
	"p999":  loadwavev1.ThresholdStat_THRESHOLD_STAT_P999,
}

// opNames maps comparison spellings onto the wire enum.
var opNames = map[string]loadwavev1.ThresholdOp{
	"<":  loadwavev1.ThresholdOp_THRESHOLD_OP_LT,
	"lt": loadwavev1.ThresholdOp_THRESHOLD_OP_LT,
	"<=": loadwavev1.ThresholdOp_THRESHOLD_OP_LTE,
	"le": loadwavev1.ThresholdOp_THRESHOLD_OP_LTE,
	">":  loadwavev1.ThresholdOp_THRESHOLD_OP_GT,
	"gt": loadwavev1.ThresholdOp_THRESHOLD_OP_GT,
	">=": loadwavev1.ThresholdOp_THRESHOLD_OP_GTE,
	"ge": loadwavev1.ThresholdOp_THRESHOLD_OP_GTE,
}

func (t Threshold) stat() (loadwavev1.ThresholdStat, error) {
	stat, ok := statNames[strings.ToLower(strings.TrimSpace(t.Stat))]
	if !ok {
		return 0, fmt.Errorf("unknown stat %q", t.Stat)
	}
	return stat, nil
}

func (t Threshold) op() (loadwavev1.ThresholdOp, error) {
	op, ok := opNames[strings.ToLower(strings.TrimSpace(t.Op))]
	if !ok {
		return 0, fmt.Errorf("unknown comparison %q", t.Op)
	}
	return op, nil
}
