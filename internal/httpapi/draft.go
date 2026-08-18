// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
)

// draft mirrors the dashboard builder's own Draft type field for field, so
// the response can be dropped straight into its form state.
//
// Every number and duration travels as a string — the same choice the
// builder itself makes, and for the same reason: a form field is text while
// someone is editing it, and coercing on arrival fights that. Zero-valued
// fields that mean "unset" in the builder (an unlimited iteration rate, a
// default graceful stop) are rendered as an empty string rather than "0", so
// the form shows its usual placeholder instead of a misleading literal zero.
type draft struct {
	Name    string `json:"name"`
	BaseURL string `json:"baseURL"`

	Executor         string       `json:"executor"`
	VUs              string       `json:"vus"`
	Duration         string       `json:"duration"`
	Stages           []draftStage `json:"stages"`
	GracefulStop     string       `json:"gracefulStop"`
	MaxIterationRate string       `json:"maxIterationRate"`
	Iterations       string       `json:"iterations"`

	BetweenRequests string `json:"betweenRequests"`
	WorkersPerAgent string `json:"workersPerAgent"`

	Timeout               string `json:"timeout"`
	FollowRedirects       bool   `json:"followRedirects"`
	InsecureSkipTLSVerify bool   `json:"insecureSkipTLSVerify"`

	Headers []draftPair `json:"headers"`
	Tags    []draftPair `json:"tags"`

	Thresholds []draftThreshold `json:"thresholds"`
	Scenarios  []draftScenario  `json:"scenarios"`
}

type draftStage struct {
	Duration string `json:"duration"`
	Target   string `json:"target"`
}

type draftPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type draftThreshold struct {
	Metric      string `json:"metric"`
	Stat        string `json:"stat"`
	Op          string `json:"op"`
	Value       string `json:"value"`
	AbortOnFail bool   `json:"abortOnFail"`
}

type draftScenario struct {
	Name        string      `json:"name"`
	Weight      string      `json:"weight"`
	Description string      `json:"description"`
	Steps       []draftStep `json:"steps"`
}

// draftStep covers both kinds the builder's Step type does. Kind is "think"
// or "request"; the fields that do not apply to it are simply left zero.
type draftStep struct {
	Kind string `json:"kind"`

	Name            string      `json:"name"`
	Method          string      `json:"method"`
	URL             string      `json:"url"`
	Expect          string      `json:"expect"`
	Headers         []draftPair `json:"headers"`
	Query           []draftPair `json:"query"`
	BodyKind        string      `json:"bodyKind"`
	Body            string      `json:"body"`
	Form            []draftPair `json:"form"`
	Capture         []draftPair `json:"capture"`
	BetweenRequests string      `json:"betweenRequests"`
	Timeout         string      `json:"timeout"`

	Think string `json:"think"`
}

// draftFromConfig renders a parsed configuration as the builder's form shape.
//
// Best-effort, deliberately: the builder's form covers a large but not
// complete slice of the schema (a scenario with no steps, referring to a Go
// scenario compiled into the binary, has nothing to show but its name and
// weight), and anything outside that — an HTTP tuning field the form has no
// control for, say — is simply dropped rather than causing an error. That
// mirrors the loss already accepted in the other direction, when the form's
// own output is generated: the trip is not meant to be lossless, only good
// enough to keep editing from.
func draftFromConfig(cfg *scenario.Config) draft {
	d := draft{
		Name:                  cfg.Name,
		BaseURL:               cfg.BaseURL,
		Executor:              cfg.Load.Executor,
		BetweenRequests:       cfg.BetweenRequests,
		WorkersPerAgent:       intOrBlank(cfg.WorkersPerAgent),
		Timeout:               durationOrBlank(cfg.HTTP.Timeout),
		FollowRedirects:       cfg.HTTP.FollowRedirects,
		InsecureSkipTLSVerify: cfg.HTTP.InsecureSkipTLSVerify,
		Headers:               pairsFromMap(cfg.HTTP.Headers),
		Tags:                  pairsFromMap(cfg.Tags),
		MaxIterationRate:      intOrBlank(cfg.Load.MaxIterationRate),
		Iterations:            uintOrBlank(cfg.Load.Iterations),
		GracefulStop:          durationOrBlank(cfg.Load.GracefulStop),
		// Never nil, any of the three: this whole struct is JSON-encoded
		// straight to the browser, which unconditionally maps over each of
		// these — including stages when the executor is constant-vus, and
		// scenarios or thresholds when there simply are none. A nil Go slice
		// marshals to `null`, and `null.map` throws.
		Stages:     []draftStage{},
		Thresholds: []draftThreshold{},
		Scenarios:  []draftScenario{},
	}

	if cfg.Load.Executor == scenario.ExecutorRampingVUs {
		for _, s := range cfg.Load.Stages {
			d.Stages = append(d.Stages, draftStage{
				Duration: s.Duration.Std().String(),
				Target:   strconv.Itoa(s.Target),
			})
		}
	} else {
		d.VUs = intOrBlank(cfg.Load.VUs)
		d.Duration = durationOrBlank(cfg.Load.Duration)
	}

	for _, t := range cfg.Thresholds {
		d.Thresholds = append(d.Thresholds, draftThreshold{
			Metric:      t.Metric,
			Stat:        t.Stat,
			Op:          t.Op,
			Value:       strconv.FormatFloat(t.Value, 'g', -1, 64),
			AbortOnFail: t.AbortOnFail,
		})
	}

	for _, s := range cfg.Scenarios {
		d.Scenarios = append(d.Scenarios, draftScenarioFrom(s))
	}

	return d
}

