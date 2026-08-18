// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apportion divides an integer total across weighted claimants.
//
// LoadWave has to split whole virtual users — and whole iterations, and whole
// worker processes — across nodes whose capacities differ. Proportional
// division produces fractions, and naive rounding either loses or invents
// units. Every split in the system funnels through this package so that the
// same rule applies everywhere and the parts always add back up to the whole.
package apportion

import "sort"

// Largest divides total across weights using the largest remainder method.
//
// Each claimant first receives the floor of its exact proportional share; the
// units left over by rounding down are then handed out one at a time, to the
// claimants with the largest discarded fractions. Ties break toward the lower
// index, which keeps the result deterministic — important, because the
// coordinator and the node both compute apportionments and they must agree.
//
// The returned slice always sums to exactly total, provided total is
// non-negative and at least one weight is positive. A total of zero, or an
// empty or all-zero weight set, yields all zeros.
func Largest(total int, weights []int) []int {
	shares := make([]int, len(weights))
	if total <= 0 || len(weights) == 0 {
		return shares
	}

	totalWeight := 0
	for _, w := range weights {
		if w > 0 {
			totalWeight += w
		}
	}
	if totalWeight == 0 {
		return shares
	}

	// remainder tracks each claimant's discarded fraction, scaled by
	// totalWeight so the comparison stays in integer arithmetic and cannot
	// disagree with itself across platforms the way floats might.
	type claim struct {
		index     int
		remainder int
	}
	claims := make([]claim, 0, len(weights))

	assigned := 0
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		exact := total * w
		shares[i] = exact / totalWeight
		assigned += shares[i]
		claims = append(claims, claim{index: i, remainder: exact % totalWeight})
	}

	leftover := total - assigned
	if leftover <= 0 {
		return shares
	}

	sort.SliceStable(claims, func(i, j int) bool {
		if claims[i].remainder != claims[j].remainder {
			return claims[i].remainder > claims[j].remainder
		}
		return claims[i].index < claims[j].index
	})

	for i := 0; i < leftover && i < len(claims); i++ {
		shares[claims[i].index]++
	}
	return shares
}

// Equal divides total across n claimants as evenly as possible, giving the
// remainder to the lowest indices.
func Equal(total, n int) []int {
	if n <= 0 {
		return nil
	}
	weights := make([]int, n)
	for i := range weights {
		weights[i] = 1
	}
	return Largest(total, weights)
}
