package loadgen

import (
	"slices"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// degradedReasons are the keys of "degraded", present even with no answers (as in /v1/stats).
var degradedReasons = []string{stats.DegradedLLMTimeout, stats.DegradedLLMUnavailable, stats.DegradedTTSUnavailable}

// RunResult is what one run gave.
type RunResult struct {
	Run        int
	StartedAt  time.Time
	FinishedAt time.Time
	Records    []Record
}

// SummaryConfig are the parameters of the runs the summary reports.
type SummaryConfig struct {
	Sessions      int
	PauseMS       int
	DescriberMode string // template, llm or vlm: llm_fallback_share counts only the llm and vlm modes
	ConfigIDs     []string
}

// Summarize builds the summary of the runs (PLT-19). Percentiles, means and
// llm_fallback_share follow the rules of /v1/stats (package stats).
func Summarize(cfg SummaryConfig, runs []RunResult) *Summary {
	summary := &Summary{
		ConfigIDs:             append([]string{}, cfg.ConfigIDs...),
		Sessions:              cfg.Sessions,
		PauseMS:               cfg.PauseMS,
		Runs:                  []RunSummary{},
		Spread:                Spread{Stages: map[string]Range{}},
		LLMFallbackShareLimit: LLMFallbackShareLimit,
	}

	all := []Record{}
	for _, run := range runs {
		summary.Runs = append(summary.Runs, RunSummary{
			Run:        run.Run,
			StartedAt:  formatTime(run.StartedAt),
			FinishedAt: formatTime(run.FinishedAt),
			Totals:     totals(run.Records, cfg.DescriberMode),
		})
		all = append(all, run.Records...)
	}

	summary.Pooled.Totals = totals(all, cfg.DescriberMode)
	if len(runs) > 0 {
		summary.Pooled.StartedAt = formatTime(runs[0].StartedAt)
		summary.Pooled.FinishedAt = formatTime(runs[len(runs)-1].FinishedAt)
	}

	summary.Spread = spread(summary.Runs)

	fallbacks, total := stats.CountFallbacks(answers(all, cfg.DescriberMode))
	if total > 0 {
		// The comparison of EXP-8 is integer, the limit is included: fallbacks*100 <= 10*total.
		ok := fallbacks*100 <= int(LLMFallbackShareLimit*100)*total
		summary.LLMFallbackShareOK = &ok
	}
	return summary
}

// totals computes the figures of the records.
func totals(records []Record, describerMode string) Totals {
	t := Totals{
		Requests: map[string]int{},
		Errors:   map[string]int{},
		Stages:   map[string]stats.Summary{},
		Degraded: map[string]stats.Share{},
	}
	for _, scenario := range Scenarios {
		t.Requests[scenario] = 0
	}

	firstAudio := []float64{}
	stages := map[string][]float64{}
	degraded := map[string]int{}
	answered := 0
	for i := range records {
		record := &records[i]
		t.Requests[record.Kind]++
		if record.Status == StatusError {
			t.Errors[record.ErrorCode]++
		}
		if record.FirstAudioMS != nil {
			firstAudio = append(firstAudio, float64(*record.FirstAudioMS))
		}
		for stage, value := range record.Timings {
			stages[stage] = append(stages[stage], float64(value))
		}
		if record.Answered() {
			answered++
			if record.DegradedReason != "" {
				degraded[record.DegradedReason]++
			}
		}
	}

	t.FirstAudio = stats.Summarize(firstAudio)
	for stage, values := range stages {
		t.Stages[stage] = stats.Summarize(values)
	}
	for _, reason := range degradedReasons {
		share := stats.Share{N: degraded[reason]}
		if answered > 0 {
			share.Share = float64(share.N) / float64(answered)
		}
		t.Degraded[reason] = share
	}
	t.LLMFallbackShare = stats.FallbackShare(answers(records, describerMode))
	return t
}

// answers are the answer.end of the records as llm_fallback_share sees them.
func answers(records []Record, describerMode string) []stats.Answer {
	result := []stats.Answer{}
	for i := range records {
		if !records[i].Answered() {
			continue
		}
		result = append(result, stats.Answer{
			Kind:           records[i].Kind,
			Intent:         records[i].Intent,
			DescriberMode:  describerMode,
			DegradedReason: records[i].DegradedReason,
		})
	}
	return result
}

// spread is the minimum and the maximum of p50 and p95 over the runs that have values.
func spread(runs []RunSummary) Spread {
	result := Spread{Stages: map[string]Range{}}

	firstAudio := []stats.Summary{}
	stages := map[string][]stats.Summary{}
	for _, run := range runs {
		if run.FirstAudio.N > 0 {
			firstAudio = append(firstAudio, run.FirstAudio)
		}
		for stage, summary := range run.Stages {
			if summary.N > 0 {
				stages[stage] = append(stages[stage], summary)
			}
		}
	}

	result.FirstAudio = rangeOf(firstAudio)
	for stage, summaries := range stages {
		result.Stages[stage] = rangeOf(summaries)
	}
	return result
}

// rangeOf is the spread of the summaries; no summaries give zeros.
func rangeOf(summaries []stats.Summary) Range {
	if len(summaries) == 0 {
		return Range{}
	}
	p50 := make([]float64, 0, len(summaries))
	p95 := make([]float64, 0, len(summaries))
	for _, summary := range summaries {
		p50 = append(p50, summary.P50)
		p95 = append(p95, summary.P95)
	}
	return Range{P50Min: slices.Min(p50), P50Max: slices.Max(p50), P95Min: slices.Min(p95), P95Max: slices.Max(p95)}
}

// formatTime writes a time in UTC with milliseconds.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// timeLayout is RFC 3339 with milliseconds.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"
