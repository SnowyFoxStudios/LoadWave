// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
)

// Executor turns a load profile into a virtual user count at any point in a
// run.
//
// It is a pure function of elapsed time, deliberately. Every node evaluates
// the same profile against the same agreed start instant and arrives at the
// same curve, so ramping needs no per-tick coordination and a node that misses
// messages for a few seconds rejoins the curve exactly where it should be
// rather than where it left off.
type Executor interface {
	// TargetAt returns the desired virtual user count at an elapsed offset
	// from the run's start.
	TargetAt(elapsed time.Duration) int

	// Duration is how long the profile runs. Zero means unbounded, and the
	// run ends only on an iteration cap or an explicit stop.
	Duration() time.Duration

	// Peak is the highest virtual user count the profile ever reaches.
	Peak() int

	// Describe renders the profile for logs and the dashboard.
	Describe() string
}

// NewExecutor builds the executor described by a load profile.
func NewExecutor(profile *loadwavev1.LoadProfile) (Executor, error) {
	if profile == nil {
		return nil, errors.New("load profile is missing")
	}

	switch profile.GetExecutor() {
	case loadwavev1.ExecutorType_EXECUTOR_TYPE_CONSTANT_VUS,
		loadwavev1.ExecutorType_EXECUTOR_TYPE_UNSPECIFIED:
		return newConstantVUs(profile)
	case loadwavev1.ExecutorType_EXECUTOR_TYPE_RAMPING_VUS:
		return newRampingVUs(profile)
	default:
		return nil, fmt.Errorf("unknown executor type %s", profile.GetExecutor())
	}
}

// constantVUs holds a fixed number of virtual users for a fixed time.
type constantVUs struct {
	vus      int
	duration time.Duration
}

func newConstantVUs(profile *loadwavev1.LoadProfile) (Executor, error) {
	vus := int(profile.GetVus())
	if vus <= 0 {
		return nil, fmt.Errorf("constant-vus profile needs a positive vus value, got %d", vus)
	}

	duration := profile.GetDuration().AsDuration()
	if duration < 0 {
		return nil, fmt.Errorf("constant-vus profile has a negative duration %s", duration)
	}
	// A profile with neither a duration nor an iteration cap would never end.
	if duration == 0 && profile.GetIterations() == 0 {
		return nil, errors.New("constant-vus profile needs either a duration or an iteration count")
	}

	return &constantVUs{vus: vus, duration: duration}, nil
}

func (c *constantVUs) TargetAt(elapsed time.Duration) int {
	if elapsed < 0 {
		return 0
	}
	if c.duration > 0 && elapsed >= c.duration {
		return 0
	}
	return c.vus
}

func (c *constantVUs) Duration() time.Duration { return c.duration }
func (c *constantVUs) Peak() int               { return c.vus }

func (c *constantVUs) Describe() string {
	if c.duration == 0 {
		return vuCount(c.vus) + " until the iteration budget is spent"
	}
	return fmt.Sprintf("%s for %s", vuCount(c.vus), c.duration)
}

// vuCount renders a virtual user count for people to read. The singular case
// is common enough — a one-VU smoke test — that "1 VUs" would show up in the
// dashboard, the builder and every report of one.
func vuCount(vus int) string {
	if vus == 1 {
		return "1 VU"
	}
	return fmt.Sprintf("%d VUs", vus)
}

// leg is one stage of a ramping profile, resolved to an absolute end offset.
type leg struct {
	end    time.Duration
	target int
}

// rampingVUs interpolates the virtual user count between stage targets.
type rampingVUs struct {
	legs  []leg
	total time.Duration
	peak  int
}

func newRampingVUs(profile *loadwavev1.LoadProfile) (Executor, error) {
	stages := profile.GetStages()
	if len(stages) == 0 {
		return nil, errors.New("ramping-vus profile needs at least one stage")
	}

	r := &rampingVUs{legs: make([]leg, 0, len(stages))}
	for i, stage := range stages {
		d := stage.GetDuration().AsDuration()
		if d <= 0 {
			return nil, fmt.Errorf("stage %d needs a positive duration, got %s", i+1, d)
		}
		r.total += d

		target := int(stage.GetTarget())
		r.legs = append(r.legs, leg{end: r.total, target: target})
		if target > r.peak {
			r.peak = target
		}
	}

	if r.peak == 0 {
		return nil, errors.New("ramping-vus profile never rises above zero VUs")
	}
	return r, nil
}

func (r *rampingVUs) TargetAt(elapsed time.Duration) int {
	if elapsed < 0 {
		return 0
	}
	if elapsed >= r.total {
		return 0
	}

	// Every profile begins at zero VUs, so the first stage ramps up from
	// nothing rather than starting at its own target.
	from := 0
	start := time.Duration(0)

	for _, l := range r.legs {
		if elapsed < l.end {
			span := l.end - start
			progress := float64(elapsed-start) / float64(span)
			return int(math.Round(float64(from) + progress*float64(l.target-from)))
		}
		from = l.target
		start = l.end
	}
	return 0
}

