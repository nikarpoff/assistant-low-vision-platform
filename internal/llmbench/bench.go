// Package llmbench measures the language model provider (LLM-4, PLT-19) by the
// client of the gateway (PLT-15): sequential streamed requests with the reference
// prompt, the time to the first token and from the first token to the end of the
// first sentence, the result in the common JSON schema of EXP-2.
package llmbench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// Fixed fields of the report.
const (
	serviceName = "llm"
	// sentenceEnds end a sentence when a space or the end of the text follows (PLT-7).
	sentenceEnds = ".!?…"
	// requestIDFormat identifies a request of the run in the log of the client; it is never sent.
	requestIDFormat = "llmbench-%d"
)

// Quantiles of the report.
const (
	quantileP50 = 0.5
	quantileP95 = 0.95
)

// Texts of the diagnostics and errors, shared by the code and the tests.
const (
	diagFailureFormat = "llmbench: request %d (%s) failed: %v\n"
	phaseWarmup       = "warmup"
	phaseMeasured     = "measured"
	errAbortFormat    = "llmbench aborted at request %d: %w"
)

//go:generate moq -out completer_moq_test.go . Completer

// Completer sends a chat completion request to the language model provider (*llm.Client).
type Completer interface {
	Complete(ctx context.Context, req *llm.Request, onText llm.TextHandler) (*llm.Result, error)
}

// Config holds the settings of one run.
type Config struct {
	// Backend is the host of the provider URL, Model - the requested model.
	Backend string
	Model   string
	// N measured requests follow Warmup requests that are not measured.
	N      int
	Warmup int
	// Messages is the prompt of every request.
	Messages []llm.Message
	// GitSHA, Hardware and MeasuredFrom describe the measuring machine.
	GitSHA       string
	Hardware     Hardware
	MeasuredFrom string
}

// Hardware is the machine the measurement is made from.
type Hardware struct {
	CPU string  `json:"cpu"`
	GPU *string `json:"gpu"`
}

// Report is the result in the common schema of EXP-2 with the fields of PLT-19.
// Latencies are in milliseconds rounded to 0.1; null when no request succeeded.
type Report struct {
	Service   string   `json:"service"`
	Backend   string   `json:"backend"`
	Model     string   `json:"model"`
	Device    *string  `json:"device"`
	GitSHA    string   `json:"git_sha"`
	N         int      `json:"n"`
	Warmup    int      `json:"warmup"`
	P50MS     *float64 `json:"p50_ms"`
	P95MS     *float64 `json:"p95_ms"`
	MeanMS    *float64 `json:"mean_ms"`
	StdMS     *float64 `json:"std_ms"`
	Hardware  Hardware `json:"hardware"`
	StartedAt string   `json:"started_at"`

	// FirstSentenceP50MS and FirstSentenceP95MS are from the first token to the end of the first sentence.
	FirstSentenceP50MS *float64 `json:"first_sentence_p50_ms"`
	FirstSentenceP95MS *float64 `json:"first_sentence_p95_ms"`
	// TokensPerS is the generation speed after the first token.
	TokensPerS *float64 `json:"tokens_per_s"`
	// PromptTokens is prompt_tokens of the provider usage, the largest one; null without usage.
	PromptTokens *int `json:"prompt_tokens"`
	// ResponseModels are the distinct values of model in the answers, in the order of appearance.
	ResponseModels []string `json:"response_models"`
	// Errors counts the failed requests by every reason of PLT-15, the warmup included.
	Errors       map[llm.Reason]int `json:"errors"`
	MeasuredFrom string             `json:"measured_from"`
}

// Failed reports whether at least one request failed or answered empty.
func (r *Report) Failed() bool {
	for _, count := range r.Errors {
		if count > 0 {
			return true
		}
	}
	return false
}

// allReasons are the keys of Report.Errors: every outcome of the PLT-15 table.
var allReasons = []llm.Reason{
	llm.ReasonInsufficientBalance,
	llm.ReasonHTTP429,
	llm.ReasonHTTP5xx,
	llm.ReasonHTTP4xx,
	llm.ReasonStreamError,
	llm.ReasonDisconnect,
	llm.ReasonEmpty,
	llm.ReasonTimeoutTTFT,
	llm.ReasonTimeoutTotal,
}

// Bench runs the requests one after another.
type Bench struct {
	client Completer
	cfg    Config
	diag   io.Writer
	now    func() time.Time
}

// NewBench creates a run; failures of single requests are written to diag.
func NewBench(client Completer, cfg Config, diag io.Writer) *Bench {
	return &Bench{client: client, cfg: cfg, diag: diag, now: time.Now}
}

// sample is what one successful measured request gave.
type sample struct {
	ttft          time.Duration
	firstSentence time.Duration
	tokens        int
	generation    time.Duration
}

