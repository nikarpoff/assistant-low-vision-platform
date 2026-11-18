package pipeline

import (
	"sync"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// Stages of a request (PLT-14): the same names in timings, the journal and the metrics.
const (
	StageReceive          = "receive_ms"
	StageQueue            = "queue_ms"
	StageDetect           = "detect_ms"
	StageASR              = "asr_ms"
	StageLLMTTFT          = "llm_ttft_ms"
	StageLLMFirstSentence = "llm_first_sentence_ms"
	StageLLMTotal         = "llm_total_ms"
	StageTTSFirstChunk    = "tts_first_chunk_ms"
	StageTTSTotal         = "tts_total_ms"
	StageServerTTFA       = "server_ttfa_ms"
	StageServerTotal      = "server_total_ms"
)

// Timings are the durations of the stages of one request. It is safe for concurrent use:
// the stages of TTS are measured while the language model is still streaming.
type Timings struct {
	mu     sync.Mutex
	values map[string]time.Duration
}

// NewTimings creates empty timings.
func NewTimings() *Timings {
	return &Timings{values: map[string]time.Duration{}}
}

// Set records the duration of the stage, replacing an earlier one.
func (t *Timings) Set(stage string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.values[stage] = max(d, 0)
}

// SetOnce records the duration of the stage unless it is already recorded: the first
// sentence, the first chunk, the first frame.
func (t *Timings) SetOnce(stage string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.values[stage]; !ok {
		t.values[stage] = max(d, 0)
	}
}

// Has reports whether the stage is recorded.
func (t *Timings) Has(stage string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, ok := t.values[stage]
	return ok
}

// Protocol returns the timings of answer.end: stage -> milliseconds with one decimal place (PROTO-12).
func (t *Timings) Protocol() protocol.Timings {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make(protocol.Timings, len(t.values))
	for stage, d := range t.values {
		result[stage] = protocol.DurationMillis(d)
	}
	return result
}

// Milliseconds returns the timings for the journal: stage -> milliseconds.
func (t *Timings) Milliseconds() map[string]float64 {
	timings := t.Protocol()
	result := make(map[string]float64, len(timings))
	for stage, value := range timings {
		result[stage] = float64(value)
	}
	return result
}

// Observe adds the stages to alv_stage_duration_seconds{stage, kind}.
func (t *Timings) Observe(m *metrics.Metrics, kind string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for stage, d := range t.values {
		m.StageDuration.WithLabelValues(stage, kind).Observe(d.Seconds())
	}
}
