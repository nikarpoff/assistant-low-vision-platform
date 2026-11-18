package stats

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// exampleResponsePath is the copy of the /v1/stats example of PLT-13.
var exampleResponsePath = filepath.Join("..", "..", "testdata", "stats", "stats.response.json")

func TestStats(t *testing.T) {
	var (
		ctx        context.Context
		sample     *Sample
		repository *RepositoryMock
		service    *Service
		request    *GetStatsRequest
	)

	setup := func(repositoryErr error) {
		ctx = context.Background()
		sample = &Sample{}
		repository = &RepositoryMock{
			SelectSampleFunc: func(ctx context.Context, filter *Filter) (*Sample, error) {
				if repositoryErr != nil {
					return nil, repositoryErr
				}
				return sample, nil
			},
		}
		service = NewService(repository)
		request = &GetStatsRequest{}
	}

	t.Run("must_pass_percentiles", func(t *testing.T) {
		setup(nil)
		for _, value := range []float64{20, 1, 19, 2, 18, 3, 17, 4, 16, 5, 15, 6, 14, 7, 13, 8, 12, 9, 11, 10} {
			sample.Stages = append(sample.Stages, Measurement{Name: "detect_ms", Value: value})
			sample.E2E = append(sample.E2E, Measurement{Name: KindDescribe, Value: value * 100})
		}
		sample.Stages = append(sample.Stages, Measurement{Name: "llm_ttft_ms", Value: 583.04})
		sample.E2E = append(sample.E2E,
			Measurement{Name: KindAsk, Value: 1300},
			Measurement{Name: KindAsk, Value: 1100},
			Measurement{Name: KindAsk, Value: 1200},
		)

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Equal(t, map[string]Summary{
			"detect_ms":   {N: 20, P50: 10, P95: 19, Mean: 10.5},
			"llm_ttft_ms": {N: 1, P50: 583, P95: 583, Mean: 583},
		}, response.Stages)
		require.Equal(t, map[string]Summary{
			KindDescribe: {N: 20, P50: 1000, P95: 1900, Mean: 1050},
			KindAsk:      {N: 3, P50: 1200, P95: 1300, Mean: 1200},
		}, response.E2E)
	})

	t.Run("must_pass_filters", func(t *testing.T) {
		setup(nil)
		request = &GetStatsRequest{
			Since:    "2026-11-01T13:00:00+03:00",
			Until:    "2026-11-01T12:00:00.5Z",
			Kind:     KindDescribe,
			ConfigID: "c9f2a1d0e3b4",
		}

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Len(t, repository.SelectSampleCalls(), 1)
		filter := repository.SelectSampleCalls()[0].Filter
		require.Equal(t, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), *filter.Since)
		require.Equal(t, time.UTC, filter.Since.Location())
		require.Equal(t, time.Date(2026, 11, 1, 12, 0, 0, 500_000_000, time.UTC), *filter.Until)
		require.Equal(t, KindDescribe, filter.Kind)
		require.Equal(t, "c9f2a1d0e3b4", filter.ConfigID)
		require.Equal(t, "2026-11-01T10:00:00Z", *response.Since)
		require.Equal(t, "2026-11-01T12:00:00.5Z", *response.Until)
		require.Equal(t, KindDescribe, *response.Kind)
		require.Equal(t, "c9f2a1d0e3b4", *response.ConfigID)
	})

	t.Run("must_pass_filters_empty", func(t *testing.T) {
		setup(nil)

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Equal(t, &Filter{}, repository.SelectSampleCalls()[0].Filter)
		require.Nil(t, response.Since)
		require.Nil(t, response.Until)
		require.Nil(t, response.Kind)
		require.Nil(t, response.ConfigID)
	})

	t.Run("must_pass_degraded_shares", func(t *testing.T) {
		setup(nil)
		sample.Requests = repeat(Request{Kind: KindDescribe, DescriberMode: "llm", Status: StatusOK}, 6)
		sample.Requests = append(sample.Requests,
			Request{Kind: KindDescribe, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMTimeout},
			Request{Kind: KindAsk, Intent: IntentQuestion, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMTimeout},
			Request{Kind: KindDescribe, DescriberMode: "vlm", Status: StatusDegraded, DegradedReason: DegradedLLMUnavailable},
			Request{Kind: KindPassive, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedTTSUnavailable},
			// no answer.end: not in the denominator
			Request{Kind: KindDescribe, DescriberMode: "llm", Status: "error", DegradedReason: DegradedLLMTimeout},
			Request{Kind: KindDescribe, DescriberMode: "llm", Status: "cancelled"},
			Request{Kind: KindPassive, DescriberMode: "llm", Status: "clear"},
			Request{Kind: KindPassive, DescriberMode: "llm", Status: "skipped"},
			Request{Kind: KindPassive, DescriberMode: "llm", Status: "stale"},
		)

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Equal(t, map[string]Share{
			DegradedLLMTimeout:     {N: 2, Share: 0.2},
			DegradedLLMUnavailable: {N: 1, Share: 0.1},
			DegradedTTSUnavailable: {N: 1, Share: 0.1},
		}, response.Degraded)
	})

	t.Run("must_pass_degraded_shares_without_answers", func(t *testing.T) {
		setup(nil)
		sample.Requests = []Request{{Kind: KindDescribe, DescriberMode: "llm", Status: "error"}}

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Equal(t, map[string]Share{
			DegradedLLMTimeout:     {},
			DegradedLLMUnavailable: {},
			DegradedTTSUnavailable: {},
		}, response.Degraded)
	})

	t.Run("must_pass_llm_errors", func(t *testing.T) {
		setup(nil)
		sample.Requests = []Request{
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMTimeout, LLMError: "timeout_ttft"},
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMTimeout, LLMError: "timeout_ttft"},
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMUnavailable, LLMError: "insufficient_balance"},
			{Kind: KindAsk, Intent: IntentQuestion, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMUnavailable, LLMError: "http_5xx"},
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusOK},
			// not an answer
			{Kind: KindDescribe, DescriberMode: "llm", Status: "cancelled", LLMError: "disconnect"},
		}

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Equal(t, map[string]int{"timeout_ttft": 2, "insufficient_balance": 1, "http_5xx": 1}, response.LLMErrors)
	})

	t.Run("must_pass_llm_fallback_share", func(t *testing.T) {
		setup(nil)
		sample.Requests = []Request{
			// scene description answers in the llm and vlm modes: 6, two of them fell back
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusOK},
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMTimeout},
			{Kind: KindDescribe, DescriberMode: "vlm", Status: StatusDegraded, DegradedReason: DegradedLLMUnavailable},
			{Kind: KindAsk, Intent: IntentQuestion, DescriberMode: "llm", Status: StatusOK},
			{Kind: KindAsk, Intent: IntentDescribeAhead, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedTTSUnavailable},
			{Kind: KindAsk, Intent: IntentDescribe, DescriberMode: "vlm", Status: StatusOK},
			// not counted
			{Kind: KindAsk, Intent: "repeat", DescriberMode: "llm", Status: StatusOK},
			{Kind: KindPassive, DescriberMode: "llm", Status: StatusOK},
			{Kind: KindDescribe, DescriberMode: "template", Status: StatusOK},
			{Kind: KindDescribe, DescriberMode: "llm", Status: "error"},
			{Kind: KindDescribe, DescriberMode: "llm", Status: "cancelled", DegradedReason: DegradedLLMTimeout},
		}

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.NotNil(t, response.LLMFallbackShare)
		require.InDelta(t, 2.0/6.0, *response.LLMFallbackShare, 1e-12)
	})

	t.Run("must_pass_llm_fallback_share_null", func(t *testing.T) {
		setup(nil)
		sample.Requests = []Request{
			{Kind: KindDescribe, DescriberMode: "template", Status: StatusOK},
			{Kind: KindPassive, DescriberMode: "llm", Status: StatusOK},
			{Kind: KindDescribe, DescriberMode: "llm", Status: "error"},
		}

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Nil(t, response.LLMFallbackShare)
	})

	t.Run("must_pass_llm_models", func(t *testing.T) {
		setup(nil)
		sample.Requests = []Request{
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusOK, LLMModel: "qwen/qwen3-8b"},
			{Kind: KindAsk, Intent: IntentQuestion, DescriberMode: "llm", Status: "error", LLMModel: "qwen/qwen3-8b"},
			{Kind: KindDescribe, DescriberMode: "llm", Status: "cancelled", LLMModel: "qwen/qwen3-8b-2025-04"},
			{Kind: KindDescribe, DescriberMode: "llm", Status: StatusDegraded, DegradedReason: DegradedLLMUnavailable},
			{Kind: KindDescribe, DescriberMode: "template", Status: StatusOK},
		}

		response, err := service.GetStats(ctx, request)

		require.NoError(t, err)
		require.Equal(t, map[string]int{"qwen/qwen3-8b": 2, "qwen/qwen3-8b-2025-04": 1}, response.LLMModels)
	})

	t.Run("must_pass_response_shape", func(t *testing.T) {
		setup(nil)
		expected, err := os.ReadFile(exampleResponsePath)
		require.NoError(t, err)
		request = &GetStatsRequest{Since: "2026-11-01T10:00:00Z", Until: "2026-11-01T12:00:00Z", ConfigID: "c9f2a1d0e3b4"}
		sample = exampleSample()

		response, err := service.GetStats(ctx, request)
		require.NoError(t, err)
		actual, err := json.Marshal(response)

		require.NoError(t, err)
		require.JSONEq(t, string(expected), string(actual))
		require.Equal(t, jsonShape(t, expected), jsonShape(t, actual))
	})

	t.Run("must_pass_response_shape_without_data", func(t *testing.T) {
		setup(nil)
		expected, err := os.ReadFile(exampleResponsePath)
		require.NoError(t, err)

		response, err := service.GetStats(ctx, request)
		require.NoError(t, err)
		actual, err := json.Marshal(response)

		require.NoError(t, err)
		require.JSONEq(t, `{
			"config_id": null, "since": null, "until": null, "kind": null,
			"stages": {}, "e2e": {},
			"degraded": {"llm_timeout": {"n": 0, "share": 0}, "llm_unavailable": {"n": 0, "share": 0}, "tts_unavailable": {"n": 0, "share": 0}},
			"llm_errors": {}, "llm_fallback_share": null, "llm_models": {}
		}`, string(actual))
		require.Equal(t, topLevelKeys(t, expected), topLevelKeys(t, actual))
	})

	t.Run("must_fail_bad_since", func(t *testing.T) {
		setup(nil)
		request.Since = "2026-11-01 10:00"

		response, err := service.GetStats(ctx, request)

		require.Nil(t, response)
		require.ErrorIs(t, err, ErrValidationFailed)
		require.EqualError(t, err, "validation failed: since must be an RFC 3339 time")
		require.Empty(t, repository.SelectSampleCalls())
	})

	t.Run("must_fail_bad_until", func(t *testing.T) {
		setup(nil)
		request.Until = "tomorrow"

		response, err := service.GetStats(ctx, request)

		require.Nil(t, response)
		require.ErrorIs(t, err, ErrValidationFailed)
		require.EqualError(t, err, "validation failed: until must be an RFC 3339 time")
		require.Empty(t, repository.SelectSampleCalls())
	})

	t.Run("must_fail_until_not_after_since", func(t *testing.T) {
		setup(nil)
		request.Since = "2026-11-01T12:00:00Z"
		request.Until = "2026-11-01T15:00:00+03:00"

		response, err := service.GetStats(ctx, request)

		require.Nil(t, response)
		require.ErrorIs(t, err, ErrValidationFailed)
		require.EqualError(t, err, "validation failed: until must be later than since")
		require.Empty(t, repository.SelectSampleCalls())
	})

	t.Run("must_fail_repository_error", func(t *testing.T) {
		setup(errors.New("db down"))

		response, err := service.GetStats(ctx, request)

		require.Nil(t, response)
		require.EqualError(t, err, "failed to select journal sample: db down")
	})

	t.Run("must_fail_journal_disabled", func(t *testing.T) {
		setup(ErrJournalDisabled)

		response, err := service.GetStats(ctx, request)

		require.Nil(t, response)
		require.ErrorIs(t, err, ErrJournalDisabled)
		require.EqualError(t, err, "failed to select journal sample: journal is disabled")
	})
}