func (r *rampingVUs) Duration() time.Duration { return r.total }
func (r *rampingVUs) Peak() int               { return r.peak }

func (r *rampingVUs) Describe() string {
	parts := make([]string, 0, len(r.legs))
	prev := time.Duration(0)
	for _, l := range r.legs {
		parts = append(parts, fmt.Sprintf("%s to %s", l.end-prev, vuCount(l.target)))
		prev = l.end
	}
	return strings.Join(parts, ", then ")
}

// scaled reduces another executor's curve to the share one node is
// responsible for.
//
// The coordinator tells a node how many VUs it owns at the profile's peak, and
// the node follows the same shape scaled to that ceiling. Doing it this way
// rather than pushing a VU count on every tick means the ramp stays smooth
// across a network hiccup, and the control plane stays quiet: quotas change
// only when the fleet does.
//
// The quota itself can also move. An operator raising the target mid-run
// usually wants the new users introduced over a period rather than all in one
// tick — spawning several hundred at once measures how the service copes with
// a thundering herd, not how it copes with the load level that was asked for.
type scaled struct {
	inner Executor
	peak  int

	// mu guards the quota, which the control loop reads on every tick while a
	// supervisor command may be rewriting it.
	mu sync.RWMutex

	// from and to bracket an in-progress quota change; rampFrom and rampTo
	// bracket it in elapsed time. Keeping the ramp in elapsed rather than
	// wall-clock terms preserves the property that the whole curve is a pure
	// function of how long the run has been going.
	from     int
	to       int
	rampFrom time.Duration
	rampTo   time.Duration
}

// Scale wraps an executor so that its peak becomes quota, preserving shape.
func Scale(inner Executor, quota int) Executor { return newScaled(inner, quota) }

// newScaled is Scale, returning the concrete type so the engine can rescale it.
func newScaled(inner Executor, quota int) *scaled {
	if quota < 0 {
		quota = 0
	}
	return &scaled{inner: inner, peak: inner.Peak(), from: quota, to: quota}
}

// Rescale moves the quota to a new value.
//
// A zero ramp applies it at once, which is what a rebalance after a lost agent
// needs: the survivors must pick up the missing load now. A non-zero ramp eases
// there from wherever the quota currently is, starting at the given elapsed
// offset.
func (s *scaled) Rescale(quota int, at, ramp time.Duration) {
	if quota < 0 {
		quota = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if ramp <= 0 {
		s.from, s.to = quota, quota
		s.rampFrom, s.rampTo = 0, 0
		return
	}

	// Ease from where the quota actually is now, not from where the previous
	// ramp was heading, so two changes in quick succession do not jump.
	s.from = s.quotaAtLocked(at)
	s.to = quota
	s.rampFrom = at
	s.rampTo = at + ramp
}

// Quota reports the node's ceiling at an elapsed offset.
func (s *scaled) Quota(at time.Duration) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quotaAtLocked(at)
}

// quotaAtLocked interpolates an in-progress quota change. Callers hold s.mu.
func (s *scaled) quotaAtLocked(at time.Duration) int {
	if s.rampTo <= s.rampFrom || at >= s.rampTo {
		return s.to
	}
	if at <= s.rampFrom {
		return s.from
	}

	progress := float64(at-s.rampFrom) / float64(s.rampTo-s.rampFrom)
	return int(math.Round(float64(s.from) + progress*float64(s.to-s.from)))
}

func (s *scaled) TargetAt(elapsed time.Duration) int {
	quota := s.Quota(elapsed)
	if quota == 0 || s.peak == 0 {
		return 0
	}

	global := s.inner.TargetAt(elapsed)
	if global <= 0 {
		return 0
	}
	if global >= s.peak {
		return quota
	}

	scaledTarget := int(math.Round(float64(quota) * float64(global) / float64(s.peak)))

	// A node with any quota at all should carry at least one VU while the
	// profile is above zero. Rounding a small share to nothing would leave
	// the node idle through the whole ramp and make the fleet's aggregate
	// curve sag below the requested one.
	if scaledTarget == 0 {
		return 1
	}
	return scaledTarget
}

func (s *scaled) Duration() time.Duration { return s.inner.Duration() }

// Peak reports the quota this node is heading for.
func (s *scaled) Peak() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.to
}

func (s *scaled) Describe() string {
	s.mu.RLock()
	to, peak := s.to, s.peak
	s.mu.RUnlock()

	return fmt.Sprintf("%s (this node carries %d of %d VUs at peak)", s.inner.Describe(), to, peak)
}
