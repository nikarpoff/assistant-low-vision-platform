package stats

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFallbackShare(t *testing.T) {
	var (
		answers []Answer
	)

	setup := func(cases ...Answer) {
		answers = cases
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(
			Answer{Kind: KindDescribe, DescriberMode: "llm"},
			Answer{Kind: KindDescribe, DescriberMode: "llm", DegradedReason: DegradedLLMTimeout},
			Answer{Kind: KindDescribe, DescriberMode: "vlm", DegradedReason: DegradedLLMUnavailable},
			Answer{Kind: KindAsk, Intent: IntentQuestion, DescriberMode: "llm", DegradedReason: DegradedTTSUnavailable},
		)

		fallbacks, total := CountFallbacks(answers)
		share := FallbackShare(answers)

		require.Equal(t, 2, fallbacks)
		require.Equal(t, 4, total)
		require.NotNil(t, share)
		require.Equal(t, 0.5, *share)
	})

	t.Run("must_pass_scene_description_answers", func(t *testing.T) {
		setup()

		tests := []struct {
			answer   Answer
			expected bool
		}{
			{answer: Answer{Kind: KindDescribe, DescriberMode: "llm"}, expected: true},
			{answer: Answer{Kind: KindDescribe, DescriberMode: "vlm"}, expected: true},
			{answer: Answer{Kind: KindDescribe, DescriberMode: "template"}, expected: false},
			{answer: Answer{Kind: KindDescribe, DescriberMode: ""}, expected: false},
			{answer: Answer{Kind: KindAsk, Intent: IntentDescribe, DescriberMode: "llm"}, expected: true},
			{answer: Answer{Kind: KindAsk, Intent: IntentDescribeAhead, DescriberMode: "llm"}, expected: true},
			{answer: Answer{Kind: KindAsk, Intent: IntentQuestion, DescriberMode: "vlm"}, expected: true},
			{answer: Answer{Kind: KindAsk, Intent: "repeat", DescriberMode: "llm"}, expected: false},
			{answer: Answer{Kind: KindAsk, Intent: "passive_on", DescriberMode: "llm"}, expected: false},
			{answer: Answer{Kind: KindAsk, Intent: IntentQuestion, DescriberMode: "template"}, expected: false},
			{answer: Answer{Kind: KindPassive, Intent: IntentDescribe, DescriberMode: "llm"}, expected: false},
		}

		for _, tt := range tests {
			require.Equal(t, tt.expected, tt.answer.IsSceneDescription(), "%+v", tt.answer)
		}
	})

	t.Run("must_pass_fallback_reasons", func(t *testing.T) {
		setup()

		require.True(t, Answer{DegradedReason: DegradedLLMTimeout}.IsFallback())
		require.True(t, Answer{DegradedReason: DegradedLLMUnavailable}.IsFallback())
		require.False(t, Answer{DegradedReason: DegradedTTSUnavailable}.IsFallback())
		require.False(t, Answer{}.IsFallback())
	})

	t.Run("must_pass_boundary_ten_percent", func(t *testing.T) {
		setup()
		for i := 0; i < 30; i++ {
			answer := Answer{Kind: KindDescribe, DescriberMode: "llm"}
			if i < 3 {
				answer.DegradedReason = DegradedLLMTimeout
			}
			answers = append(answers, answer)
		}

		fallbacks, total := CountFallbacks(answers)
		share := FallbackShare(answers)

		require.True(t, fallbacks*100 <= 10*total)
		require.Equal(t, 0.1, *share)
	})

	t.Run("must_pass_null_without_scene_description", func(t *testing.T) {
		setup(
			Answer{Kind: KindPassive, DescriberMode: "llm"},
			Answer{Kind: KindAsk, Intent: "stop", DescriberMode: "llm"},
			Answer{Kind: KindDescribe, DescriberMode: "template", DegradedReason: DegradedTTSUnavailable},
		)

		fallbacks, total := CountFallbacks(answers)

		require.Equal(t, 0, fallbacks)
		require.Equal(t, 0, total)
		require.Nil(t, FallbackShare(answers))
		require.Nil(t, FallbackShare(nil))
	})
}