// exampleSample gives exactly the numbers of the PLT-13 example: 300 describe answers in the llm mode,
// 9 of them fell back on a timeout and 3 on refusals of the provider, and 12 cancelled requests.
func exampleSample() *Sample {
	sample := &Sample{}

	answer := Request{Kind: KindDescribe, DescriberMode: "llm", Status: StatusOK, LLMModel: "fake/fake-llm"}
	sample.Requests = repeat(answer, 288)

	answer.Status = StatusDegraded
	answer.DegradedReason = DegradedLLMTimeout
	answer.LLMError = "timeout_ttft"
	sample.Requests = append(sample.Requests, repeat(answer, 9)...)

	answer.DegradedReason = DegradedLLMUnavailable
	answer.LLMError = "http_429"
	sample.Requests = append(sample.Requests, repeat(answer, 2)...)

	answer.LLMError = "insufficient_balance"
	sample.Requests = append(sample.Requests, answer)

	cancelled := Request{Kind: KindDescribe, DescriberMode: "llm", Status: "cancelled", LLMModel: "fake/fake-llm"}
	sample.Requests = append(sample.Requests, repeat(cancelled, 12)...)

	// sorted: x[149] = p50, x[284] = p95, the tail sets the mean
	sample.Stages = measurements("detect_ms", map[float64]int{38.1: 284, 61.0: 1, 78.6: 15})
	sample.E2E = measurements(KindDescribe, map[float64]int{1310: 284, 1820: 1, 2084: 15})

	return sample
}

