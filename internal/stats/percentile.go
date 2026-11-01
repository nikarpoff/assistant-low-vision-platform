// Package stats computes the statistics of the gateway: percentiles, means and
// llm_fallback_share by one rule for /v1/stats, loadgen and score, so the
// numbers of different reports are comparable.
package stats

import (
	"math"
	"slices"
)

// Quantiles of a Summary.
const (
	quantileP50 = 0.5
	quantileP95 = 0.95
)

// Percentile returns the q-quantile (0 < q <= 1) of the values by the nearest-rank
// method: x[ceil(q*n)-1] of the sorted values. No values give 0.
func Percentile(values []float64, q float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return percentileSorted(sorted, q)
}

// Mean returns the arithmetic mean of the values. No values give 0.
func Mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

// Summarize returns the number of the values, their p50, p95 and mean rounded
// to 0.1, as the gateway writes durations in milliseconds (PROTO-12).
func Summarize(values []float64) Summary {
	sorted := slices.Clone(values)
	slices.Sort(sorted)

	return Summary{
		N:    len(sorted),
		P50:  roundTenth(percentileSorted(sorted, quantileP50)),
		P95:  roundTenth(percentileSorted(sorted, quantileP95)),
		Mean: roundTenth(Mean(sorted)),
	}
}

// percentileSorted is Percentile of values sorted in ascending order.
func percentileSorted(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}

	rank := int(math.Ceil(q * float64(n)))
	rank = min(max(rank, 1), n)
	return sorted[rank-1]
}

// roundTenth rounds to one digit after the decimal point.
func roundTenth(value float64) float64 {
	return math.Round(value*10) / 10
}
