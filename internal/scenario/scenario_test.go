// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

const minimalConfig = `
name: example
baseURL: https://example.com
load:
  executor: ramping-vus
  stages:
    - { duration: 10s, target: 5 }
scenarios:
  - name: browse
    steps:
      - get: /api/things
        expect: [200]
`

func TestParseAndPlan(t *testing.T) {
	t.Parallel()

	cfg, err := scenario.Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Name != "example" || cfg.BaseURL != "https://example.com" {
		t.Fatalf("parsed %+v", cfg)
	}

	plan, err := cfg.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.GetLoad().GetExecutor() != loadwavev1.ExecutorType_EXECUTOR_TYPE_RAMPING_VUS {
		t.Errorf("executor = %s", plan.GetLoad().GetExecutor())
	}
	if len(plan.GetScenarios()) != 1 || plan.GetScenarios()[0].GetName() != "browse" {
		t.Errorf("scenarios = %v", plan.GetScenarios())
	}
	if len(plan.GetConfigYaml()) == 0 {
		t.Error("the plan must carry the configuration for remote workers to rebuild")
	}

	// A worker on another host recovers the configuration from the plan, so
	// the round trip has to preserve everything it needs.
	recovered, err := scenario.FromPlan(plan)
	if err != nil {
		t.Fatalf("FromPlan: %v", err)
	}
	if recovered.BaseURL != cfg.BaseURL || len(recovered.Scenarios) != 1 {
		t.Fatalf("round trip lost data: %+v", recovered)
	}
	if len(recovered.Scenarios[0].Steps) != 1 {
		t.Fatal("round trip lost the declarative steps")
	}
}

