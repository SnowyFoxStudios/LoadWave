// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// ScenarioConfig is one entry in a configuration's scenario list.
//
// An entry carrying steps defines the scenario here, declaratively. An entry
// with only a name refers to a scenario compiled into the binary through the
// Go SDK, and exists just to set its weight.
type ScenarioConfig struct {
	Name        string            `yaml:"name,omitempty"`
	Weight      int               `yaml:"weight,omitempty"`
	Description string            `yaml:"description,omitempty"`
	Vars        map[string]string `yaml:"vars,omitempty"`
	Steps       []StepConfig      `yaml:"steps,omitempty"`
}

// StepConfig is one action in a declarative scenario: either an HTTP request
// or a pause.
type StepConfig struct {
	// Name labels the step's metrics. Defaults to the method and path with
	// variable segments collapsed.
	Name string `yaml:"name,omitempty"`

	// Method and URL state the request explicitly. Alternatively use one of
	// the shorthands below, which set both at once.
	Method string `yaml:"method,omitempty"`
	URL    string `yaml:"url,omitempty"`

	Get    string `yaml:"get,omitempty"`
	Post   string `yaml:"post,omitempty"`
	Put    string `yaml:"put,omitempty"`
	Patch  string `yaml:"patch,omitempty"`
	Delete string `yaml:"delete,omitempty"`
	Head   string `yaml:"head,omitempty"`

	Headers map[string]string `yaml:"headers,omitempty"`
	Query   map[string]string `yaml:"query,omitempty"`

	// At most one body form may be set.
	JSON any               `yaml:"json,omitempty"`
	Form map[string]string `yaml:"form,omitempty"`
	Body string            `yaml:"body,omitempty"`

	// Expect lists acceptable status codes. A response outside the list
	// fails the step's check and ends the iteration.
	Expect []int `yaml:"expect,omitempty"`

	// Capture pulls values out of the JSON response into variables that
	// later steps can interpolate with ${name}.
	Capture map[string]string `yaml:"capture,omitempty"`

	// Think pauses instead of making a request. Either a fixed duration
	// ("2s") or a range ("1s-3s") drawn uniformly.
	Think string `yaml:"think,omitempty"`

	Timeout Duration `yaml:"timeout,omitempty"`

	// BetweenRequests overrides the run's pacing for this step alone: a
	// duration, a range, or "0" for no pause at all. Empty uses the run's
	// setting.
	BetweenRequests string `yaml:"betweenRequests,omitempty"`
}

// shorthands maps a StepConfig field onto the method it implies.
func (s *StepConfig) shorthands() []struct {
	method string
	url    string
} {
	return []struct {
		method string
		url    string
	}{
		{http.MethodGet, s.Get},
		{http.MethodPost, s.Post},
		{http.MethodPut, s.Put},
		{http.MethodPatch, s.Patch},
		{http.MethodDelete, s.Delete},
		{http.MethodHead, s.Head},
	}
}

