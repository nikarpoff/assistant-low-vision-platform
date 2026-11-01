package stats

import (
	"slices"
)

// Answer is an answer.end as llm_fallback_share sees it.
type Answer struct {
	Kind           string // describe, ask, passive
	Intent         string
	DescriberMode  string // template, llm, vlm
	DegradedReason string
}

// IsSceneDescription reports whether the scene description built the answer in the llm or vlm mode:
// a describe with mode=on_demand or an ask with the intent describe, describe_ahead or question (PLT-13).
func (a Answer) IsSceneDescription() bool {
	if !slices.Contains(llmModes, a.DescriberMode) {
		return false
	}

	switch a.Kind {
	case KindDescribe:
		return true
	case KindAsk:
		return a.Intent == IntentDescribe || a.Intent == IntentDescribeAhead || a.Intent == IntentQuestion
	default:
		return false
	}
}

// IsFallback reports whether the answer fell back to the template because of the language model:
// degraded_reason llm_timeout or llm_unavailable.
func (a Answer) IsFallback() bool {
	return a.DegradedReason == DegradedLLMTimeout || a.DegradedReason == DegradedLLMUnavailable
}

// CountFallbacks returns the number of scene description answers (total) and of the fallbacks among them.
// The verdict of EXP-8 compares them as integers: fallbacks*100 <= 10*total.
func CountFallbacks(answers []Answer) (fallbacks, total int) {
	for _, answer := range answers {
		if !answer.IsSceneDescription() {
			continue
		}
		total++
		if answer.IsFallback() {
			fallbacks++
		}
	}
	return fallbacks, total
}

// FallbackShare returns llm_fallback_share (PLT-13, EXP-8): the fallbacks divided by all scene
// description answers in the llm and vlm modes; nil when there is no such answer.
func FallbackShare(answers []Answer) *float64 {
	fallbacks, total := CountFallbacks(answers)
	if total == 0 {
		return nil
	}

	share := float64(fallbacks) / float64(total)
	return &share
}
