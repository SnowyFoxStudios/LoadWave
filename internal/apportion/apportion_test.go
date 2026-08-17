// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package apportion_test

import (
	"slices"
	"testing"

	"github.com/SnowyFoxStudios/LoadWave/internal/apportion"
)

func TestLargestSumsToTotal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		total   int
		weights []int
		want    []int
	}{
		{"even split", 100, []int{1, 1, 1, 1}, []int{25, 25, 25, 25}},
		{"weighted", 100, []int{3, 1}, []int{75, 25}},
		{"remainder to the largest fractions", 10, []int{1, 1, 1}, []int{4, 3, 3}},
		{"fewer units than claimants", 2, []int{1, 1, 1, 1}, []int{1, 1, 0, 0}},
		{"single claimant", 7, []int{5}, []int{7}},
		{"zero weight gets nothing", 10, []int{1, 0, 1}, []int{5, 0, 5}},
		{"negative weight gets nothing", 10, []int{1, -4, 1}, []int{5, 0, 5}},
		{"zero total", 0, []int{1, 2, 3}, []int{0, 0, 0}},
		{"no weights", 10, nil, []int{}},
		{"all weights zero", 10, []int{0, 0}, []int{0, 0}},
		// Exact shares are 7.27, 1.81 and 0.91. Flooring assigns 7, 1 and 0,
		// leaving two units for the two largest discarded fractions.
		{"uneven capacities", 10, []int{16, 4, 2}, []int{7, 2, 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := apportion.Largest(tc.total, tc.weights)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Largest(%d, %v) = %v, want %v", tc.total, tc.weights, got, tc.want)
			}
		})
	}
}

// The whole point of this package is that nothing is lost or invented when a
// total is split, so the invariant is checked across a wide sweep rather than
// only at the hand-picked cases above.
func TestLargestConservesTheTotal(t *testing.T) {
	t.Parallel()

	weightSets := [][]int{
		{1},
		{1, 1},
		{1, 2, 3},
		{16, 8, 4, 2, 1},
		{1000, 1, 1},
		{7, 7, 7, 7, 7, 7, 7},
	}

	for _, weights := range weightSets {
		for total := range 500 {
			shares := apportion.Largest(total, weights)

			sum := 0
			for _, share := range shares {
				if share < 0 {
					t.Fatalf("Largest(%d, %v) produced a negative share %v", total, weights, shares)
				}
				sum += share
			}
			if sum != total {
				t.Fatalf("Largest(%d, %v) = %v, sums to %d", total, weights, shares, sum)
			}
		}
	}
}

// The coordinator and each agent apportion independently and must agree, so
// the result has to depend only on the inputs.
func TestLargestIsDeterministic(t *testing.T) {
	t.Parallel()

	weights := []int{5, 3, 3, 1}
	first := apportion.Largest(97, weights)

	for range 100 {
		again := apportion.Largest(97, weights)
		if !slices.Equal(first, again) {
			t.Fatalf("Largest is not deterministic: %v then %v", first, again)
		}
	}
}

func TestLargestFavoursTheBiggerWeight(t *testing.T) {
	t.Parallel()

	// A sixteen-core host must carry more than a two-core one, not the same.
	shares := apportion.Largest(9, []int{16, 2})
	if shares[0] <= shares[1] {
		t.Fatalf("expected the larger weight to receive more, got %v", shares)
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()

	if got := apportion.Equal(10, 3); !slices.Equal(got, []int{4, 3, 3}) {
		t.Fatalf("Equal(10, 3) = %v, want [4 3 3]", got)
	}
	if got := apportion.Equal(10, 0); got != nil {
		t.Fatalf("Equal(10, 0) = %v, want nil", got)
	}
}
