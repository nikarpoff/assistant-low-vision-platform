package stats

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPercentile(t *testing.T) {
	var (
		oneToTwenty []float64
	)

	setup := func() {
		// 1..20 in a mixed order: the functions must not rely on sorted input
		oneToTwenty = []float64{7, 20, 1, 13, 4, 18, 10, 2, 15, 9, 3, 19, 12, 5, 17, 8, 14, 6, 16, 11}
	}

	tests := []struct {
		name     string
		values   []float64
		q        float64
		expected float64
	}{
		{name: "p50_even", values: []float64{4, 1, 3, 2}, q: 0.5, expected: 2},
		{name: "p50_odd", values: []float64{5, 1, 3}, q: 0.5, expected: 3},
		{name: "p95_of_twenty", values: nil, q: 0.95, expected: 19},
		{name: "p95_of_hundred_is_95th", values: sequence(100), q: 0.95, expected: 95},
		{name: "p95_of_ten_is_max", values: sequence(10), q: 0.95, expected: 10},
		{name: "p95_of_300", values: sequence(300), q: 0.95, expected: 285},
		{name: "p100_is_max", values: []float64{3, 9, 1}, q: 1, expected: 9},
		{name: "small_q_is_min", values: []float64{3, 9, 1}, q: 0.01, expected: 1},
		{name: "single_value", values: []float64{42.5}, q: 0.95, expected: 42.5},
		{name: "no_values", values: []float64{}, q: 0.5, expected: 0},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("must_pass_%s", tt.name), func(t *testing.T) {
			setup()
			values := tt.values
			if values == nil {
				values = oneToTwenty
			}
			before := slices.Clone(values)

			result := Percentile(values, tt.q)

			require.Equal(t, tt.expected, result)
			require.Equal(t, before, values)
		})
	}

	t.Run("must_pass_mean", func(t *testing.T) {
		setup()

		require.Equal(t, 10.5, Mean(oneToTwenty))
		require.Equal(t, 0.0, Mean(nil))
	})

	t.Run("must_pass_summarize", func(t *testing.T) {
		setup()

		summary := Summarize(oneToTwenty)

		require.Equal(t, Summary{N: 20, P50: 10, P95: 19, Mean: 10.5}, summary)
	})

	t.Run("must_pass_summarize_rounds_to_tenth", func(t *testing.T) {
		setup()

		summary := Summarize([]float64{38.14, 38.16, 61.04})

		require.Equal(t, Summary{N: 3, P50: 38.2, P95: 61.0, Mean: 45.8}, summary)
	})

	t.Run("must_pass_summarize_no_values", func(t *testing.T) {
		setup()

		require.Equal(t, Summary{}, Summarize(nil))
	})
}

// sequence returns 1..n.
func sequence(n int) []float64 {
	values := make([]float64, n)
	for i := range values {
		values[i] = float64(i + 1)
	}
	return values
}
