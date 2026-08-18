// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"errors"
	"fmt"
	"math"
	"sync"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
)

// HistogramConfig fixes the resolution of every trend metric in a run.
//
// Every node must use identical settings: HDR histograms can only be merged
// when their bucket layouts match, and a mismatch is what turns a distributed
// p99 into nonsense. The coordinator therefore pins the configuration and
// nodes never choose their own.
type HistogramConfig struct {
	// Lowest is the smallest distinguishable value, in histogram units.
	Lowest int64
	// Highest is the largest trackable value, in histogram units. Larger
	// observations are clamped to it and counted as clipped.
	Highest int64
	// SigFigs is the number of significant decimal digits kept, which sets
	// the relative error and, with it, the memory each histogram occupies.
	SigFigs int
}

// Trend metrics arrive as floating point values in their natural unit —
// milliseconds for every built-in latency metric. HDR stores integers, so
// values are scaled before recording and unscaled on the way out.
//
// A scale of 1000 with a lowest value of 100 gives a resolution floor of
// 0.1ms and a ceiling of 60s, which brackets everything an HTTP load test
// cares to distinguish.
const (
	// HistogramScale converts a metric's natural unit into histogram units.
	HistogramScale = 1000

	DefaultHistogramLowest  int64 = 100        // 0.1 ms
	DefaultHistogramHighest int64 = 60_000_000 // 60 s
	DefaultSigFigs                = 2          // ~1% relative error
)

// DefaultHistogramConfig returns the resolution used unless a run overrides it.
func DefaultHistogramConfig() HistogramConfig {
	return HistogramConfig{
		Lowest:  DefaultHistogramLowest,
		Highest: DefaultHistogramHighest,
		SigFigs: DefaultSigFigs,
	}
}

// Validate reports whether the configuration can build a histogram.
func (c HistogramConfig) Validate() error {
	if c.Lowest < 1 {
		return fmt.Errorf("histogram lowest value must be at least 1, got %d", c.Lowest)
	}
	if c.Highest <= c.Lowest {
		return fmt.Errorf("histogram highest value %d must exceed lowest value %d", c.Highest, c.Lowest)
	}
	if c.SigFigs < 1 || c.SigFigs > 5 {
		return fmt.Errorf("histogram significant figures must be between 1 and 5, got %d", c.SigFigs)
	}
	return nil
}

// New allocates a histogram with this configuration.
func (c HistogramConfig) New() *hdr.Histogram {
	return hdr.New(c.Lowest, c.Highest, c.SigFigs)
}

// Equal reports whether two configurations produce mergeable histograms.
func (c HistogramConfig) Equal(other HistogramConfig) bool {
	return c.Lowest == other.Lowest && c.Highest == other.Highest && c.SigFigs == other.SigFigs
}

// ScaleValue converts a metric value in its natural unit into histogram units,
// clamped into the trackable range.
//
// Clamping rather than dropping is deliberate: an observation above the
// ceiling is nearly always a genuine, interesting outlier — a request that hit
// its timeout — and silently discarding it would make the tail look better
// than it is. Recording it at the ceiling understates it but keeps it counted.
func (c HistogramConfig) ScaleValue(v float64) int64 {
	if math.IsNaN(v) {
		return c.Lowest
	}
	scaled := int64(math.Round(v * HistogramScale))
	if scaled < c.Lowest {
		return c.Lowest
	}
	if scaled > c.Highest {
		return c.Highest
	}
	return scaled
}

// UnscaleValue converts a histogram unit back to the metric's natural unit.
func UnscaleValue(v int64) float64 { return float64(v) / HistogramScale }

// EncodeHistogram converts a histogram into its wire form.
func EncodeHistogram(h *hdr.Histogram) *loadwavev1.HistogramSnapshot {
	snap := h.Export()
	return &loadwavev1.HistogramSnapshot{
		LowestTrackableValue:  snap.LowestTrackableValue,
		HighestTrackableValue: snap.HighestTrackableValue,
		SignificantFigures:    int32(snap.SignificantFigures),
		CountsRle:             encodeRLE(snap.Counts),
		TotalCount:            h.TotalCount(),
	}
}

