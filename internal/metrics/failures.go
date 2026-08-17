// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// DefaultMaxFailureKinds bounds how many distinct kinds of failure a node or a
// run will track.
//
// Kinds, not occurrences: the count of each is unbounded. A run failing in
// more than a hundred distinguishable ways has a problem the hundred-and-first
// row was not going to explain.
const DefaultMaxFailureKinds = 100

// failureKey identifies a kind of failure by its bounded fields only.
//
// The message is deliberately not part of the key. Bodies vary — a request id,
// a timestamp, a row count — and keying on them would make every failure its
// own row and defeat the cap.
type failureKey struct {
	name       string
	method     string
	status     int32
	errorClass string
}

// failureEntry accumulates one kind of failure.
type failureEntry struct {
	key      failureKey
	message  string
	count    uint64
	lastSeen time.Time
}

// failureTable aggregates failures under a cap. Safe for concurrent use.
type failureTable struct {
	mu      sync.Mutex
	entries map[failureKey]*failureEntry
	limit   int
	dropped uint64
}

func newFailureTable(limit int) *failureTable {
	if limit <= 0 {
		limit = DefaultMaxFailureKinds
	}
	return &failureTable{entries: make(map[failureKey]*failureEntry), limit: limit}
}

// record folds one failure in.
//
// The message is captured only when the kind is first seen. A run in which
// every request fails would otherwise spend its time copying and truncating
// strings on the hot path, and the second occurrence's body almost never says
// anything the first one did not.
func (t *failureTable) record(f loadwave.Failure, now time.Time) {
	key := failureKey{
		name:       f.Name,
		method:     f.Method,
		status:     int32(f.Status),
		errorClass: f.ErrorClass,
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if entry, ok := t.entries[key]; ok {
		entry.count++
		entry.lastSeen = now
		return
	}
	if len(t.entries) >= t.limit {
		t.dropped++
		return
	}
	t.entries[key] = &failureEntry{key: key, message: f.Message, count: 1, lastSeen: now}
}

// drain returns the accumulated failures and empties the table.
func (t *failureTable) drain() []*loadwavev1.FailureSample {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.entries) == 0 {
		return nil
	}

	out := make([]*loadwavev1.FailureSample, 0, len(t.entries))
	for key, entry := range t.entries {
		out = append(out, &loadwavev1.FailureSample{
			Name:       key.name,
			Method:     key.method,
			Status:     key.status,
			ErrorClass: key.errorClass,
			Message:    entry.message,
			Count:      entry.count,
			LastSeen:   timestamppb.New(entry.lastSeen),
		})
	}

	// Cleared rather than reset in place: a kind of failure that has stopped
	// happening should stop being reported, and the map is small.
	clear(t.entries)
	return out
}

// FailureSummary is one kind of failure over the whole run.
type FailureSummary struct {
	Name       string    `json:"name"`
	Method     string    `json:"method"`
	Status     int32     `json:"status"`
	ErrorClass string    `json:"errorClass,omitempty"`
	Message    string    `json:"message,omitempty"`
	Count      uint64    `json:"count"`
	LastSeen   time.Time `json:"lastSeen"`
}

// failureStore accumulates failures across every node, for the whole run.
type failureStore struct {
	mu      sync.RWMutex
	entries map[failureKey]*failureEntry
	limit   int
	dropped uint64
}

func newFailureStore(limit int) *failureStore {
	if limit <= 0 {
		limit = DefaultMaxFailureKinds
	}
	return &failureStore{entries: make(map[failureKey]*failureEntry), limit: limit}
}

// ingest folds one node's batch of failures into the run-wide table.
func (s *failureStore) ingest(samples []*loadwavev1.FailureSample) {
	if len(samples) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sample := range samples {
		key := failureKey{
			name:       sample.GetName(),
			method:     sample.GetMethod(),
			status:     sample.GetStatus(),
			errorClass: sample.GetErrorClass(),
		}
		seen := sample.GetLastSeen().AsTime()

		entry, ok := s.entries[key]
		if !ok {
			if len(s.entries) >= s.limit {
				s.dropped++
				continue
			}
			entry = &failureEntry{key: key}
			s.entries[key] = entry
		}

		entry.count += sample.GetCount()
		if entry.message == "" {
			entry.message = sample.GetMessage()
		}
		if seen.After(entry.lastSeen) {
			entry.lastSeen = seen
		}
	}
}

// summaries returns every kind of failure, most frequent first.
func (s *failureStore) summaries() []FailureSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]FailureSummary, 0, len(s.entries))
	for key, entry := range s.entries {
		out = append(out, FailureSummary{
			Name:       key.name,
			Method:     key.method,
			Status:     key.status,
			ErrorClass: key.errorClass,
			Message:    entry.message,
			Count:      entry.count,
			LastSeen:   entry.lastSeen,
		})
	}

	// Most frequent first: the failure happening ten thousand times is the one
	// worth reading, not the one that happened once.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Status < out[j].Status
	})
	return out
}

// droppedKinds reports how many kinds of failure went untracked.
func (s *failureStore) droppedKinds() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dropped
}
