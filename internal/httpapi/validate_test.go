// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// The builder revalidates on every edit, so these cases stand in for what it
// puts on screen. A server constructed by hand keeps the coordinator out of it:
// validation deliberately touches nothing that a run would.
func testServer(registry *loadwave.Registry) *Server {
	return &Server{cfg: Config{Registry: registry}}
}

const builderOutput = `
name: checkout-flow
baseURL: "https://shop.example.com"
load:
  executor: ramping-vus
  gracefulStop: 30s
  stages:
    - { duration: 30s, target: 25 }
    - { duration: 2m, target: 25 }
    - { duration: 30s, target: 0 }
thresholds:
  - { metric: http_req_duration, stat: p95, op: "<", value: 500 }
scenarios:
  - name: browse
    weight: 3
    description: "Look around"
    steps:
      - get: /
        expect: [200]
      - think: 1s
`

func TestValidateSummarisesWhatTheRunnerWouldDo(t *testing.T) {
	result := testServer(nil).validateConfig([]byte(builderOutput))
	if !result.Valid {
		t.Fatalf("valid configuration rejected: %s", result.Error)
	}

	got := result.Summary
	if got == nil {
		t.Fatal("a valid configuration produced no summary")
	}

	// The profile is echoed in words because that, not a green tick, is what
	// tells somebody the runner read the form the way they meant it.
	if want := "30s to 25 VUs, then 2m0s to 25 VUs, then 30s to 0 VUs"; got.Profile != want {
		t.Errorf("profile = %q, want %q", got.Profile, want)
	}
	if got.PeakVUs != 25 {
		t.Errorf("peak VUs = %d, want 25", got.PeakVUs)
	}
	if got.DurationSeconds != 180 {
		t.Errorf("duration = %vs, want 180s", got.DurationSeconds)
	}
	if len(got.Scenarios) != 1 {
		t.Fatalf("scenarios = %d, want 1", len(got.Scenarios))
	}

	scenario := got.Scenarios[0]
	if scenario.Name != "browse" || scenario.Weight != 3 || scenario.Steps != 2 {
		t.Errorf("scenario = %+v, want browse ×3 with 2 steps", scenario)
	}
	if scenario.Source != "yaml" {
		t.Errorf("scenario source = %q, want yaml", scenario.Source)
	}
	if scenario.Description != "Look around" {
		t.Errorf("description = %q, want %q", scenario.Description, "Look around")
	}
	if want := []string{"http_req_duration p95 < 500"}; !slices.Equal(got.Thresholds, want) {
		t.Errorf("thresholds = %v, want %v", got.Thresholds, want)
	}
}

// Pacing is reported resolved rather than as written, because an omitted field
// still paces the run: a builder that showed "none" for a blank field would be
// describing a test nobody asked for.
func TestValidateReportsResolvedPacing(t *testing.T) {
	cases := []struct {
		name      string
		field     string
		want      string
		defaulted bool
	}{
		{name: "omitted", field: "", want: "1s", defaulted: true},
		{name: "range", field: "betweenRequests: 500ms-2s", want: "500ms-2s"},
		{name: "explicitly none", field: "betweenRequests: \"0\"", want: "0s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := strings.Replace(builderOutput, "name: checkout-flow",
				"name: checkout-flow\n"+tc.field, 1)

			result := testServer(nil).validateConfig([]byte(cfg))
			if !result.Valid {
				t.Fatalf("rejected: %s", result.Error)
			}
			if result.Summary.BetweenRequests != tc.want {
				t.Errorf("betweenRequests = %q, want %q", result.Summary.BetweenRequests, tc.want)
			}
			if result.Summary.PacingDefaulted != tc.defaulted {
				t.Errorf("pacingDefaulted = %v, want %v", result.Summary.PacingDefaulted, tc.defaulted)
			}
		})
	}
}

// A scenario named but not defined is the one thing the builder cannot judge
// for itself: whether it is compiled into this binary or simply misspelled
// depends on the binary being talked to.
func TestValidateDistinguishesCompiledInScenarios(t *testing.T) {
	const named = `
name: go-scenarios
baseURL: "https://shop.example.com"
load: { executor: constant-vus, vus: 1, duration: 10s }
scenarios:
  - name: checkout
`

	registry := loadwave.NewRegistry()
	registry.MustRegister(loadwave.Scenario{
		Name: "checkout",
		Run:  func(context.Context, *loadwave.VU) error { return nil },
	})

	result := testServer(registry).validateConfig([]byte(named))
	if !result.Valid {
		t.Fatalf("a registered scenario was rejected: %s", result.Error)
	}
	if got := result.Summary.Scenarios[0].Source; got != "go" {
		t.Errorf("source = %q, want go", got)
	}

	// The same configuration against a binary without it must fail, and say so.
	missing := testServer(nil).validateConfig([]byte(named))
	if missing.Valid {
		t.Fatal("a scenario this binary does not have was accepted")
	}
	if !strings.Contains(missing.Error, "checkout") {
		t.Errorf("error %q does not name the missing scenario", missing.Error)
	}
}

// Validation is idempotent. It has not always been: compiling steps once
// mutated the parsed configuration, so a second pass on the same bytes failed
// with a contradiction. A builder revalidates constantly, so this matters.
func TestValidateIsRepeatable(t *testing.T) {
	server := testServer(nil)
	for i := range 3 {
		if result := server.validateConfig([]byte(builderOutput)); !result.Valid {
			t.Fatalf("pass %d rejected what pass 1 accepted: %s", i+1, result.Error)
		}
	}
}

// An invalid configuration is a successful answer to the question being asked.
// Answering 4xx would make a builder that checks on every keystroke read
// failure out of the status code and show a transport error instead.
func TestValidateEndpointAnswersOKForInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty", body: "", want: "empty"},
		{name: "not yaml", body: "\tthis: [is not", want: "tab character"},
		{name: "unknown field", body: "name: x\nvus: 4\n", want: "vus"},
		{
			name: "unknown executor",
			body: "name: x\nload:\n  executor: nope\n",
			want: "unknown executor",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/validate", strings.NewReader(tc.body))
			recorder := httptest.NewRecorder()

			testServer(nil).handleValidate(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}

			var result ValidateResult
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if result.Valid {
				t.Fatal("invalid configuration reported valid")
			}
			// The parser's own message, which carries the offending field.
			if !strings.Contains(strings.ToLower(result.Error), tc.want) {
				t.Errorf("error %q does not mention %q", result.Error, tc.want)
			}
		})
	}
}

func TestValidateEndpointAcceptsBuilderOutput(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/validate", strings.NewReader(builderOutput))
	request.Header.Set("Content-Type", "application/yaml")
	recorder := httptest.NewRecorder()

	testServer(nil).handleValidate(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var result ValidateResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !result.Valid || result.Summary == nil {
		t.Fatalf("builder output rejected: %s", result.Error)
	}
}
