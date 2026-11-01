package stats

import (
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
)

// Request kinds of the journal (PLT-11): describe is a describe with mode=on_demand, passive - with mode=passive.
const (
	KindDescribe = "describe"
	KindAsk      = "ask"
	KindPassive  = "passive"
)

// Final statuses of a request that sent answer.end; the other statuses (error, cancelled, skipped, stale, clear) did not.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
)

// Values of degraded_reason (PLT-9).
const (
	DegradedLLMTimeout     = "llm_timeout"
	DegradedLLMUnavailable = "llm_unavailable"
	DegradedTTSUnavailable = "tts_unavailable"
)

// Intents of ask answered by the scene description (PLT-4).
const (
	IntentDescribe      = "describe"
	IntentDescribeAhead = "describe_ahead"
	IntentQuestion      = "question"
)

// MetricE2E is the metric of client.metrics that gives the E2E of a request (PLT-14).
const MetricE2E = "trigger_to_first_audio_ms"

// degradedReasons are the keys of "degraded" in the response, present even with no answers.
var degradedReasons = []string{DegradedLLMTimeout, DegradedLLMUnavailable, DegradedTTSUnavailable}

// llmModes are the describer modes that call the language model.
var llmModes = []string{string(env.DescriberModeLLM), string(env.DescriberModeVLM)}

// GetStatsRequest is the query of GET /v1/stats; an empty value does not filter.
type GetStatsRequest struct {
	Since    string `json:"since" validate:"omitempty,datetime=2006-01-02T15:04:05Z07:00"` // RFC 3339, inclusive
	Until    string `json:"until" validate:"omitempty,datetime=2006-01-02T15:04:05Z07:00"` // RFC 3339, exclusive
	Kind     string `json:"kind" validate:"omitempty,oneof=describe ask passive"`
	ConfigID string `json:"config_id" validate:"omitempty,max=64"`
}

// GetStatsResponse is the answer of GET /v1/stats; its form is the example of PLT-13 (testdata/stats/stats.response.json).
type GetStatsResponse struct {
	ConfigID         *string            `json:"config_id"`
	Since            *string            `json:"since"`
	Until            *string            `json:"until"`
	Kind             *string            `json:"kind"`
	Stages           map[string]Summary `json:"stages"`             // stage of PLT-14 -> durations, ms
	E2E              map[string]Summary `json:"e2e"`                // request kind -> trigger_to_first_audio_ms, ms
	Degraded         map[string]Share   `json:"degraded"`           // degraded_reason -> answers
	LLMErrors        map[string]int     `json:"llm_errors"`         // llm_error -> answers
	LLMFallbackShare *float64           `json:"llm_fallback_share"` // null without scene description answers
	LLMModels        map[string]int     `json:"llm_models"`         // llm_model -> requests
}

// Summary describes a sample of durations, ms.
type Summary struct {
	N    int     `json:"n"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	Mean float64 `json:"mean"`
}

// Share is the number of answers with a property and their share of all answers.
type Share struct {
	N     int     `json:"n"`
	Share float64 `json:"share"`
}

// ErrorResponse is the body of an error answer.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Filter selects the journal requests by their created_at, kind and config_id; a zero value does not filter.
type Filter struct {
	Since    *time.Time // inclusive
	Until    *time.Time // exclusive
	Kind     string
	ConfigID string
}

// Sample is what the journal holds about the selected requests.
type Sample struct {
	Requests []Request
	Stages   []Measurement // Name is the stage of PLT-14
	E2E      []Measurement // Name is the request kind, Value is trigger_to_first_audio_ms
}

// Request is a journal request as far as the statistics are concerned.
type Request struct {
	Kind           string
	DescriberMode  string
	Intent         string
	Status         string
	DegradedReason string
	LLMError       string
	LLMModel       string
}

// Measurement is one named value, ms.
type Measurement struct {
	Name  string
	Value float64
}