// repeat returns n copies of the request.
func repeat(request Request, n int) []Request {
	requests := make([]Request, n)
	for i := range requests {
		requests[i] = request
	}
	return requests
}

// measurements returns the named values, each repeated the given number of times, in descending order.
func measurements(name string, counts map[float64]int) []Measurement {
	values := make([]float64, 0, len(counts))
	for value := range counts {
		values = append(values, value)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(values)))

	result := []Measurement{}
	for _, value := range values {
		for i := 0; i < counts[value]; i++ {
			result = append(result, Measurement{Name: name, Value: value})
		}
	}
	return result
}

// jsonShape replaces every value of the JSON document by its type, keeping the keys.
func jsonShape(t *testing.T, document []byte) any {
	var value any
	require.NoError(t, json.Unmarshal(document, &value))
	return shapeOf(value)
}

func shapeOf(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		shape := map[string]any{}
		for key, nested := range typed {
			shape[key] = shapeOf(nested)
		}
		return shape
	case []any:
		shape := make([]any, len(typed))
		for i, nested := range typed {
			shape[i] = shapeOf(nested)
		}
		return shape
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	default:
		return "null"
	}
}

// topLevelKeys returns the sorted keys of the JSON object.
func topLevelKeys(t *testing.T, document []byte) []string {
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(document, &object))

	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
