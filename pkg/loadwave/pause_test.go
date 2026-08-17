// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave_test

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

func TestParsePause(t *testing.T) {
	t.Parallel()

	cases := map[string]loadwave.Pause{
		"1s":       loadwave.NewPause(time.Second),
		"500ms":    loadwave.NewPause(500 * time.Millisecond),
		"0":        {},
		"0s":       {},
		"1s-3s":    loadwave.NewPauseRange(time.Second, 3*time.Second),
		"200ms-1s": loadwave.NewPauseRange(200*time.Millisecond, time.Second),
		" 2s ":     loadwave.NewPause(2 * time.Second),
	}

	for spec, want := range cases {
		got, err := loadwave.ParsePause(spec)
		if err != nil {
			t.Fatalf("ParsePause(%q): %v", spec, err)
		}
		if got != want {
			t.Errorf("ParsePause(%q) = %+v, want %+v", spec, got, want)
		}
	}
}

func TestParsePauseRejectsNonsense(t *testing.T) {
	t.Parallel()

	// An empty string is an error rather than "no pause": only the caller
	// knows whether a blank field means unset or explicitly none.
	for _, bad := range []string{"", "   ", "soon", "-1s", "3s-1s", "1s-", "1s-nope"} {
		if _, err := loadwave.ParsePause(bad); err == nil {
			t.Errorf("ParsePause(%q) was accepted", bad)
		}
	}
}

func TestPauseDuration(t *testing.T) {
	t.Parallel()

	if got := loadwave.NewPause(time.Second).Duration(nil); got != time.Second {
		t.Errorf("fixed pause = %s", got)
	}
	if got := (loadwave.Pause{}).Duration(nil); got != 0 {
		t.Errorf("zero pause = %s", got)
	}

	// A range must actually vary, or virtual users march in lockstep and
	// produce synchronised bursts rather than a smooth arrival pattern.
	rnd := rand.New(rand.NewPCG(1, 2))
	span := loadwave.NewPauseRange(100*time.Millisecond, 400*time.Millisecond)

	seen := map[time.Duration]bool{}
	for range 200 {
		d := span.Duration(rnd)
		if d < 100*time.Millisecond || d >= 400*time.Millisecond {
			t.Fatalf("drew %s, outside the range", d)
		}
		seen[d] = true
	}
	if len(seen) < 20 {
		t.Errorf("only %d distinct values drawn from a range; barely varying", len(seen))
	}
}

// The whole point of the default: a scenario whose request fails instantly
// must not loop as fast as the CPU allows.
func TestPacingThrottlesAFailingRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "always broken", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	vu, _ := newTestVU(t, server.URL, loadwave.HTTPOptions{
		BetweenRequests: loadwave.NewPause(120 * time.Millisecond),
	})
	vu.BeginIteration(0)

	started := time.Now()
	for range 5 {
		// Every one of these returns a 500; the pause has to happen anyway.
		if _, err := vu.HTTP().Get(context.Background(), "/"); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	elapsed := time.Since(started)

	if calls.Load() != 5 {
		t.Fatalf("server saw %d calls, want 5", calls.Load())
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("five paced requests took %s; the pause was not applied on the failure path", elapsed)
	}
}

func TestPacingDefaultsToOneSecond(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	// An unconfigured run must be paced, not flat out.
	vu, _ := newTestVU(t, server.URL, loadwave.HTTPOptions{})
	vu.BeginIteration(0)

	started := time.Now()
	if _, err := vu.HTTP().Get(context.Background(), "/"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("one request took %s; expected the default one-second pause", elapsed)
	}
}

func TestPacingCanBeDisabled(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	// A throughput test needs flat out, and zero has to mean zero rather than
	// falling back to the default.
	vu, _ := newTestVU(t, server.URL, loadwave.HTTPOptions{NoBetweenRequests: true})
	vu.BeginIteration(0)

	started := time.Now()
	for range 5 {
		if _, err := vu.HTTP().Get(context.Background(), "/"); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("five unpaced requests took %s; pacing was not disabled", elapsed)
	}
}

func TestPacingPerRequestOverride(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	vu, _ := newTestVU(t, server.URL, loadwave.HTTPOptions{
		BetweenRequests: loadwave.NewPause(2 * time.Second),
	})
	vu.BeginIteration(0)

	// A pointer to the zero Pause means none, distinct from nil meaning
	// "use the run's setting".
	none := loadwave.Pause{}
	started := time.Now()
	if _, err := vu.HTTP().Do(context.Background(), loadwave.Request{
		URL:             "/",
		BetweenRequests: &none,
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("an override of none still paused for %s", elapsed)
	}
}

// Pacing is not work, so it must not inflate the iteration's duration.
func TestPacingIsExcludedFromIterationDuration(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	vu, recorder := newTestVU(t, server.URL, loadwave.HTTPOptions{
		BetweenRequests: loadwave.NewPause(400 * time.Millisecond),
	})
	vu.BeginIteration(0)

	started := time.Now()
	if _, err := vu.HTTP().Get(context.Background(), "/"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	vu.EndIteration(time.Since(started), nil)

	observed, ok := recorder.find(loadwave.MetricIterationDuration)
	if !ok {
		t.Fatal("no iteration_duration recorded")
	}
	if observed.value > 300 {
		t.Fatalf("iteration_duration = %.0fms; the 400ms pause was counted as work", observed.value)
	}
}

// A stopping run must not sit through everyone's pause.
func TestPacingIsInterruptible(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	vu, _ := newTestVU(t, server.URL, loadwave.HTTPOptions{
		BetweenRequests: loadwave.NewPause(30 * time.Second),
	})
	vu.BeginIteration(0)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := vu.HTTP().Get(ctx, "/"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("a cancelled context still waited out %s of pacing", elapsed)
	}
}