func (s *ScenarioConfig) validate() error {
	if s.Name == "" {
		return errors.New("name is required")
	}
	if s.Weight < 0 {
		return errors.New("weight cannot be negative")
	}
	for i := range s.Steps {
		if err := s.Steps[i].validate(); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *StepConfig) validate() error {
	if s.Think != "" {
		if _, _, err := parseThink(s.Think); err != nil {
			return err
		}
		if s.URL != "" || s.Method != "" || s.hasShorthand() {
			return errors.New("a think step cannot also make a request")
		}
		return nil
	}

	// resolve is not allowed to write back to the step. Validation runs more
	// than once on the same configuration — once when it is parsed, and again
	// after command-line overrides are applied — and a step that recorded its
	// shorthand into URL would fail the second pass for setting both.
	_, target, err := s.resolve()
	if err != nil {
		return err
	}
	if target == "" {
		return errors.New("no URL: give one of get/post/put/patch/delete/head, or method and url")
	}

	// Counted by emptiness, not by nil-ness. A plan travels to remote workers
	// as YAML, and a nil map marshals to `{}` and comes back non-nil — so a
	// nil check here would reject a configuration this very process produced.
	bodies := 0
	if s.JSON != nil {
		bodies++
	}
	if len(s.Form) > 0 {
		bodies++
	}
	if s.Body != "" {
		bodies++
	}
	if bodies > 1 {
		return errors.New("set at most one of json, form and body")
	}

	for name, path := range s.Capture {
		if _, err := ParsePath(path); err != nil {
			return fmt.Errorf("capture %q: %w", name, err)
		}
	}

	if s.BetweenRequests != "" {
		if _, err := loadwave.ParsePause(s.BetweenRequests); err != nil {
			return fmt.Errorf("betweenRequests: %w", err)
		}
	}
	return nil
}

// hasShorthand reports whether any method shorthand is set.
func (s *StepConfig) hasShorthand() bool {
	for _, sh := range s.shorthands() {
		if sh.url != "" {
			return true
		}
	}
	return false
}

// resolve works out the method and URL from the explicit fields or a
// shorthand, rejecting a step that sets more than one.
func (s *StepConfig) resolve() (string, string, error) {
	method, target := strings.ToUpper(s.Method), s.URL

	found := 0
	for _, sh := range s.shorthands() {
		if sh.url == "" {
			continue
		}
		found++
		method, target = sh.method, sh.url
	}

	switch {
	case found > 1:
		return "", "", errors.New("more than one method shorthand is set")
	case found == 1 && s.URL != "":
		return "", "", errors.New("set either a method shorthand or url, not both")
	case found == 0 && method == "":
		method = http.MethodGet
	}
	return method, target, nil
}

// parseThink reads a fixed duration or a "min-max" range.
func parseThink(spec string) (time.Duration, time.Duration, error) {
	spec = strings.TrimSpace(spec)

	// Split on the last '-' so that a bare negative duration still fails
	// loudly rather than being read as a range with an empty lower bound.
	if idx := strings.LastIndex(spec, "-"); idx > 0 {
		lo, errLo := time.ParseDuration(strings.TrimSpace(spec[:idx]))
		hi, errHi := time.ParseDuration(strings.TrimSpace(spec[idx+1:]))
		if errLo == nil && errHi == nil {
			if lo < 0 || hi < lo {
				return 0, 0, fmt.Errorf("think range %q must be ascending and non-negative", spec)
			}
			return lo, hi, nil
		}
	}

	fixed, err := time.ParseDuration(spec)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid think time %q: expected \"2s\" or \"1s-3s\"", spec)
	}
	if fixed < 0 {
		return 0, 0, fmt.Errorf("think time %q cannot be negative", spec)
	}
	return fixed, fixed, nil
}

// step is a validated, pre-compiled action.
type step struct {
	name    string
	method  string
	url     *Template
	headers map[string]*Template
	query   map[string]*Template
	body    *Template
	jsonDoc any
	form    map[string]*Template
	expect  []int
	capture map[string]*Path
	timeout time.Duration

	// pace overrides the run's pacing for this step. Nil uses the run's.
	pace *loadwave.Pause

	thinkMin time.Duration
	thinkMax time.Duration
	isThink  bool
}