// DecodeHistogram rebuilds a histogram from its wire form.
//
// The bucket layout implied by the message is checked against cfg before the
// counts are installed. Skipping that check would let a node running a
// different resolution corrupt the merged distribution, or index out of bounds
// inside the HDR library.
func DecodeHistogram(snap *loadwavev1.HistogramSnapshot, cfg HistogramConfig) (*hdr.Histogram, error) {
	if snap == nil {
		return nil, errors.New("histogram snapshot is nil")
	}

	got := HistogramConfig{
		Lowest:  snap.GetLowestTrackableValue(),
		Highest: snap.GetHighestTrackableValue(),
		SigFigs: int(snap.GetSignificantFigures()),
	}
	if !got.Equal(cfg) {
		return nil, fmt.Errorf(
			"histogram resolution mismatch: node sent lowest=%d highest=%d sigfigs=%d, run uses lowest=%d highest=%d sigfigs=%d",
			got.Lowest, got.Highest, got.SigFigs, cfg.Lowest, cfg.Highest, cfg.SigFigs)
	}

	counts, err := decodeRLE(snap.GetCountsRle())
	if err != nil {
		return nil, err
	}

	want := CountsLen(cfg)
	if len(counts) != want {
		return nil, fmt.Errorf("histogram has %d buckets, expected %d", len(counts), want)
	}

	return hdr.Import(&hdr.Snapshot{
		LowestTrackableValue:  cfg.Lowest,
		HighestTrackableValue: cfg.Highest,
		SignificantFigures:    int64(cfg.SigFigs),
		Counts:                counts,
	}), nil
}

// countsLenCache memoises the bucket count for a configuration, since working
// it out means allocating a throwaway histogram and every decode needs it.
//
// The coordinator decodes histograms on every agent's stream goroutine at
// once, so this is genuinely contended and cannot be a bare map.
var countsLenCache sync.Map // HistogramConfig -> int

// CountsLen reports how many buckets a histogram with this configuration has.
//
// Safe for concurrent use. Two callers racing on a cold configuration may both
// compute the value, which is harmless: it is a pure function of cfg, so they
// agree, and one simply overwrites the other.
func CountsLen(cfg HistogramConfig) int {
	if cached, ok := countsLenCache.Load(cfg); ok {
		return cached.(int)
	}
	n := len(cfg.New().Export().Counts)
	countsLenCache.Store(cfg, n)
	return n
}

// encodeRLE compresses a histogram's bucket counts by collapsing zero runs.
//
// A histogram sized for 0.1ms-to-60s has a couple of thousand buckets, and a
// real latency distribution lights up a few dozen of them. Sending the raw
// array would cost a few kilobytes per series per second per node, which at
// realistic cardinality dominates the entire control-plane budget. Collapsing
// the zeros typically shrinks it by one to two orders of magnitude.
//
// The encoding is: a non-zero count stands for itself; a zero is followed by
// the length of the run of zeros it begins.
func encodeRLE(counts []int64) []int64 {
	out := make([]int64, 0, 64)
	for i := 0; i < len(counts); {
		if counts[i] != 0 {
			out = append(out, counts[i])
			i++
			continue
		}
		run := int64(0)
		for i < len(counts) && counts[i] == 0 {
			run++
			i++
		}
		out = append(out, 0, run)
	}
	return out
}

// maxDecodedCounts bounds how large a decoded bucket array may get, so a
// malformed or hostile run length cannot make a node allocate without limit.
// It is far above any legitimate histogram: five significant figures over the
// widest sensible range stays well under it.
const maxDecodedCounts = 1 << 22

// decodeRLE reverses encodeRLE.
func decodeRLE(encoded []int64) ([]int64, error) {
	out := make([]int64, 0, len(encoded)*2)
	for i := 0; i < len(encoded); i++ {
		v := encoded[i]
		if v > 0 {
			out = append(out, v)
			continue
		}
		if v < 0 {
			return nil, fmt.Errorf("histogram bucket count %d at index %d is negative", v, i)
		}

		i++
		if i >= len(encoded) {
			return nil, errors.New("histogram encoding ends with a zero marker and no run length")
		}
		run := encoded[i]
		if run <= 0 {
			return nil, fmt.Errorf("histogram zero run length %d at index %d is not positive", run, i)
		}
		if int64(len(out))+run > maxDecodedCounts {
			return nil, fmt.Errorf("histogram expands to more than %d buckets", maxDecodedCounts)
		}
		out = append(out, make([]int64, run)...)
	}
	return out, nil
}