// Run sends Warmup and then N requests sequentially and builds the report. A
// failed request is counted in Report.Errors and the run goes on; only an error
// that is not a provider failure (the end of ctx) aborts the run.
func (b *Bench) Run(ctx context.Context) (*Report, error) {
	report := &Report{
		Service:        serviceName,
		Backend:        b.cfg.Backend,
		Model:          b.cfg.Model,
		GitSHA:         b.cfg.GitSHA,
		N:              b.cfg.N,
		Warmup:         b.cfg.Warmup,
		Hardware:       b.cfg.Hardware,
		StartedAt:      b.now().UTC().Format(time.RFC3339),
		ResponseModels: []string{},
		Errors:         make(map[llm.Reason]int, len(allReasons)),
		MeasuredFrom:   b.cfg.MeasuredFrom,
	}
	for _, reason := range allReasons {
		report.Errors[reason] = 0
	}

	samples := make([]sample, 0, b.cfg.N)
	total := b.cfg.Warmup + b.cfg.N
	for i := 1; i <= total; i++ {
		phase := phaseMeasured
		if i <= b.cfg.Warmup {
			phase = phaseWarmup
		}

		result, recorder, err := b.request(ctx, i)
		if result != nil && result.Usage != nil && result.Usage.PromptTokens > 0 {
			report.PromptTokens = maxPointer(report.PromptTokens, result.Usage.PromptTokens)
		}

		var failure *llm.Error
		switch {
		case errors.As(err, &failure):
			report.Errors[failure.Reason]++
			fmt.Fprintf(b.diag, diagFailureFormat, i, phase, err)
			continue
		case err != nil:
			return nil, fmt.Errorf(errAbortFormat, i, err)
		}

		if phase == phaseWarmup {
			continue
		}
		report.ResponseModels = appendDistinct(report.ResponseModels, result.Model)
		samples = append(samples, recorder.sample(result))
	}

	b.summarize(report, samples)
	return report, nil
}

// request sends one request and records when every piece of the text came.
func (b *Bench) request(ctx context.Context, i int) (*llm.Result, *chunkRecorder, error) {
	recorder := &chunkRecorder{now: b.now}
	req := &llm.Request{RequestID: fmt.Sprintf(requestIDFormat, i), Model: b.cfg.Model, Messages: b.cfg.Messages}
	result, err := b.client.Complete(ctx, req, recorder.onText)
	return result, recorder, err
}

// summarize computes the statistics of the successful measured requests.
func (b *Bench) summarize(report *Report, samples []sample) {
	if len(samples) == 0 {
		return
	}

	ttft := make([]float64, len(samples))
	firstSentence := make([]float64, len(samples))
	tokens := 0
	generation := time.Duration(0)
	for i, s := range samples {
		ttft[i] = milliseconds(s.ttft)
		firstSentence[i] = milliseconds(s.firstSentence)
		tokens += s.tokens
		generation += s.generation
	}

	report.P50MS = roundedPointer(stats.Percentile(ttft, quantileP50))
	report.P95MS = roundedPointer(stats.Percentile(ttft, quantileP95))
	report.MeanMS = roundedPointer(stats.Mean(ttft))
	report.StdMS = roundedPointer(sampleStd(ttft))
	report.FirstSentenceP50MS = roundedPointer(stats.Percentile(firstSentence, quantileP50))
	report.FirstSentenceP95MS = roundedPointer(stats.Percentile(firstSentence, quantileP95))
	if generation > 0 {
		report.TokensPerS = roundedPointer(float64(tokens) / generation.Seconds())
	}
}

// chunkRecorder keeps the end offset of the text and the arrival time of every piece.
type chunkRecorder struct {
	now    func() time.Time
	text   strings.Builder
	chunks []chunk
}

// chunk is a piece of the text: where the text ended after it and when it came.
type chunk struct {
	end int
	at  time.Time
}

// onText is the TextHandler of the client.
func (c *chunkRecorder) onText(text string) error {
	c.text.WriteString(text)
	c.chunks = append(c.chunks, chunk{end: c.text.Len(), at: c.now()})
	return nil
}

// sample turns a successful result into the measured values. The end of the first
// sentence is the arrival of the piece with its final mark; a text without one ends with the last piece.
func (c *chunkRecorder) sample(result *llm.Result) sample {
	s := sample{ttft: result.TTFT, tokens: len(c.chunks)}
	if result.Usage != nil && result.Usage.CompletionTokens > 0 {
		s.tokens = result.Usage.CompletionTokens
	}
	if len(c.chunks) == 0 {
		return s
	}

	first := c.chunks[0].at
	last := c.chunks[len(c.chunks)-1].at
	end := firstSentenceEnd(c.text.String())
	for _, piece := range c.chunks {
		if piece.end >= end {
			s.firstSentence = piece.at.Sub(first)
			break
		}
	}
	s.generation = max(result.Duration-result.TTFT, last.Sub(first))
	return s
}

// firstSentenceEnd returns the byte offset after the final mark of the first
// sentence: a mark of sentenceEnds followed by a space or the end of the text.
// A text without such a mark is one sentence.
func firstSentenceEnd(text string) int {
	for i, r := range text {
		if !strings.ContainsRune(sentenceEnds, r) {
			continue
		}
		end := i + utf8.RuneLen(r)
		if end == len(text) {
			return end
		}
		next, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsSpace(next) {
			return end
		}
	}
	return len(text)
}

// sampleStd is the sample standard deviation (n-1); fewer than two values give 0.
func sampleStd(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	mean := stats.Mean(values)
	sum := 0.0
	for _, value := range values {
		sum += (value - mean) * (value - mean)
	}
	return math.Sqrt(sum / float64(len(values)-1))
}

// milliseconds converts a duration to fractional milliseconds.
func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// roundedPointer rounds to 0.1 as the gateway writes durations (PROTO-12).
func roundedPointer(value float64) *float64 {
	rounded := math.Round(value*10) / 10
	return &rounded
}

// maxPointer returns the larger of the current value and the new one.
func maxPointer(current *int, value int) *int {
	if current != nil && *current >= value {
		return current
	}
	return &value
}

// appendDistinct adds the value if it is not in the list yet.
func appendDistinct(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