func TestValidateIsIdempotent(t *testing.T) {
	t.Parallel()

	// Validation runs when the file is parsed and again after command-line
	// overrides are applied. A validator that rewrote the config would fail
	// the second pass — this caught a real bug.
	cfg, err := scenario.Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for i := range 3 {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validation %d failed: %v", i+1, err)
		}
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	// A misspelled key must be an error. Ignoring it would let a run appear
	// to work while quietly measuring something else.
	_, err := scenario.Parse([]byte(`
name: example
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: browse
    stepz:
      - get: /
`))
	if err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestParseRejectsBadConfigurations(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"unknown executor": `
load: { executor: wobble, vus: 1, duration: 1s }`,
		"constant-vus with no end": `
load: { executor: constant-vus, vus: 5 }`,
		"ramping with no stages": `
load: { executor: ramping-vus }`,
		"stages on constant-vus": `
load: { executor: constant-vus, vus: 1, duration: 1s, stages: [{ duration: 1s, target: 1 }] }`,
		"unknown threshold stat": `
load: { executor: constant-vus, vus: 1, duration: 1s }
thresholds: [{ metric: http_req_duration, stat: p42, op: "<", value: 1 }]`,
		"unknown threshold operator": `
load: { executor: constant-vus, vus: 1, duration: 1s }
thresholds: [{ metric: http_req_duration, stat: p95, op: "~=", value: 1 }]`,
		"duplicate scenario name": `
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - { name: a, steps: [{ get: / }] }
  - { name: a, steps: [{ get: / }] }`,
		"step with two body forms": `
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: a
    steps:
      - post: /
        json: { a: 1 }
        body: "raw"`,
		"step with two methods": `
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: a
    steps:
      - get: /a
        post: /b`,
		"think step that also requests": `
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: a
    steps:
      - think: 1s
        get: /`,
		"invalid think range": `
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: a
    steps:
      - think: 5s-1s`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := scenario.Parse([]byte(body)); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestTemplateRendering(t *testing.T) {
	t.Parallel()

	vars := scenario.NewVars(nil, map[string]string{"id": "42", "name": "widget"})

	cases := map[string]string{
		"/things/${id}":            "/things/42",
		"${name}-${id}":            "widget-42",
		"no placeholders":          "no placeholders",
		"${missing}":               "",
		"prefix-${id}-suffix":      "prefix-42-suffix",
		"${ id }":                  "42", // surrounding space is trimmed
		"literal $ and { braces }": "literal $ and { braces }",
	}

	for input, want := range cases {
		tmpl, err := scenario.ParseTemplate(input)
		if err != nil {
			t.Fatalf("ParseTemplate(%q): %v", input, err)
		}
		if got := tmpl.Render(vars); got != want {
			t.Errorf("Render(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTemplateRejectsUnterminatedPlaceholder(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"${unterminated", "${}"} {
		if _, err := scenario.ParseTemplate(bad); err == nil {
			t.Errorf("ParseTemplate(%q) was accepted", bad)
		}
	}
}

func TestTemplateBuiltins(t *testing.T) {
	t.Parallel()

	vu := loadwave.NewVU(loadwave.VUConfig{
		ID:    7,
		Shard: loadwave.Shard{Index: 2, Count: 4},
	})
	vu.BeginIteration(3)
	vars := scenario.NewVars(vu, nil)

	if got := vars.Get(scenario.VarVUID); got != "7" {
		t.Errorf("__vu = %q", got)
	}
	if got := vars.Get(scenario.VarIteration); got != "3" {
		t.Errorf("__iteration = %q", got)
	}
	if got := vars.Get(scenario.VarShard); got != "2" {
		t.Errorf("__shard = %q", got)
	}
	if got := vars.Get(scenario.VarShards); got != "4" {
		t.Errorf("__shards = %q", got)
	}

	uuid := vars.Get(scenario.VarUUID)
	if len(uuid) != 36 || strings.Count(uuid, "-") != 4 || uuid[14] != '4' {
		t.Errorf("__uuid = %q, want a v4 UUID", uuid)
	}

	// A user-declared value shadows a built-in deliberately.
	shadowed := scenario.NewVars(vu, map[string]string{scenario.VarVUID: "custom"})
	if got := shadowed.Get(scenario.VarVUID); got != "custom" {
		t.Errorf("shadowing failed: %q", got)
	}
}

func TestJSONPathExtraction(t *testing.T) {
	t.Parallel()

	document := []byte(`{
      "id": 4711,
      "ratio": 0.5,
      "active": true,
      "missing": null,
      "items": [{"sku": "abc", "tags": ["x", "y"]}, {"sku": "def"}],
      "nested": {"deep": {"value": "found"}}
    }`)

	cases := map[string]string{
		"id":                  "4711", // an integral JSON number must not render as 4711.000000
		"$.id":                "4711",
		".id":                 "4711",
		"ratio":               "0.5",
		"active":              "true",
		"missing":             "",
		"items.0.sku":         "abc",
		"items[0].sku":        "abc",
		"items[1].sku":        "def",
		"items[0].tags[1]":    "y",
		"nested.deep.value":   "found",
		"$.nested.deep.value": "found",
	}

	for expr, want := range cases {
		path, err := scenario.ParsePath(expr)
		if err != nil {
			t.Fatalf("ParsePath(%q): %v", expr, err)
		}
		got, err := path.Extract(document)
		if err != nil {
			t.Fatalf("Extract(%q): %v", expr, err)
		}
		if got != want {
			t.Errorf("Extract(%q) = %q, want %q", expr, got, want)
		}
	}
}

func TestJSONPathErrors(t *testing.T) {
	t.Parallel()

	document := []byte(`{"items": [1, 2], "obj": {"a": 1}}`)

	for _, expr := range []string{"nope", "items.5", "obj.b", "items.a", "obj.0"} {
		path, err := scenario.ParsePath(expr)
		if err != nil {
			continue // rejected at parse time, which is also fine
		}
		if _, err := path.Extract(document); err == nil {
			t.Errorf("Extract(%q) should have failed", expr)
		}
	}

	if _, err := scenario.ParsePath(""); err == nil {
		t.Error("an empty path was accepted")
	}

	path, _ := scenario.ParsePath("id")
	if _, err := path.Extract([]byte("not json")); err == nil {
		t.Error("extracting from a non-JSON body should fail")
	}
}

// The declarative interpreter is exercised end to end against a real server:
// templates, captures, expectations and the metric labels it produces.
func TestDeclarativeScenarioExecutes(t *testing.T) {
	t.Parallel()

	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		if strings.HasPrefix(r.URL.Path, "/api/things/") {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":4711}]}`))
	}))
	t.Cleanup(server.Close)

	cfg, err := scenario.Parse([]byte(`
name: capture-test
load: { executor: constant-vus, vus: 1, iterations: 1 }
scenarios:
  - name: browse
    steps:
      - name: list
        get: /api/things
        expect: [200]
        capture:
          thingId: items.0.id
      - name: view
        get: /api/things/${thingId}
        expect: [200]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	registry := loadwave.NewRegistry()
	if err := cfg.BuildScenarios(registry); err != nil {
		t.Fatalf("BuildScenarios: %v", err)
	}

	built, ok := registry.Lookup("browse")
	if !ok {
		t.Fatal("scenario was not registered")
	}

	options := cfg.HTTPOptions()
	options.BaseURL = server.URL
	factory, err := loadwave.NewHTTPClientFactory(options)
	if err != nil {
		t.Fatalf("NewHTTPClientFactory: %v", err)
	}
	t.Cleanup(factory.Close)

	vu := loadwave.NewVU(loadwave.VUConfig{ID: 1, Scenario: "browse", HTTP: factory.New()})
	t.Cleanup(func() { _ = vu.Close() })
	vu.BeginIteration(0)

	if err := built.Run(context.Background(), vu); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The captured id must have reached the second request's URL.
	want := []string{"/api/things", "/api/things/4711"}
	if len(requested) != len(want) {
		t.Fatalf("requested %v, want %v", requested, want)
	}
	for i, path := range want {
		if requested[i] != path {
			t.Fatalf("requested %v, want %v", requested, want)
		}
	}
}

func TestBuildScenariosRejectsCollisionWithGoCode(t *testing.T) {
	t.Parallel()

	registry := loadwave.NewRegistry()
	if err := registry.Register(loadwave.Scenario{
		Name: "browse",
		Run:  func(context.Context, *loadwave.VU) error { return nil },
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cfg, err := scenario.Parse([]byte(`
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: browse
    steps:
      - get: /
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Silently shadowing compiled-in Go code with YAML would be a miserable
	// thing to debug, so it is an error rather than an override.
	if err := cfg.BuildScenarios(registry); err == nil {
		t.Fatal("a YAML scenario was allowed to shadow a registered Go scenario")
	}
}

func TestBuildScenariosRejectsUnknownReference(t *testing.T) {
	t.Parallel()

	cfg, err := scenario.Parse([]byte(`
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: not-compiled-in
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if err := cfg.BuildScenarios(loadwave.NewRegistry()); err == nil {
		t.Fatal("a reference to a scenario that does not exist was accepted")
	}
}

// A plan travels to remote workers as YAML and is parsed again there. Any
// field that survives the trip differently from how it went in will break a
// distributed run while leaving a single-process run working — which is the
// worst possible place for a bug to hide.
func TestPlanRoundTripSurvivesEveryBodyForm(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"json body": `
        - name: create
          post: /api/orders
          json: { productId: 4711, quantity: 1 }
          expect: [201]`,
		"form body": `
        - name: submit
          post: /api/form
          form: { field: value }
          expect: [200]`,
		"raw body": `
        - name: upload
          post: /api/raw
          body: "hello"
          expect: [200]`,
		"no body": `
        - name: fetch
          get: /api/things
          expect: [200]`,
	}

	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			source := `
name: round-trip
baseURL: https://example.com
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: scenario
    steps:` + steps + "\n"

			cfg, err := scenario.Parse([]byte(source))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			// This is the trip a worker on another host makes.
			plan, err := cfg.Plan()
			if err != nil {
				t.Fatalf("build plan: %v", err)
			}
			recovered, err := scenario.FromPlan(plan)
			if err != nil {
				t.Fatalf("recover from plan: %v", err)
			}

			// Nil maps marshal to `{}` and come back non-nil, so a validator
			// counting bodies by nil-ness rejects its own output here.
			if err := recovered.BuildScenarios(loadwave.NewRegistry()); err != nil {
				t.Fatalf("worker could not rebuild the scenario: %v", err)
			}

			// And a second full trip, in case the first one added something
			// that only breaks on the way round again.
			plan2, err := recovered.Plan()
			if err != nil {
				t.Fatalf("second plan: %v", err)
			}
			again, err := scenario.FromPlan(plan2)
			if err != nil {
				t.Fatalf("second recovery: %v", err)
			}
			if err := again.BuildScenarios(loadwave.NewRegistry()); err != nil {
				t.Fatalf("second round trip failed: %v", err)
			}
		})
	}
}

// The bare `--url` smoke test has no scenarios anywhere, and must still run.
func TestDefaultScenarioIsSynthesisedFromBaseURL(t *testing.T) {
	t.Parallel()

	cfg, err := scenario.Parse([]byte(`
name: smoke
baseURL: https://example.com/health
load: { executor: constant-vus, vus: 1, duration: 1s }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	registry := loadwave.NewRegistry()
	if err := cfg.BuildScenarios(registry); err != nil {
		t.Fatalf("BuildScenarios: %v", err)
	}

	if _, ok := registry.Lookup(scenario.DefaultScenarioName); !ok {
		t.Fatalf("no default scenario; registry holds %v", registry.Names())
	}
}

// A binary with compiled-in Go scenarios must not have one invented for it.
func TestDefaultScenarioNotSynthesisedWhenGoScenariosExist(t *testing.T) {
	t.Parallel()

	registry := loadwave.NewRegistry()
	if err := registry.Register(loadwave.Scenario{
		Name: "compiled-in",
		Run:  func(context.Context, *loadwave.VU) error { return nil },
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cfg, err := scenario.Parse([]byte(`
baseURL: https://example.com
load: { executor: constant-vus, vus: 1, duration: 1s }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.BuildScenarios(registry); err != nil {
		t.Fatalf("BuildScenarios: %v", err)
	}

	if _, ok := registry.Lookup(scenario.DefaultScenarioName); ok {
		t.Fatal("a default scenario was invented despite compiled-in scenarios")
	}
}

// Pacing has to survive the trip to a remote worker, and a per-step override
// has to reach the request the worker actually issues.
func TestPacingConfiguration(t *testing.T) {
	t.Parallel()

	cfg, err := scenario.Parse([]byte(`
name: paced
baseURL: https://example.com
betweenRequests: 250ms-750ms
load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: browse
    steps:
      - get: /a
      - get: /b
        betweenRequests: "0"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	options := cfg.HTTPOptions()
	want := loadwave.NewPauseRange(250*time.Millisecond, 750*time.Millisecond)
	if options.BetweenRequests != want {
		t.Fatalf("run pacing = %+v, want %+v", options.BetweenRequests, want)
	}
	if options.NoBetweenRequests {
		t.Error("pacing was disabled despite being configured")
	}

	// Through a plan and back, as a worker on another host would see it.
	plan, err := cfg.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	recovered, err := scenario.FromPlan(plan)
	if err != nil {
		t.Fatalf("FromPlan: %v", err)
	}
	if got := recovered.HTTPOptions().BetweenRequests; got != want {
		t.Fatalf("pacing after a round trip = %+v, want %+v", got, want)
	}
	if got := recovered.Scenarios[0].Steps[1].BetweenRequests; got != "0" {
		t.Fatalf("per-step override after a round trip = %q, want \"0\"", got)
	}
	if err := recovered.BuildScenarios(loadwave.NewRegistry()); err != nil {
		t.Fatalf("worker could not rebuild the scenario: %v", err)
	}
}

func TestPacingDefaultsWhenUnset(t *testing.T) {
	t.Parallel()

	cfg, err := scenario.Parse([]byte(`
baseURL: https://example.com
load: { executor: constant-vus, vus: 1, duration: 1s }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// An unconfigured run must be paced, not flat out — that is the whole
	// point of the default.
	options := cfg.HTTPOptions()
	if options.NoBetweenRequests {
		t.Fatal("an unconfigured run was left unpaced")
	}
	if got := options.BetweenRequests; got != loadwave.NewPause(loadwave.DefaultBetweenRequests) {
		t.Fatalf("default pacing = %+v, want one second", got)
	}
}

func TestPacingCanBeTurnedOffForAThroughputTest(t *testing.T) {
	t.Parallel()

	cfg, err := scenario.Parse([]byte(`
baseURL: https://example.com
betweenRequests: "0"
load: { executor: constant-vus, vus: 1, duration: 1s }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Zero has to mean zero rather than falling back to the default, or a
	// throughput test is impossible to express.
	if !cfg.HTTPOptions().NoBetweenRequests {
		t.Fatal(`betweenRequests: "0" did not disable pacing`)
	}
}

func TestPacingRejectsNonsense(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		"betweenRequests: nope\nload: { executor: constant-vus, vus: 1, duration: 1s }",
		"betweenRequests: 3s-1s\nload: { executor: constant-vus, vus: 1, duration: 1s }",
		`load: { executor: constant-vus, vus: 1, duration: 1s }
scenarios:
  - name: a
    steps:
      - get: /
        betweenRequests: soon`,
	} {
		if _, err := scenario.Parse([]byte(body)); err == nil {
			t.Errorf("an invalid pacing value was accepted:\n%s", body)
		}
	}
}
