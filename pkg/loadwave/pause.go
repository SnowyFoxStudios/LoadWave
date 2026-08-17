// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// DefaultBetweenRequests is the pause inserted after every request when a run
// does not say otherwise.
//
// It is deliberately not zero. A scenario with no explicit think time will
// otherwise loop as fast as the network allows, and a scenario whose first
// request fails instantly — a refused connection, a 500 returned from a cache —
// loops as fast as the CPU allows. Both bury the system under test in traffic
// no real population would generate, and the second is how a load test turns
// into an accidental denial of service against a service that is already down.
//
// One second is roughly a real person's pace and easy to reason about. Set it
// to zero explicitly for a throughput test, where flat out is the point.
const DefaultBetweenRequests = time.Second

// Pause is a delay, optionally drawn uniformly from a range.
//
// The zero Pause means no delay.
type Pause struct {
	Min time.Duration
	Max time.Duration
}

// NewPause returns a fixed-length pause.
func NewPause(d time.Duration) Pause { return Pause{Min: d, Max: d} }

// NewPauseRange returns a pause drawn uniformly from [minDur, maxDur].
//
// Prefer a range over a fixed value. Identical pauses make every virtual user
// march in lockstep, which produces traffic in synchronised bursts rather than
// the smooth arrival pattern a real population generates — and the bursts are
// what your service ends up being measured against.
func NewPauseRange(minDur, maxDur time.Duration) Pause {
	if maxDur < minDur {
		minDur, maxDur = maxDur, minDur
	}
	return Pause{Min: minDur, Max: maxDur}
}

// IsZero reports whether the pause is no delay at all.
func (p Pause) IsZero() bool { return p.Min <= 0 && p.Max <= 0 }

// Duration draws a delay from the pause.
func (p Pause) Duration(rnd *rand.Rand) time.Duration {
	if p.Max <= p.Min {
		return max(p.Min, 0)
	}
	low := max(p.Min, 0)
	if rnd == nil {
		return low + (p.Max-low)/2
	}
	return low + time.Duration(rnd.Int64N(int64(p.Max-low)))
}

// String renders the pause the way it is written in configuration.
func (p Pause) String() string {
	switch {
	case p.IsZero():
		return "0s"
	case p.Max <= p.Min:
		return p.Min.String()
	default:
		return p.Min.String() + "-" + p.Max.String()
	}
}

// ParsePause reads a fixed delay or a range: "500ms", "1s", "1s-3s".
//
// An empty string is an error rather than a zero pause: at every call site the
// difference between "not specified" and "explicitly none" matters, and only
// the caller knows which an empty field means.
func ParsePause(spec string) (Pause, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return Pause{}, fmt.Errorf("pause is empty; write a duration such as %q or a range such as %q", "1s", "1s-3s")
	}

	// Split on the last '-' so a bare negative duration still fails loudly
	// rather than parsing as a range with an empty lower bound.
	if idx := strings.LastIndex(trimmed, "-"); idx > 0 {
		low, errLow := time.ParseDuration(strings.TrimSpace(trimmed[:idx]))
		high, errHigh := time.ParseDuration(strings.TrimSpace(trimmed[idx+1:]))
		if errLow == nil && errHigh == nil {
			if low < 0 || high < low {
				return Pause{}, fmt.Errorf("pause range %q must be ascending and non-negative", spec)
			}
			return Pause{Min: low, Max: high}, nil
		}
	}

	fixed, err := time.ParseDuration(trimmed)
	if err != nil {
		return Pause{}, fmt.Errorf("invalid pause %q: expected a duration such as %q or a range such as %q", spec, "1s", "1s-3s")
	}
	if fixed < 0 {
		return Pause{}, fmt.Errorf("pause %q cannot be negative", spec)
	}
	return NewPause(fixed), nil
}
