// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

// MetricKind determines how observations for a metric are aggregated, both
// within a node and when deltas from many nodes are merged centrally.
type MetricKind uint8

const (
	// KindCounter accumulates a monotonically increasing total, such as the
	// number of requests issued. Merging adds.
	KindCounter MetricKind = iota + 1

	// KindGauge records a value that goes up and down, such as the number of
	// active virtual users. Merging adds across nodes at the same instant,
	// but never across instants.
	KindGauge

	// KindTrend records a distribution and reports percentiles. Merging is
	// done through an HDR histogram, which is why a p99 stays correct when a
	// run is spread over many machines.
	KindTrend

	// KindRate records the fraction of observations that were true, such as
	// the share of requests that failed. Merging adds both the numerator and
	// the denominator.
	KindRate
)

// String implements fmt.Stringer.
func (k MetricKind) String() string {
	switch k {
	case KindCounter:
		return "counter"
	case KindGauge:
		return "gauge"
	case KindTrend:
		return "trend"
	case KindRate:
		return "rate"
	default:
		return "unknown"
	}
}

// Built-in metric names.
//
// Scenarios are free to emit their own metrics alongside these, but the
// dashboard and the default threshold set are written against these names, so
// custom HTTP-like protocols are best served by reusing them.
const (
	// MetricIterations counts completed scenario iterations.
	MetricIterations = "iterations"
	// MetricIterationDuration is the wall time of one full iteration, in
	// milliseconds, excluding time the VU spent in Think.
	MetricIterationDuration = "iteration_duration"
	// MetricIterationFailed is the share of iterations that returned an error.
	MetricIterationFailed = "iteration_failed"
	// MetricVUs is the number of virtual users currently executing.
	MetricVUs = "vus"

	// MetricHTTPReqs counts HTTP requests issued.
	MetricHTTPReqs = "http_reqs"
	// MetricHTTPReqDuration is total request time in milliseconds, from the
	// start of the request to the last byte of the body.
	MetricHTTPReqDuration = "http_req_duration"
	// MetricHTTPReqWaiting is time to first byte in milliseconds: the server's
	// own think time, with connection setup and body transfer excluded.
	MetricHTTPReqWaiting = "http_req_waiting"
	// MetricHTTPReqConnecting is time spent establishing a TCP connection, in
	// milliseconds. Zero on a reused connection.
	MetricHTTPReqConnecting = "http_req_connecting"
	// MetricHTTPReqTLS is time spent on the TLS handshake, in milliseconds.
	MetricHTTPReqTLS = "http_req_tls_handshaking"
	// MetricHTTPReqFailed is the share of requests judged unsuccessful.
	MetricHTTPReqFailed = "http_req_failed"
	// MetricHTTPReqBytesIn counts response bytes read.
	MetricHTTPReqBytesIn = "http_req_bytes_in"
	// MetricHTTPReqBytesOut counts request bytes written.
	MetricHTTPReqBytesOut = "http_req_bytes_out"

	// MetricChecks is the share of checks that passed.
	MetricChecks = "checks"
	// MetricErrors counts errors reported by scenarios via VU.Fail.
	MetricErrors = "errors"
)

// Standard label keys. Sticking to these keeps scenario metrics legible in the
// dashboard, which groups and filters on them.
const (
	// LabelScenario is the name of the scenario that produced the observation.
	LabelScenario = "scenario"
	// LabelName is the call site's stable identity. For HTTP this is the URL
	// with high-cardinality path segments collapsed, so that /users/1 and
	// /users/2 aggregate into one series instead of two million.
	LabelName = "name"
	// LabelMethod is the HTTP method.
	LabelMethod = "method"
	// LabelStatus is the HTTP status code as a string, or "0" if the request
	// never produced a response.
	LabelStatus = "status"
	// LabelError is a short, bounded classification of a transport failure —
	// "timeout", "connection_refused" and the like. Never a raw error string,
	// which would blow up series cardinality.
	LabelError = "error"
	// LabelCheck is the name given to a check.
	LabelCheck = "check"
	// LabelExpected marks whether a failure was anticipated by the scenario.
	LabelExpected = "expected"
)

// Recorder is the sink a scenario writes observations to.
//
// The engine supplies the implementation; scenarios only ever consume it via
// VU.Metrics. It is defined here, in the public package, so that the engine
// can depend on the SDK rather than the other way round.
//
// Implementations must be safe for concurrent use: many virtual users share
// one Recorder.
type Recorder interface {
	// Count adds delta to a counter.
	Count(metric string, labels Labels, delta float64)
	// Trend records one observation in a distribution.
	Trend(metric string, labels Labels, value float64)
	// Rate records one boolean observation.
	Rate(metric string, labels Labels, ok bool)
	// Gauge sets the current value of a gauge.
	Gauge(metric string, labels Labels, value float64)
}

// nopRecorder discards everything. It backs VUs constructed outside a real
// run — most usefully in a scenario's own unit tests, where the author wants
// to exercise the logic without standing up an engine.
type nopRecorder struct{}

func (nopRecorder) Count(string, Labels, float64) {}
func (nopRecorder) Trend(string, Labels, float64) {}
func (nopRecorder) Rate(string, Labels, bool)     {}
func (nopRecorder) Gauge(string, Labels, float64) {}

// DiscardRecorder is a Recorder that drops every observation.
func DiscardRecorder() Recorder { return nopRecorder{} }
