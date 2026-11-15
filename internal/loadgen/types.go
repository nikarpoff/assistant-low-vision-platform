// Package loadgen imitates the applications for the experiments (PLT-19): it connects to
// the gateway over the real protocol alv.v1 (WS or WSS, binary frames), runs the manifest of
// a data set (EXP-1) in the scenarios describe, ask and passive the given number of times,
// writes a JSONL line per request and a summary of the runs with p50, p95 and the mean of
// the stages and of the time to the first audio, the spread between the runs, the shares of
// every degraded_reason and llm_fallback_share next to its 10 % limit (EXP-3, EXP-5, EXP-8).
//
// loadgen does not send client.metrics: E2E in /v1/stats comes only from real phones, the
// times of loadgen are in its JSONL.
package loadgen

import (
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// Scenarios of loadgen: the request kinds of the journal (PLT-11).
const (
	ScenarioDescribe = stats.KindDescribe
	ScenarioAsk      = stats.KindAsk
	ScenarioPassive  = stats.KindPassive
)

// Scenarios lists every scenario in the order a run goes through them.
var Scenarios = []string{ScenarioDescribe, ScenarioAsk, ScenarioPassive}

// Final statuses of a request in the JSONL: the statuses of the journal (PLT-11).
const (
	StatusOK       = stats.StatusOK
	StatusDegraded = stats.StatusDegraded
	StatusError    = "error"
	StatusClear    = "clear"
	StatusSkipped  = "skipped"
	StatusStale    = "stale"
)

// Error codes of the failures on the side of loadgen; the gateway codes are those of PROTO-11.
const (
	// CodeTimeout - no final message of the request within --timeout-ms.
	CodeTimeout = "loadgen_timeout"
	// CodeConnection - the connection could not be opened or broke.
	CodeConnection = "loadgen_connection"
)

// LLMFallbackShareLimit is the limit of llm_fallback_share in the hypothesis (EXP-8).
const LLMFallbackShareLimit = 0.1

// Record is a line of the JSONL: one request (PLT-19).
type Record struct {
	Run            int                        `json:"run"` // run number, from --first-run
	Kind           string                     `json:"kind"`
	ImageID        *int                       `json:"image_id"`
	QuestionID     *string                    `json:"question_id"`
	RequestID      string                     `json:"request_id"`
	ConfigID       string                     `json:"config_id"`
	SentAt         string                     `json:"sent_at"`        // UTC, RFC 3339 with milliseconds
	AcceptedMS     *protocol.Millis           `json:"accepted_ms"`    // sent - accepted
	FirstAudioMS   *protocol.Millis           `json:"first_audio_ms"` // sent - first AUDIO_OUT
	EndMS          *protocol.Millis           `json:"end_ms"`         // sent - answer.end, passive.clear or error
	Timings        map[string]protocol.Millis `json:"timings"`        // timings of answer.end
	Text           string                     `json:"text"`
	Intent         string                     `json:"intent"`
	Status         string                     `json:"status"`
	Degraded       bool                       `json:"degraded"`
	DegradedReason string                     `json:"degraded_reason"`
	ErrorCode      string                     `json:"error_code"`
	AudioBytes     int                        `json:"audio_bytes"` // PCM of the AUDIO_OUT frames
}

// Answered reports whether the request ended with answer.end.
func (r *Record) Answered() bool {
	return r.Status == StatusOK || r.Status == StatusDegraded
}

// Summary is the summary of the runs (PLT-19).
type Summary struct {
	ConfigIDs             []string     `json:"config_ids"` // every config_id of welcome, in order of appearance
	Sessions              int          `json:"sessions"`
	PauseMS               int          `json:"pause_ms"`
	Runs                  []RunSummary `json:"runs"`
	Pooled                Pooled       `json:"pooled"`
	Spread                Spread       `json:"spread"`
	LLMFallbackShareLimit float64      `json:"llm_fallback_share_limit"`
	LLMFallbackShareOK    *bool        `json:"llm_fallback_share_ok"` // null without scene description answers
}

// Totals are the figures of a run or of the union of the runs.
type Totals struct {
	Requests         map[string]int           `json:"requests"`    // kind -> requests
	Errors           map[string]int           `json:"errors"`      // error code -> requests
	FirstAudio       stats.Summary            `json:"first_audio"` // time to the first audio, ms
	Stages           map[string]stats.Summary `json:"stages"`      // stage of PLT-14 -> timings, ms
	Degraded         map[string]stats.Share   `json:"degraded"`    // degraded_reason -> answers
	LLMFallbackShare *float64                 `json:"llm_fallback_share"`
}

// RunSummary is the summary of one run.
type RunSummary struct {
	Run        int    `json:"run"`
	StartedAt  string `json:"started_at"`  // UTC
	FinishedAt string `json:"finished_at"` // UTC
	Totals
}

// Pooled is the summary of the union of the runs: from the start of the first to the end of the last.
type Pooled struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Totals
}

// Spread is the minimum and the maximum of p50 and p95 over the runs.
type Spread struct {
	FirstAudio Range            `json:"first_audio"`
	Stages     map[string]Range `json:"stages"`
}

// Range is the spread of a sample between the runs; the runs without values are not counted.
type Range struct {
	P50Min float64 `json:"p50_min"`
	P50Max float64 `json:"p50_max"`
	P95Min float64 `json:"p95_min"`
	P95Max float64 `json:"p95_max"`
}