func draftScenarioFrom(s scenario.ScenarioConfig) draftScenario {
	out := draftScenario{
		Name:        s.Name,
		Weight:      intOrBlank(s.Weight),
		Description: s.Description,
		// A Go-registered scenario has no steps to show — never nil even
		// then, for the same reason as draftFromConfig's slices.
		Steps: []draftStep{},
	}
	for i := range s.Steps {
		out.Steps = append(out.Steps, draftStepFrom(&s.Steps[i]))
	}
	return out
}

func draftStepFrom(s *scenario.StepConfig) draftStep {
	// Headers, query, form and capture are never nil on either branch below:
	// the dashboard's own conversion back into its form state maps over all
	// four unconditionally, on every step regardless of kind, and a nil Go
	// slice would marshal to `null` and break exactly that map.
	if s.Think != "" {
		return draftStep{
			Kind:    "think",
			Think:   s.Think,
			Headers: []draftPair{},
			Query:   []draftPair{},
			Form:    []draftPair{},
			Capture: []draftPair{},
		}
	}

	// A step this package itself parsed and validated already resolves
	// cleanly; an error here would mean the plan's embedded YAML is
	// inconsistent with its own validator, which is a bug worth surfacing
	// rather than papering over with a blank method and URL.
	method, url, err := s.ResolvedRequest()
	if err != nil {
		method, url = "GET", s.URL
	}

	step := draftStep{
		Kind:            "request",
		Name:            s.Name,
		Method:          method,
		URL:             url,
		Expect:          expectToText(s.Expect),
		Headers:         pairsFromMap(s.Headers),
		Query:           pairsFromMap(s.Query),
		Form:            []draftPair{},
		Capture:         pairsFromMap(s.Capture),
		BetweenRequests: s.BetweenRequests,
		Timeout:         durationOrBlank(s.Timeout),
		BodyKind:        "none",
	}

	switch {
	case s.JSON != nil:
		step.BodyKind = "json"
		step.Body = jsonBodyToText(s.JSON)
	case len(s.Form) > 0:
		step.BodyKind = "form"
		step.Form = pairsFromMap(s.Form)
	case s.Body != "":
		step.BodyKind = "raw"
		step.Body = s.Body
	}

	return step
}

// intOrBlank renders a count as the builder would type it: blank when it
// means "not set" rather than a literal, misleading zero.
func intOrBlank(v int) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(v)
}

func uintOrBlank(v uint64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatUint(v, 10)
}

func durationOrBlank(d scenario.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.Std().String()
}

// expectToText joins expected status codes the way the builder's own field
// reads them back: space or comma separated, either works.
func expectToText(codes []int) string {
	if len(codes) == 0 {
		return ""
	}
	out := make([]string, len(codes))
	for i, code := range codes {
		out[i] = strconv.Itoa(code)
	}
	return strings.Join(out, ", ")
}

// pairsFromMap turns a map into the builder's ordered key/value rows. Never
// nil: this is JSON-encoded straight to the browser, which maps over it
// unconditionally, and a nil Go slice marshals to `null`.
//
// Go map iteration order is random, which would make the same configuration
// render its headers or tags in a different order on every open — a
// distracting, purely cosmetic diff for something that changed nothing.
// Sorting by key keeps it stable.
func pairsFromMap(m map[string]string) []draftPair {
	if len(m) == 0 {
		return []draftPair{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]draftPair, 0, len(keys))
	for _, k := range keys {
		out = append(out, draftPair{Key: k, Value: m[k]})
	}
	return out
}

// jsonBodyToText renders a parsed JSON body back into the text the builder's
// JSON body field would show — compact enough to be recognisable, but the
// builder's own textarea reformats it further as it re-parses on the way
// back out, so this only needs to be valid JSON, not the prettiest kind.
func jsonBodyToText(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(encoded)
}
