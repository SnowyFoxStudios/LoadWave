// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
)

func rampingProfile(stages ...*loadwavev1.Stage) *loadwavev1.LoadProfile {
	return &loadwavev1.LoadProfile{
		Executor: loadwavev1.ExecutorType_EXECUTOR_TYPE_RAMPING_VUS,
		Stages:   stages,
	}
}

func stage(d time.Duration, target uint32) *loadwavev1.Stage {
	return &loadwavev1.Stage{Duration: durationpb.New(d), Target: target}
}

func TestConstantVUs(t *testing.T) {
	t.Parallel()

	executor, err := NewExecutor(&loadwavev1.LoadProfile{
		Executor: loadwavev1.ExecutorType_EXECUTOR_TYPE_CONSTANT_VUS,
		Vus:      50,
		Duration: durationpb.New(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	cases := []struct {
		at   time.Duration
		want int
	}{
		{-time.Second, 0},
		{0, 50},
		{15 * time.Second, 50},
		{29999 * time.Millisecond, 50},
		{30 * time.Second, 0}, // the profile is over
		{time.Minute, 0},
	}
	for _, tc := range cases {
		if got := executor.TargetAt(tc.at); got != tc.want {
			t.Errorf("TargetAt(%s) = %d, want %d", tc.at, got, tc.want)
		}
	}

	if executor.Peak() != 50 {
		t.Errorf("Peak() = %d", executor.Peak())
	}
	if executor.Duration() != 30*time.Second {
		t.Errorf("Duration() = %s", executor.Duration())
	}
}

func TestConstantVUsNeedsAnEnd(t *testing.T) {
	t.Parallel()

	// Neither a duration nor an iteration budget means the run would never
	// finish; that is a configuration mistake worth catching up front.
	_, err := NewExecutor(&loadwavev1.LoadProfile{
		Executor: loadwavev1.ExecutorType_EXECUTOR_TYPE_CONSTANT_VUS,
		Vus:      10,
	})
	if err == nil {
		t.Fatal("an unbounded constant-vus profile was accepted")
	}

	// With an iteration budget it is bounded, so it is allowed.
	if _, err := NewExecutor(&loadwavev1.LoadProfile{
		Executor:   loadwavev1.ExecutorType_EXECUTOR_TYPE_CONSTANT_VUS,
		Vus:        10,
		Iterations: 100,
	}); err != nil {
		t.Fatalf("an iteration-bounded profile was rejected: %v", err)
	}
}

func TestRampingVUsInterpolates(t *testing.T) {
	t.Parallel()

	// Up to 100 over 10s, hold 10s, down to 0 over 10s.
	executor, err := NewExecutor(rampingProfile(
		stage(10*time.Second, 100),
		stage(10*time.Second, 100),
		stage(10*time.Second, 0),
	))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	cases := []struct {
		at   time.Duration
		want int
	}{
		{0, 0}, // every profile starts from nothing
		{2500 * time.Millisecond, 25},
		{5 * time.Second, 50},
		{10 * time.Second, 100},
		{15 * time.Second, 100}, // holding
		{20 * time.Second, 100},
		{25 * time.Second, 50}, // ramping down
		{29 * time.Second, 10},
		{30 * time.Second, 0}, // finished
		{time.Minute, 0},
	}
	for _, tc := range cases {
		if got := executor.TargetAt(tc.at); got != tc.want {
			t.Errorf("TargetAt(%s) = %d, want %d", tc.at, got, tc.want)
		}
	}

	if executor.Peak() != 100 {
		t.Errorf("Peak() = %d", executor.Peak())
	}
	if executor.Duration() != 30*time.Second {
		t.Errorf("Duration() = %s", executor.Duration())
	}
}

func TestRampingVUsValidation(t *testing.T) {
	t.Parallel()

	if _, err := NewExecutor(rampingProfile()); err == nil {
		t.Error("a profile with no stages was accepted")
	}
	if _, err := NewExecutor(rampingProfile(stage(0, 10))); err == nil {
		t.Error("a zero-duration stage was accepted")
	}
	if _, err := NewExecutor(rampingProfile(stage(time.Second, 0))); err == nil {
		t.Error("a profile that never rises above zero was accepted")
	}
	if _, err := NewExecutor(nil); err == nil {
		t.Error("a nil profile was accepted")
	}
}

func TestScalePreservesShape(t *testing.T) {
	t.Parallel()

	base, err := NewExecutor(rampingProfile(
		stage(10*time.Second, 100),
		stage(10*time.Second, 100),
	))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	// A node given a quarter of the peak follows the same curve at a quarter
	// scale, so the fleet's aggregate reproduces the requested profile
	// without anyone pushing per-tick VU counts over the network.
	quarter := Scale(base, 25)

	if got := quarter.Peak(); got != 25 {
		t.Errorf("Peak() = %d, want 25", got)
	}
	if got := quarter.TargetAt(10 * time.Second); got != 25 {
		t.Errorf("at peak: %d, want 25", got)
	}
	if got := quarter.TargetAt(5 * time.Second); got != 13 {
		t.Errorf("at half ramp: %d, want 13 (half of 25, rounded)", got)
	}
	if got := quarter.TargetAt(20 * time.Second); got != 0 {
		t.Errorf("after the profile: %d, want 0", got)
	}
}

func TestScaleKeepsASmallNodeWorking(t *testing.T) {
	t.Parallel()

	base, err := NewExecutor(rampingProfile(stage(100*time.Second, 1000)))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	// A node holding one VU at peak must still carry it during the ramp.
	// Rounding its share to zero would leave the node idle throughout and
	// make the fleet's aggregate sag below the requested curve.
	tiny := Scale(base, 1)
	if got := tiny.TargetAt(time.Second); got != 1 {
		t.Errorf("a one-VU node was idle early in the ramp: %d", got)
	}
	if got := tiny.TargetAt(100 * time.Second); got != 0 {
		t.Errorf("after the profile: %d, want 0", got)
	}

	// A node with no quota stays at zero regardless.
	if got := Scale(base, 0).TargetAt(50 * time.Second); got != 0 {
		t.Errorf("a zero-quota node ran %d VUs", got)
	}
}

// The sum of the nodes' curves should track the requested one closely, which
// is the property the whole distribution scheme rests on.
func TestScaledNodesReproduceTheGlobalCurve(t *testing.T) {
	t.Parallel()

	base, err := NewExecutor(rampingProfile(
		stage(30*time.Second, 300),
		stage(30*time.Second, 300),
		stage(30*time.Second, 0),
	))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	// Three nodes of unequal capacity, summing to the peak.
	nodes := []Executor{Scale(base, 150), Scale(base, 100), Scale(base, 50)}

	for at := time.Duration(0); at < 90*time.Second; at += time.Second {
		want := base.TargetAt(at)

		total := 0
		for _, node := range nodes {
			total += node.TargetAt(at)
		}

		// Each node rounds independently, so a few VUs of drift is expected;
		// anything more would mean the fleet is not running the profile.
		if diff := total - want; diff < -3 || diff > 3 {
			t.Fatalf("at %s the fleet ran %d VUs, profile asks for %d", at, total, want)
		}
	}
}

func TestRescaleAppliesImmediatelyWithoutARamp(t *testing.T) {
	t.Parallel()

	base, err := NewExecutor(rampingProfile(stage(time.Minute, 100)))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	node := newScaled(base, 50)

	// A rebalance after an agent is lost must take effect now; the survivors
	// picking up the missing load "in a minute" means the run silently runs
	// under target until then.
	node.Rescale(100, 30*time.Second, 0)

	if got := node.Quota(30 * time.Second); got != 100 {
		t.Fatalf("quota = %d immediately after an unramped rescale, want 100", got)
	}
}

func TestRescaleEasesOverTheRamp(t *testing.T) {
	t.Parallel()

	base, err := NewExecutor(rampingProfile(stage(10*time.Minute, 100)))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	node := newScaled(base, 20)

	// Raise to 120 over a minute, starting a minute in.
	const start = time.Minute
	node.Rescale(120, start, time.Minute)

	cases := []struct {
		at   time.Duration
		want int
	}{
		{start, 20},                  // nothing yet
		{start + 15*time.Second, 45}, // a quarter of the way
		{start + 30*time.Second, 70}, // halfway
		{start + 45*time.Second, 95}, // three quarters
		{start + time.Minute, 120},   // arrived
		{start + 5*time.Minute, 120}, // and stays
	}
	for _, tc := range cases {
		if got := node.Quota(tc.at); got != tc.want {
			t.Errorf("quota at %s = %d, want %d", tc.at-start, got, tc.want)
		}
	}

	// The quota is a ceiling; the profile's own shape still applies beneath it.
	if got := node.Peak(); got != 120 {
		t.Errorf("Peak() = %d, want the value being ramped to", got)
	}
}

func TestRescaleMidRampEasesFromWhereItIs(t *testing.T) {
	t.Parallel()

	base, err := NewExecutor(rampingProfile(stage(10*time.Minute, 100)))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	node := newScaled(base, 0)

	node.Rescale(100, 0, time.Minute)
	midway := node.Quota(30 * time.Second)
	if midway < 45 || midway > 55 {
		t.Fatalf("halfway through the first ramp the quota is %d, want about 50", midway)
	}

	// Changing target mid-ramp must continue from the current value rather
	// than snapping back to where the first ramp began.
	node.Rescale(0, 30*time.Second, time.Minute)
	if got := node.Quota(30 * time.Second); got != midway {
		t.Fatalf("quota jumped to %d on rescale, want it to continue from %d", got, midway)
	}
	if got := node.Quota(60 * time.Second); got >= midway {
		t.Fatalf("quota = %d halfway through the second ramp, want it falling from %d", got, midway)
	}
	if got := node.Quota(90 * time.Second); got != 0 {
		t.Fatalf("quota = %d at the end of the second ramp, want 0", got)
	}
}

func TestRescaleIsSafeUnderConcurrency(t *testing.T) {
	t.Parallel()

	base, err := NewExecutor(rampingProfile(stage(time.Hour, 1000)))
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	node := newScaled(base, 100)

	// The control loop reads the quota every tick while a supervisor command
	// may be rewriting it.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := range 2000 {
			node.Rescale(i%500, time.Duration(i)*time.Millisecond, time.Second)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 2000 {
			_ = node.TargetAt(time.Duration(i) * time.Millisecond)
		}
	}()
	wg.Wait()
}
