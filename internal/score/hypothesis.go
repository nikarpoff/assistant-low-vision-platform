package score

import (
	"fmt"
	"math"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// microsPerUnit turns the float values of /v1/stats into integers: milliseconds into
// microseconds and shares into millionths, so that the limits compare as integers.
const microsPerUnit = 1_000_000

// errConfigMismatchFormat is the text of the error of /v1/stats saved for another configuration.
const errConfigMismatchFormat = "the JSONL has config_id %q, the /v1/stats response is for config_id %q"

// BuildHypothesis gives the verdict of EXP-8 from the union of the runs scored by Score and the
// /v1/stats response saved for the config_id over the time of the runs:
//   - e2e: p95 of E2E of describe from /v1/stats, not more than 2000 ms;
//   - quality: key_objects_with_direction of the union of the runs, not less than 75 %;
//   - fallbacks: llm_fallback_share from /v1/stats, not more than 10 %.
//
// The limits are included and compared as integers. A missing value and its check are nil.
// The verdict is not_confirmed when a check fails, insufficient_data when no check fails but a
// value is missing or there are fewer than MinRuns runs, and confirmed otherwise.
func BuildHypothesis(result *Result, response *stats.GetStatsResponse) (*Hypothesis, error) {
	configID, err := hypothesisConfigID(result.ConfigIDs, response.ConfigID)
	if err != nil {
		return nil, err
	}

	h := &Hypothesis{
		ConfigID:                configID,
		Runs:                    len(result.Runs),
		E2EP95LimitMS:           E2EP95LimitMS,
		KeyObjectsWithDirection: result.Pooled.KeyObjectsWithDirection,
		KeyObjectsLimit:         float64(KeyObjectsLimitPercent) / 100,
		LLMFallbackShare:        response.LLMFallbackShare,
		LLMFallbackShareLimit:   float64(LLMFallbackShareLimitPercent) / 100,
	}

	if e2e, ok := response.E2E[stats.KindDescribe]; ok && e2e.N > 0 {
		p95 := e2e.P95
		h.E2EP95MS = &p95
		h.Checks.E2E = check(toMicros(p95) <= int64(E2EP95LimitMS)*microsPerUnit)
	}
	if result.Pooled.KeyObjects > 0 {
		pooled := result.Pooled
		h.Checks.Quality = check(pooled.NamedWithDirection*100 >= KeyObjectsLimitPercent*pooled.KeyObjects)
	}
	if response.LLMFallbackShare != nil {
		h.Checks.Fallbacks = check(toMicros(*response.LLMFallbackShare)*100 <= int64(LLMFallbackShareLimitPercent)*microsPerUnit)
	}

	h.Verdict = verdict(h)
	return h, nil
}

// verdict decides on the hypothesis by its checks.
func verdict(h *Hypothesis) string {
	checks := []*bool{h.Checks.E2E, h.Checks.Quality, h.Checks.Fallbacks}
	complete := h.Runs >= MinRuns
	for _, ok := range checks {
		if ok != nil && !*ok {
			return VerdictNotConfirmed
		}
		if ok == nil {
			complete = false
		}
	}
	if !complete {
		return VerdictInsufficientData
	}
	return VerdictConfirmed
}

// hypothesisConfigID returns the config_id of the verdict: the one of /v1/stats, which every
// config_id of the JSONL must match, or without it the only config_id of the JSONL.
func hypothesisConfigID(jsonl []string, statsConfigID *string) (*string, error) {
	if statsConfigID == nil {
		if len(jsonl) == 1 {
			id := jsonl[0]
			return &id, nil
		}
		return nil, nil
	}
	for _, id := range jsonl {
		if id != *statsConfigID {
			return nil, fmt.Errorf(errConfigMismatchFormat, id, *statsConfigID)
		}
	}
	id := *statsConfigID
	return &id, nil
}

// toMicros returns the value in millionths of its unit, rounded.
func toMicros(value float64) int64 {
	return int64(math.Round(value * microsPerUnit))
}

// check returns a pointer to ok.
func check(ok bool) *bool {
	return &ok
}
