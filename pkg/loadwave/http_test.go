// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

func TestDeriveRequestName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want string
	}{
		// The whole purpose is bounding cardinality: an id in the path must
		// collapse, or a run against /orders/{id} produces one series per
		// order and the coordinator runs out of memory.
		{"/users/42", "GET /users/*"},
		{"/users/42/orders/7", "GET /users/*/orders/*"},
		{"/items/a3f9c1e07b42", "GET /items/*"},
		{"/o/f47ac10b-58cc-4372-a567-0e02b2c3d479", "GET /o/*"},
		{"/session/abc123def456ghi789", "GET /session/*"},

		// Genuine path segments must survive, or every endpoint merges into
		// one unreadable row.
		{"/api/products", "GET /api/products"},
		{"/v2/health", "GET /v2/health"},
		{"/", "GET /"},
		{"", "GET /"},
		{"/abc", "GET /abc"},
		{"/deadbeef", "GET /deadbeef"}, // hex but under the length floor
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			if got := loadwave.DeriveRequestName(http.MethodGet, tc.path); got != tc.want {
				t.Fatalf("DeriveRequestName(GET, %q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// recordedObservation is one metric write captured by the fake recorder.
type recordedObservation struct {
	metric string
	labels loadwave.Labels
	value  float64
	truthy bool
}

// captureRecorder records every observation for assertion.
type captureRecorder struct {
	observations []recordedObservation
}

func (c *captureRecorder) Count(metric string, labels loadwave.Labels, delta float64) {
	c.observations = append(c.observations, recordedObservation{metric, labels, delta, false})
}

func (c *captureRecorder) Trend(metric string, labels loadwave.Labels, value float64) {
	c.observations = append(c.observations, recordedObservation{metric, labels, value, false})
}

func (c *captureRecorder) Rate(metric string, labels loadwave.Labels, ok bool) {
	c.observations = append(c.observations, recordedObservation{metric, labels, 0, ok})
}

func (c *captureRecorder) Gauge(metric string, labels loadwave.Labels, value float64) {
	c.observations = append(c.observations, recordedObservation{metric, labels, value, false})
}

func (c *captureRecorder) find(metric string) (recordedObservation, bool) {
	for _, o := range c.observations {
		if o.metric == metric {
			return o, true
		}
	}
	return recordedObservation{}, false
}

// newTestVU wires a VU to a real HTTP client pointed at a test server.
func newTestVU(t *testing.T, baseURL string, opts loadwave.HTTPOptions) (*loadwave.VU, *captureRecorder) {
	t.Helper()

	opts.BaseURL = baseURL
	factory, err := loadwave.NewHTTPClientFactory(opts)
	if err != nil {
		t.Fatalf("NewHTTPClientFactory: %v", err)
	}
	t.Cleanup(factory.Close)

	recorder := &captureRecorder{}
	vu := loadwave.NewVU(loadwave.VUConfig{
		ID:       1,
		Scenario: "test",
		Recorder: recorder,
		HTTP:     factory.New(),
	})
	t.Cleanup(func() { _ = vu.Close() })
	return vu, recorder
}

func TestHTTPClientRecordsTheStandardMetrics(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"name":"widget"}`))
	}))
	t.Cleanup(server.Close)

	vu, recorder := newTestVU(t, server.URL, loadwave.HTTPOptions{})
	vu.BeginIteration(0)

	resp, err := vu.HTTP().Get(context.Background(), "/api/things/42")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	for _, metric := range []string{
		loadwave.MetricHTTPReqs,
		loadwave.MetricHTTPReqDuration,
		loadwave.MetricHTTPReqFailed,
		loadwave.MetricHTTPReqBytesIn,
		loadwave.MetricHTTPReqBytesOut,
	} {
		if _, ok := recorder.find(metric); !ok {
			t.Errorf("no observation for %s", metric)
		}
	}

	reqs, _ := recorder.find(loadwave.MetricHTTPReqs)
	if name, _ := reqs.labels.Get(loadwave.LabelName); name != "GET /api/things/*" {
		t.Errorf("name label = %q, want the collapsed path", name)
	}
	if status, _ := reqs.labels.Get(loadwave.LabelStatus); status != "200" {
		t.Errorf("status label = %q", status)
	}
	if scenario, _ := reqs.labels.Get(loadwave.LabelScenario); scenario != "test" {
		t.Errorf("scenario label = %q", scenario)
	}

	failed, _ := recorder.find(loadwave.MetricHTTPReqFailed)
	if failed.truthy {
		t.Error("a 200 was recorded as a failure")
	}

	var decoded struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := resp.JSON(&decoded); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if decoded.ID != 7 || decoded.Name != "widget" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestHTTPClientTreatsServerErrorsAsMeasurements(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	vu, recorder := newTestVU(t, server.URL, loadwave.HTTPOptions{})
	vu.BeginIteration(0)

	// A 500 is a successful exchange with a bad status, not a transport
	// error: the scenario must be able to see it rather than have it thrown.
	resp, err := vu.HTTP().Get(context.Background(), "/")
	if err != nil {
		t.Fatalf("a 500 should not be returned as an error, got %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.OK() {
		t.Error("OK() should be false for a 500")
	}

	failed, ok := recorder.find(loadwave.MetricHTTPReqFailed)
	if !ok || !failed.truthy {
		t.Error("a 500 should count toward the failure rate")
	}
}

func TestHTTPClientExpectStatusOverridesSuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	vu, recorder := newTestVU(t, server.URL, loadwave.HTTPOptions{})
	vu.BeginIteration(0)

	// A scenario probing for absence should not have its own expectation
	// counted as a failure.
	_, err := vu.HTTP().Do(context.Background(), loadwave.Request{
		URL:          "/missing",
		ExpectStatus: []int{http.StatusNotFound},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	failed, _ := recorder.find(loadwave.MetricHTTPReqFailed)
	if failed.truthy {
		t.Error("an expected 404 was counted as a failure")
	}
}

func TestHTTPClientClassifiesTransportFailures(t *testing.T) {
	t.Parallel()

	// A port nothing is listening on gives a deterministic connection error.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := closed.URL
	closed.Close()

	vu, recorder := newTestVU(t, address, loadwave.HTTPOptions{Timeout: 2 * time.Second})
	vu.BeginIteration(0)

	resp, err := vu.HTTP().Get(context.Background(), "/")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if resp == nil {
		t.Fatal("Response must be non-nil even on a transport error")
	}
	if resp.StatusCode != 0 {
		t.Fatalf("status = %d, want 0 when no response arrived", resp.StatusCode)
	}

	reqs, ok := recorder.find(loadwave.MetricHTTPReqs)
	if !ok {
		t.Fatal("a failed request must still be counted")
	}
	// The label must be from the bounded vocabulary, never the raw error, or
	// every distinct failure creates its own time series.
	class, has := reqs.labels.Get(loadwave.LabelError)
	if !has {
		t.Fatal("no error label")
	}
	switch class {
	case "connection_refused", "timeout", "eof", "connection_reset":
	default:
		t.Fatalf("unexpected error classification %q", class)
	}
}

func TestHTTPClientTruncatesOversizedBodies(t *testing.T) {
	t.Parallel()

	const bodySize = 4096
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, bodySize))
	}))
	t.Cleanup(server.Close)

	vu, _ := newTestVU(t, server.URL, loadwave.HTTPOptions{MaxBodyBytes: 100})
	vu.BeginIteration(0)

	resp, err := vu.HTTP().Get(context.Background(), "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(resp.Body) != 100 {
		t.Fatalf("buffered %d bytes, want the cap of 100", len(resp.Body))
	}
	if !resp.Truncated {
		t.Error("Truncated should be set")
	}
	// Byte accounting must reflect what crossed the wire, not what was kept,
	// or throughput silently understates the load being generated.
	if resp.BytesIn != bodySize {
		t.Errorf("BytesIn = %d, want %d", resp.BytesIn, bodySize)
	}
}

func TestHTTPClientSendsJSONBodies(t *testing.T) {
	t.Parallel()

	type payload struct {
		Name string `json:"name"`
	}

	received := make(chan payload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body payload
		_ = json.NewDecoder(r.Body).Decode(&body)
		received <- body
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	vu, _ := newTestVU(t, server.URL, loadwave.HTTPOptions{})
	vu.BeginIteration(0)

	resp, err := vu.HTTP().PostJSON(context.Background(), "/things", payload{Name: "widget"})
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := <-received; got.Name != "widget" {
		t.Fatalf("server received %+v", got)
	}
}

func TestHTTPClientRejectsRelativeURLWithoutBase(t *testing.T) {
	t.Parallel()

	factory, err := loadwave.NewHTTPClientFactory(loadwave.HTTPOptions{})
	if err != nil {
		t.Fatalf("NewHTTPClientFactory: %v", err)
	}
	t.Cleanup(factory.Close)

	vu := loadwave.NewVU(loadwave.VUConfig{ID: 1, HTTP: factory.New()})
	if _, err := vu.HTTP().Get(context.Background(), "/relative"); err == nil {
		t.Fatal("expected an error for a relative URL with no base")
	}
}

func TestNewHTTPClientFactoryRejectsBadBaseURL(t *testing.T) {
	t.Parallel()

	for _, base := range []string{"not-a-url", "/just/a/path", "://broken"} {
		if _, err := loadwave.NewHTTPClientFactory(loadwave.HTTPOptions{BaseURL: base}); err == nil {
			t.Errorf("base URL %q was accepted", base)
		}
	}
}
