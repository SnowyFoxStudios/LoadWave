// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

import (
	"hash/maphash"
	"slices"
	"strings"
)

// labelSeed keeps hashing consistent for the lifetime of the process. It does
// not need to be stable across processes: hashes are only ever used to look up
// series inside one node's local aggregator, and equality is always confirmed
// against the actual pairs before two series are treated as the same.
var labelSeed = maphash.MakeSeed()

// Labels is an immutable, pre-hashed set of metric tags.
//
// Metric tags are attached to every single observation, so building them has
// to be cheap. Labels are meant to be constructed once — when a scenario is
// set up, or when an HTTP call site is first seen — and then reused for the
// millions of samples that follow. Because the value is immutable, the hash
// can be computed once at construction and reused for every map lookup.
//
// The zero Labels is valid and carries no tags.
type Labels struct {
	// pairs holds key/value pairs flattened and sorted by key: k0,v0,k1,v1.
	// Sorting makes equality and hashing independent of construction order.
	pairs []string
	hash  uint64
}

// NewLabels builds a Labels from alternating key/value arguments.
//
// It panics if given an odd number of arguments, because that is always a
// programming error at a call site rather than a runtime condition worth
// propagating. Later duplicate keys overwrite earlier ones.
func NewLabels(kv ...string) Labels {
	if len(kv)%2 != 0 {
		panic("loadwave: NewLabels requires an even number of arguments")
	}
	if len(kv) == 0 {
		return Labels{}
	}
	return buildLabels(nil, kv)
}

// With returns a copy of l with the given key/value pairs added or replaced.
// The receiver is never modified, so a Labels value may be shared freely
// across virtual users without synchronisation.
func (l Labels) With(kv ...string) Labels {
	if len(kv)%2 != 0 {
		panic("loadwave: Labels.With requires an even number of arguments")
	}
	if len(kv) == 0 {
		return l
	}
	return buildLabels(l.pairs, kv)
}

// buildLabels merges `extra` over `base` and returns a fully-built Labels.
func buildLabels(base, extra []string) Labels {
	merged := make([]string, 0, len(base)+len(extra))
	merged = append(merged, base...)

	for i := 0; i < len(extra); i += 2 {
		k, v := extra[i], extra[i+1]
		if replaced := replacePair(merged, k, v); !replaced {
			merged = append(merged, k, v)
		}
	}

	sortPairs(merged)

	var h maphash.Hash
	h.SetSeed(labelSeed)
	for _, s := range merged {
		_, _ = h.WriteString(s)
		// A separator prevents {"ab":"c"} and {"a":"bc"} colliding.
		_ = h.WriteByte(0)
	}

	return Labels{pairs: merged, hash: h.Sum64()}
}

// replacePair overwrites the value for key k in place, reporting whether the
// key was present.
func replacePair(pairs []string, k, v string) bool {
	for i := 0; i < len(pairs); i += 2 {
		if pairs[i] == k {
			pairs[i+1] = v
			return true
		}
	}
	return false
}

// sortPairs sorts a flattened key/value slice by key, keeping each value
// adjacent to its key. Label sets are tiny — a handful of pairs at most — so
// an insertion sort beats anything that allocates.
func sortPairs(pairs []string) {
	for i := 2; i < len(pairs); i += 2 {
		k, v := pairs[i], pairs[i+1]
		j := i - 2
		for j >= 0 && pairs[j] > k {
			pairs[j+2], pairs[j+3] = pairs[j], pairs[j+1]
			j -= 2
		}
		pairs[j+2], pairs[j+3] = k, v
	}
}

// Hash returns the precomputed hash of the label set. Callers must still
// compare with Equal before treating two label sets as identical.
func (l Labels) Hash() uint64 { return l.hash }

// Len reports how many key/value pairs the set holds.
func (l Labels) Len() int { return len(l.pairs) / 2 }

// Get returns the value for a key and whether it was present.
func (l Labels) Get(key string) (string, bool) {
	for i := 0; i < len(l.pairs); i += 2 {
		if l.pairs[i] == key {
			return l.pairs[i+1], true
		}
	}
	return "", false
}

// Equal reports whether two label sets carry exactly the same pairs.
func (l Labels) Equal(other Labels) bool {
	return l.hash == other.hash && slices.Equal(l.pairs, other.pairs)
}

// All iterates the pairs in key order.
func (l Labels) All(yield func(key, value string) bool) {
	for i := 0; i < len(l.pairs); i += 2 {
		if !yield(l.pairs[i], l.pairs[i+1]) {
			return
		}
	}
}

// Map materialises the labels as a map. It allocates, so it belongs on
// reporting paths — serialising a batch, rendering the UI — never on the
// per-request hot path.
func (l Labels) Map() map[string]string {
	if len(l.pairs) == 0 {
		return nil
	}
	m := make(map[string]string, len(l.pairs)/2)
	for i := 0; i < len(l.pairs); i += 2 {
		m[l.pairs[i]] = l.pairs[i+1]
	}
	return m
}

// LabelsFromMap builds a Labels from a map, for use at boundaries where tags
// arrive as maps — configuration files and protobuf messages.
func LabelsFromMap(m map[string]string) Labels {
	if len(m) == 0 {
		return Labels{}
	}
	kv := make([]string, 0, len(m)*2)
	for k, v := range m {
		kv = append(kv, k, v)
	}
	return NewLabels(kv...)
}

// String renders the labels as `k=v,k=v` in key order. Intended for logs and
// test failure messages.
func (l Labels) String() string {
	if len(l.pairs) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(l.pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.pairs[i])
		b.WriteByte('=')
		b.WriteString(l.pairs[i+1])
	}
	return b.String()
}
