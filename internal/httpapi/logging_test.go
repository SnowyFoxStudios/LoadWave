// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request path reaches the log, and a client controls it.
//
// `%0a` survives URL decoding into a real newline, which is enough to append a
// line of the client's choosing to the file an operator later reads as
// evidence of what the server did.
func TestLogPathCannotForgeALogLine(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/%0alevel=INFO%20msg=%22all%20clear%22", nil)
	if !strings.Contains(request.URL.Path, "\n") {
		t.Fatal("the premise no longer holds: the path carries no newline to strip")
	}

	logged := logPath(request)
	for _, forbidden := range []string{"\n", "\r", "\x1b"} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("logged path %q still carries a control character", logged)
		}
	}
	// The probe should still be visible as a probe; sanitising is not hiding.
	if !strings.Contains(logged, "all clear") {
		t.Errorf("logged path %q dropped the attempt instead of defusing it", logged)
	}
}

func TestLogPathIsBounded(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/"+strings.Repeat("a", 4000), nil)

	logged := logPath(request)
	if len(logged) > maxLoggedPath+8 {
		t.Errorf("logged path is %d bytes; a client should not be able to flood the log", len(logged))
	}
	if !strings.HasSuffix(logged, "…") {
		t.Error("a truncated path should say that it was truncated")
	}
}

func TestLogPathLeavesAnOrdinaryPathAlone(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-20260817-120000-abc123", nil)
	if got := logPath(request); got != "/api/v1/runs/run-20260817-120000-abc123" {
		t.Errorf("logPath mangled an ordinary path: %q", got)
	}
}