// compileStep pre-parses every template and path in a step.
func compileStep(cfg *StepConfig) (*step, error) {
	if cfg.Think != "" {
		lo, hi, err := parseThink(cfg.Think)
		if err != nil {
			return nil, err
		}
		return &step{name: cfg.Name, isThink: true, thinkMin: lo, thinkMax: hi}, nil
	}

	method, target, err := cfg.resolve()
	if err != nil {
		return nil, err
	}

	urlTemplate, err := ParseTemplate(target)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}

	s := &step{
		name:    cfg.Name,
		method:  method,
		url:     urlTemplate,
		expect:  cfg.Expect,
		timeout: cfg.Timeout.Std(),
		jsonDoc: cfg.JSON,
	}

	if s.headers, err = compileTemplateMap(cfg.Headers); err != nil {
		return nil, fmt.Errorf("headers: %w", err)
	}
	if s.query, err = compileTemplateMap(cfg.Query); err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	if s.form, err = compileTemplateMap(cfg.Form); err != nil {
		return nil, fmt.Errorf("form: %w", err)
	}
	if cfg.Body != "" {
		if s.body, err = ParseTemplate(cfg.Body); err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
	}

	if len(cfg.Capture) > 0 {
		s.capture = make(map[string]*Path, len(cfg.Capture))
		for name, expr := range cfg.Capture {
			path, err := ParsePath(expr)
			if err != nil {
				return nil, fmt.Errorf("capture %q: %w", name, err)
			}
			s.capture[name] = path
		}
	}

	if cfg.BetweenRequests != "" {
		pause, err := loadwave.ParsePause(cfg.BetweenRequests)
		if err != nil {
			return nil, fmt.Errorf("betweenRequests: %w", err)
		}
		s.pace = &pause
	}

	if s.name == "" {
		// Derive from the template text rather than a rendered URL, so that
		// every iteration reports the same series regardless of what the
		// placeholders happened to resolve to.
		s.name = loadwave.DeriveRequestName(s.method, templatePath(urlTemplate.String()))
	}
	return s, nil
}

// compileTemplateMap parses every value of a string map as a template.
func compileTemplateMap(in map[string]string) (map[string]*Template, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]*Template, len(in))
	for k, v := range in {
		parsed, err := ParseTemplate(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = parsed
	}
	return out, nil
}

// templatePath extracts the path portion of a URL template for naming, with
// placeholders left intact so they read as the variables they are.
func templatePath(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Path != "" {
		return parsed.Path
	}
	if idx := strings.Index(raw, "?"); idx >= 0 {
		return raw[:idx]
	}
	return raw
}

// BuildScenarios compiles every declarative scenario in the configuration and
// registers it.
//
// Scenarios that only reference compiled-in Go code are skipped, since they
// are already in the registry. A name that collides with a registered Go
// scenario is an error rather than an override: silently shadowing compiled
// code with YAML would be a genuinely confusing thing to debug.
func (c *Config) BuildScenarios(registry *loadwave.Registry) error {
	// A configuration with no scenarios at all, in a binary with none
	// compiled in, is the `--url ... --vus ... --duration ...` smoke test.
	// Synthesising a single GET makes that one-liner work instead of failing
	// with "nothing to run", which is a poor first experience of the tool.
	if len(c.Scenarios) == 0 && registry.Len() == 0 && c.BaseURL != "" {
		return c.registerDefaultScenario(registry)
	}

	for i := range c.Scenarios {
		cfg := &c.Scenarios[i]
		if len(cfg.Steps) == 0 {
			if _, ok := registry.Lookup(cfg.Name); !ok {
				return fmt.Errorf(
					"scenario %q has no steps and is not registered in this binary; "+
						"add steps to define it here, or build a binary that registers it",
					cfg.Name)
			}
			continue
		}

		if _, exists := registry.Lookup(cfg.Name); exists {
			return fmt.Errorf(
				"scenario %q is defined in the configuration and also registered in this binary; rename one",
				cfg.Name)
		}

		steps := make([]*step, 0, len(cfg.Steps))
		for j := range cfg.Steps {
			compiled, err := compileStep(&cfg.Steps[j])
			if err != nil {
				return fmt.Errorf("scenario %q step %d: %w", cfg.Name, j+1, err)
			}
			steps = append(steps, compiled)
		}

		runner := &declarative{name: cfg.Name, vars: cfg.Vars, steps: steps}
		if err := registry.Register(loadwave.Scenario{
			Name:        cfg.Name,
			Weight:      cfg.Weight,
			Description: cfg.Description,
			Run:         runner.run,
		}); err != nil {
			return err
		}
	}
	return nil
}

