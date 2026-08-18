// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave_test

import (
	"testing"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

func TestLabelsOrderIndependent(t *testing.T) {
	t.Parallel()

	// Two call sites that build the same tags in different orders must land on
	// the same series, or the metrics store will double-count them.
	a := loadwave.NewLabels("method", "GET", "status", "200")
	b := loadwave.NewLabels("status", "200", "method", "GET")

	if !a.Equal(b) {
		t.Fatalf("%v and %v should be equal", a, b)
	}
	if a.Hash() != b.Hash() {
		t.Fatalf("hashes differ: %d vs %d", a.Hash(), b.Hash())
	}
	if a.String() != "method=GET,status=200" {
		t.Fatalf("String() = %q", a.String())
	}
}

func TestLabelsWithDoesNotMutateTheReceiver(t *testing.T) {
	t.Parallel()

	// VUs share a base label set; if With aliased its receiver's backing
	// array, one virtual user's tag would appear on another's metrics.
	base := loadwave.NewLabels("scenario", "browse")
	derived := base.With("status", "500")

	if _, ok := base.Get("status"); ok {
		t.Fatal("With mutated the receiver")
	}
	if got, _ := derived.Get("status"); got != "500" {
		t.Fatalf("derived lost its added label: %v", derived)
	}
	if got, _ := derived.Get("scenario"); got != "browse" {
		t.Fatalf("derived lost the base label: %v", derived)
	}

	// Deriving twice from the same base must not have the first derivation
	// leak into the second.
	other := base.With("status", "200")
	if got, _ := derived.Get("status"); got != "500" {
		t.Fatalf("second derivation corrupted the first: %v", derived)
	}
	if got, _ := other.Get("status"); got != "200" {
		t.Fatalf("second derivation is wrong: %v", other)
	}
}

func TestLabelsWithReplacesExistingKeys(t *testing.T) {
	t.Parallel()

	labels := loadwave.NewLabels("status", "200").With("status", "404")

	if labels.Len() != 1 {
		t.Fatalf("expected one pair, got %d: %v", labels.Len(), labels)
	}
	if got, _ := labels.Get("status"); got != "404" {
		t.Fatalf("Get(status) = %q, want 404", got)
	}
}

func TestLabelsSeparatorPreventsCollision(t *testing.T) {
	t.Parallel()

	// Without a separator byte between parts, these two would hash the same.
	a := loadwave.NewLabels("ab", "c")
	b := loadwave.NewLabels("a", "bc")

	if a.Equal(b) {
		t.Fatal("distinct label sets compared equal")
	}
	if a.Hash() == b.Hash() {
		t.Fatalf("hash collision between %v and %v", a, b)
	}
}

func TestLabelsZeroValue(t *testing.T) {
	t.Parallel()

	var zero loadwave.Labels

	if zero.Len() != 0 {
		t.Fatalf("zero Labels has %d pairs", zero.Len())
	}
	if zero.String() != "" {
		t.Fatalf("zero Labels renders as %q", zero.String())
	}
	if zero.Map() != nil {
		t.Fatal("zero Labels should map to nil")
	}
	if got, ok := zero.Get("anything"); ok || got != "" {
		t.Fatalf("Get on zero Labels returned (%q, %v)", got, ok)
	}

	added := zero.With("a", "1")
	if got, _ := added.Get("a"); got != "1" {
		t.Fatal("With on a zero Labels did not work")
	}
}

func TestLabelsRoundTripThroughMap(t *testing.T) {
	t.Parallel()

	original := loadwave.NewLabels("scenario", "browse", "method", "GET", "status", "200")
	recovered := loadwave.LabelsFromMap(original.Map())

	if !original.Equal(recovered) {
		t.Fatalf("round trip changed the labels: %v -> %v", original, recovered)
	}
}

func TestNewLabelsRejectsOddArguments(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic on an odd number of arguments")
		}
	}()
	//nolint:staticcheck // The odd argument count is the thing under test.
	loadwave.NewLabels("lonely")
}

func TestLabelsAllIteratesInKeyOrder(t *testing.T) {
	t.Parallel()

	labels := loadwave.NewLabels("z", "1", "a", "2", "m", "3")

	var keys []string
	labels.All(func(key, _ string) bool {
		keys = append(keys, key)
		return true
	})

	want := []string{"a", "m", "z"}
	for i, key := range want {
		if keys[i] != key {
			t.Fatalf("iteration order = %v, want %v", keys, want)
		}
	}
}

func BenchmarkLabelsWith(b *testing.B) {
	base := loadwave.NewLabels("scenario", "browse")
	b.ReportAllocs()

	for b.Loop() {
		_ = base.With("method", "GET", "status", "200")
	}
}
