// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

import (
	"strings"
	"unicode"
)

// MaxFailureMessage bounds how much of a response body or error string is kept
// as a failure excerpt.
//
// This is a hint for a human reading the dashboard, not a payload. A server
// that answers a load test with a stack trace should not be able to turn the
// control plane into a log shipper.
const MaxFailureMessage = 240

// Failure describes one request that did not succeed.
//
// Every field except Message is bounded by construction: Name is already
// collapsed to low cardinality, and ErrorClass comes from a fixed vocabulary.
// That is what lets failures be aggregated rather than streamed.
type Failure struct {
	// Name is the request's metric name.
	Name string
	// Method is the HTTP method.
	Method string
	// Status is the HTTP status, or 0 when no response arrived.
	Status int
	// ErrorClass is the transport failure classification, empty when a
	// response was received.
	ErrorClass string
	// Message is a short excerpt of what went wrong: the response body, or
	// the transport error's text.
	Message string
}

// FailureReporter receives details of failed requests.
//
// It is a separate, optional interface rather than part of Recorder because a
// failure is not a number: it carries text, it is aggregated differently, and
// most Recorder implementations have no use for it. A recorder that does not
// implement this simply receives no samples.
type FailureReporter interface {
	ReportFailure(Failure)
}

// reportFailure hands a failure to the recorder, if it wants them.
func (c *HTTPClient) reportFailure(name string, req *Request, resp *Response) {
	reporter, ok := c.vu.Metrics().(FailureReporter)
	if !ok {
		return
	}

	failure := Failure{
		Name:   name,
		Method: req.Method,
		Status: resp.StatusCode,
	}
	if resp.Err != nil {
		failure.ErrorClass = classifyError(resp.Err)
		failure.Message = TruncateMessage(resp.Err.Error())
	} else {
		failure.Message = TruncateMessage(string(resp.Body))
	}

	reporter.ReportFailure(failure)
}

// TruncateMessage reduces arbitrary text to a short single-line excerpt.
//
// Response bodies are HTML pages, JSON documents and stack traces; rendered
// verbatim they would wreck the table they appear in. Collapsing whitespace
// and clipping to a fixed length keeps a row readable while preserving the
// part that usually identifies the problem, which is nearly always at the
// front.
func TruncateMessage(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(min(len(trimmed), MaxFailureMessage))

	space := false
	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		// Control characters in a terminal or a table cell are worse than
		// useless, and a body can contain anything.
		if !unicode.IsPrint(r) {
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false

		if b.Len() >= MaxFailureMessage {
			return b.String() + "…"
		}
		b.WriteRune(r)
	}
	return b.String()
}