// DefaultScenarioName is the scenario synthesised for a bare --url run.
const DefaultScenarioName = "default"

// registerDefaultScenario adds a single GET of the base URL's path.
func (c *Config) registerDefaultScenario(registry *loadwave.Registry) error {
	target, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("parse base URL %q: %w", c.BaseURL, err)
	}

	path := target.Path
	if path == "" {
		path = "/"
	}

	compiled, err := compileStep(&StepConfig{
		Name:   loadwave.DeriveRequestName(http.MethodGet, path),
		Get:    path,
		Expect: []int{http.StatusOK},
	})
	if err != nil {
		return err
	}

	runner := &declarative{name: DefaultScenarioName, steps: []*step{compiled}}
	return registry.Register(loadwave.Scenario{
		Name:        DefaultScenarioName,
		Description: "Synthesised from --url: a single GET.",
		Run:         runner.run,
	})
}

// declarative executes a compiled step list.
type declarative struct {
	name  string
	vars  map[string]string
	steps []*step
}

// run performs one iteration of the scenario.
func (d *declarative) run(ctx context.Context, vu *loadwave.VU) error {
	vars := NewVars(vu, d.vars)

	for _, s := range d.steps {
		if s.isThink {
			vu.ThinkBetween(ctx, s.thinkMin, s.thinkMax)
			continue
		}
		if err := d.runStep(ctx, vu, s, vars); err != nil {
			return err
		}
	}
	return nil
}

// runStep issues one request and applies its expectations and captures.
func (d *declarative) runStep(ctx context.Context, vu *loadwave.VU, s *step, vars *Vars) error {
	req := loadwave.Request{
		Method:          s.method,
		URL:             s.url.Render(vars),
		Name:            s.name,
		Timeout:         s.timeout,
		ExpectStatus:    s.expect,
		BetweenRequests: s.pace,
	}

	if len(s.headers) > 0 {
		req.Header = make(http.Header, len(s.headers))
		for k, tmpl := range s.headers {
			req.Header.Set(k, tmpl.Render(vars))
		}
	}
	if len(s.query) > 0 {
		req.Query = make(url.Values, len(s.query))
		for k, tmpl := range s.query {
			req.Query.Set(k, tmpl.Render(vars))
		}
	}
	if len(s.form) > 0 {
		req.Form = make(url.Values, len(s.form))
		for k, tmpl := range s.form {
			req.Form.Set(k, tmpl.Render(vars))
		}
	}
	if s.body != nil {
		req.Body = []byte(s.body.Render(vars))
	}
	if s.jsonDoc != nil {
		req.JSON = renderJSON(s.jsonDoc, vars)
	}

	resp, err := vu.HTTP().Do(ctx, req)
	if err != nil {
		return fmt.Errorf("step %q: %w", s.name, err)
	}

	if len(s.expect) > 0 {
		ok := slices.Contains(s.expect, resp.StatusCode)
		if !vu.Checkf(s.name, ok, "expected one of %v, got %d", s.expect, resp.StatusCode) {
			return fmt.Errorf("step %q: expected status in %v, got %d", s.name, s.expect, resp.StatusCode)
		}
	}

	for name, path := range s.capture {
		value, err := path.Extract(resp.Body)
		if err != nil {
			return fmt.Errorf("step %q: %w", s.name, err)
		}
		vars.Set(name, value)
	}
	return nil
}

// renderJSON walks a decoded YAML document substituting templates into every
// string, so that placeholders work at any depth of a JSON body.
func renderJSON(doc any, vars *Vars) any {
	switch typed := doc.(type) {
	case string:
		if !strings.Contains(typed, "${") {
			return typed
		}
		tmpl, err := ParseTemplate(typed)
		if err != nil {
			return typed
		}
		return tmpl.Render(vars)

	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			out[k] = renderJSON(v, vars)
		}
		return out

	case []any:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = renderJSON(v, vars)
		}
		return out

	default:
		return doc
	}
}
