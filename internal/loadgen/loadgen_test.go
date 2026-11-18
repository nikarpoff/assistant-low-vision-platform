package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/app"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/servicestub"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/session"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// Settings of the tests.
const (
	testToken   = "device-token-1"
	testModel   = "qwen/qwen3-8b"
	testTimeout = 10 * time.Second
	// testDataset is the embedded mini data set: 5 snapshots and 2 questions.
	testDataset = "../../testdata/minidataset"
	// testRequests is the number of requests of a run over the mini data set: 5 describe, 2 ask, 5 passive.
	testRequests = 12
)

// gateway is the whole gateway in-process over the service stubs and the provider stub.
type gateway struct {
	app *app.App
	asr *servicestub.ASR
	// handler serves the sessions; tests may put another gateway in front of it.
	handler http.Handler
}

// newGateway starts the gateway from the environment, as cmd/gateway does.
func newGateway(t *testing.T, llmConfig fakellm.Config, environment map[string]string) *gateway {
	t.Helper()
	detector := servicestub.NewDetector()
	t.Cleanup(detector.Close)
	tts := servicestub.NewTTS()
	t.Cleanup(tts.Close)
	g := &gateway{asr: servicestub.NewASR()}
	t.Cleanup(g.asr.Close)
	llmServer := httptest.NewServer(fakellm.NewServer(llmConfig))
	t.Cleanup(llmServer.Close)

	variables := map[string]string{
		"APP_DEVICE_TOKENS":  testToken,
		"APP_ADMIN_TOKEN":    "admin-token-1",
		"APP_DESCRIBER_MODE": "llm",
		"APP_LLM_URL":        llmServer.URL + fakellm.BasePath,
		"APP_LLM_MODEL":      testModel,
		"APP_DETECTOR_URL":   detector.URL,
		"APP_ASR_URL":        g.asr.URL,
		"APP_TTS_URL":        tts.URL,
	}
	for name, value := range environment {
		variables[name] = value
	}
	for name, value := range variables {
		t.Setenv(name, value)
	}
	vars, err := env.Load()
	require.NoError(t, err)

	g.app, err = app.NewApp(vars)
	require.NoError(t, err)
	require.NoError(t, g.app.Start(context.Background()))
	t.Cleanup(g.app.Stop)
	select {
	case <-g.app.Ready():
	case <-time.After(testTimeout):
		t.Fatal("gateway is not ready")
	}
	g.handler = g.app.Handler()
	return g
}

// serve publishes the handler and returns the ws:// address of its sessions.
func serve(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + session.Path
}

// result is what a run of the command gave.
type result struct {
	code     int
	stderr   string
	lines    []map[string]any // JSONL as is
	records  []Record
	summary  *Summary
	document map[string]any // summary as is
}

// runLoadgen runs the command over the mini data set without warmup unless args set it.
func runLoadgen(t *testing.T, url string, args ...string) result {
	t.Helper()
	out := filepath.Join(t.TempDir(), "requests.jsonl")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	all := append([]string{"--url", url, "--token", testToken, "--dataset", testDataset, "--out", out, "--warmup", "0", "--timeout-ms", "10000"}, args...)

	r := result{code: Main(context.Background(), all, stdout, stderr), stderr: stderr.String()}

	body, err := os.ReadFile(out)
	require.NoError(t, err)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := map[string]any{}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line))
		r.lines = append(r.lines, line)
		var record Record
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &record))
		r.records = append(r.records, record)
	}
	if stdout.Len() > 0 {
		r.summary = &Summary{}
		require.NoError(t, json.Unmarshal(stdout.Bytes(), r.summary), stdout.String())
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &r.document))
	}
	return r
}

// keys returns the sorted keys of a JSON object.
func keys(t *testing.T, value any) []string {
	t.Helper()
	object, ok := value.(map[string]any)
	require.True(t, ok, "%v is not an object", value)
	result := make([]string, 0, len(object))
	for key := range object {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// sorted returns the strings in ascending order.
func sorted(values ...string) []string {
	result := slices.Clone(values)
	sort.Strings(result)
	return result
}

// millis returns the value of a JSONL duration.
func millis(t *testing.T, line map[string]any, key string) float64 {
	t.Helper()
	value, ok := line[key].(float64)
	require.True(t, ok, "%s is %v", key, line[key])
	return value
}

// rounded rounds to 0.1 as stats.Summarize does.
func rounded(value float64) float64 {
	return math.Round(value*10) / 10
}

func TestLoadgen(t *testing.T) {
	var (
		g       *gateway
		url     string
		getLogs func() string
	)

	setup := func(t *testing.T, llmConfig fakellm.Config) {
		getLogs = logger.InitTestLogger()
		g = newGateway(t, llmConfig, nil)
		url = serve(t, g.handler)
	}

	t.Run("must_pass_runs", func(t *testing.T) {
		setup(t, fakellm.Config{})

		r := runLoadgen(t, url, "--runs", "2", "--warmup", "3", "--sessions", "2", "--pause-ms", "5")

		require.Equal(t, ExitOK, r.code, r.stderr)
		// The warmup is not recorded: two full passes of the manifest.
		require.Len(t, r.records, 2*testRequests)
		for _, run := range []int{1, 2} {
			kinds := map[string]int{}
			for _, record := range r.records {
				if record.Run == run {
					kinds[record.Kind]++
					require.NotEqual(t, StatusError, record.Status, record)
				}
			}
			require.Equal(t, map[string]int{ScenarioDescribe: 5, ScenarioAsk: 2, ScenarioPassive: 5}, kinds)
		}
		require.Len(t, r.summary.Runs, 2)
		require.Equal(t, 1, r.summary.Runs[0].Run)
		require.Equal(t, 2, r.summary.Runs[1].Run)
		require.Equal(t, 2, r.summary.Sessions)
		require.Equal(t, 5, r.summary.PauseMS)
		require.Contains(t, r.stderr, fmt.Sprintf(diagRunFormat, 1, testRequests, 0))
		require.Contains(t, r.stderr, fmt.Sprintf(diagRunFormat, 2, testRequests, 0))
		require.NotContains(t, getLogs(), "ERROR")
	})

	t.Run("must_pass_first_run", func(t *testing.T) {
		setup(t, fakellm.Config{})

		r := runLoadgen(t, url, "--runs", "2", "--first-run", "4")

		require.Equal(t, ExitOK, r.code, r.stderr)
		runs := map[int]int{}
		for _, line := range r.lines {
			runs[int(line["run"].(float64))]++
		}
		require.Equal(t, map[int]int{4: testRequests, 5: testRequests}, runs)
		require.Equal(t, 4, r.summary.Runs[0].Run)
		require.Equal(t, 5, r.summary.Runs[1].Run)
	})

	t.Run("must_pass_jsonl_fields", func(t *testing.T) {
		setup(t, fakellm.Config{})

		// The pause lets the gateway release the previous request: otherwise a passive one may be skipped (PROTO-8).
		r := runLoadgen(t, url, "--pause-ms", "20")

		require.Equal(t, ExitOK, r.code, r.stderr)
		require.Len(t, r.lines, testRequests)
		for _, line := range r.lines {
			// Fields of PLT-19 and what score and the strict check need.
			require.Equal(t, sorted("run", "kind", "image_id", "question_id", "request_id", "config_id", "sent_at",
				"accepted_ms", "first_audio_ms", "end_ms", "timings", "text", "intent", "status", "degraded",
				"degraded_reason", "error_code", "audio_bytes"), keys(t, line))
			require.Equal(t, float64(1), line["run"])
			require.Equal(t, r.summary.ConfigIDs[0], line["config_id"])
			_, err := uuid.Parse(line["request_id"].(string))
			require.NoError(t, err)
			sentAt, err := time.Parse(time.RFC3339Nano, line["sent_at"].(string))
			require.NoError(t, err)
			require.True(t, strings.HasSuffix(line["sent_at"].(string), "Z"), "sent_at is not UTC")
			require.WithinDuration(t, time.Now(), sentAt, time.Minute)
			require.Equal(t, false, line["degraded"])
			require.Equal(t, "", line["degraded_reason"])
			require.Equal(t, "", line["error_code"])
			require.NotNil(t, line["end_ms"])
			if line["status"] == StatusSkipped {
				// A skipped passive request is answered at once, without accepted (PROTO-8).
				require.Nil(t, line["accepted_ms"])
				continue
			}
			require.NotNil(t, line["accepted_ms"], "%v", line)
			require.LessOrEqual(t, millis(t, line, "accepted_ms"), millis(t, line, "end_ms"))
		}

		describe, ask := r.lines[0], r.lines[5]
		require.Equal(t, ScenarioDescribe, describe["kind"])
		require.Equal(t, float64(1), describe["image_id"])
		require.Nil(t, describe["question_id"])
		require.Equal(t, StatusOK, describe["status"])
		require.Equal(t, fakellm.DefaultText, describe["text"])
		require.Equal(t, "describe", describe["intent"])
		require.Positive(t, describe["audio_bytes"])
		require.LessOrEqual(t, millis(t, describe, "accepted_ms"), millis(t, describe, "first_audio_ms"))
		require.LessOrEqual(t, millis(t, describe, "first_audio_ms"), millis(t, describe, "end_ms"))
		timings := describe["timings"].(map[string]any)
		for _, stage := range []string{"receive_ms", "detect_ms", "llm_ttft_ms", "tts_first_chunk_ms", "server_ttfa_ms", "server_total_ms"} {
			require.Contains(t, timings, stage)
		}

		require.Equal(t, ScenarioAsk, ask["kind"])
		require.Equal(t, "q001", ask["question_id"])
		require.Equal(t, float64(3), ask["image_id"])
		require.Equal(t, StatusOK, ask["status"])
		require.Equal(t, "question", ask["intent"])
		require.Contains(t, ask["timings"].(map[string]any), "asr_ms")

		// Passive: the stairs of the detector stub give one warning, then the cooldown clears the rest.
		warnings := 0
		for _, line := range r.lines[7:] {
			require.Equal(t, ScenarioPassive, line["kind"])
			if line["status"] == StatusOK {
				warnings++
				require.Positive(t, line["audio_bytes"])
				require.NotNil(t, line["first_audio_ms"])
				continue
			}
			require.Contains(t, []any{StatusClear, StatusSkipped}, line["status"])
			require.Nil(t, line["first_audio_ms"])
			require.Empty(t, line["timings"])
		}
		require.Equal(t, 1, warnings)
	})

	t.Run("must_pass_summary", func(t *testing.T) {
		// The provider fails: every scene description falls back to the template.
		setup(t, fakellm.Config{Fail: fakellm.FailHTTP500})

		r := runLoadgen(t, url, "--runs", "2", "--pause-ms", "1")

		require.Equal(t, ExitOK, r.code, r.stderr)
		require.Len(t, r.summary.ConfigIDs, 1)
		require.Equal(t, 1, r.summary.Sessions)
		require.Equal(t, 1, r.summary.PauseMS)
		require.Equal(t, LLMFallbackShareLimit, r.summary.LLMFallbackShareLimit)
		require.NotNil(t, r.summary.LLMFallbackShareOK)
		require.False(t, *r.summary.LLMFallbackShareOK)

		check := func(totals Totals, records []Record, runs int) {
			require.Equal(t, map[string]int{ScenarioDescribe: 5 * runs, ScenarioAsk: 2 * runs, ScenarioPassive: 5 * runs}, totals.Requests)
			require.Equal(t, map[string]int{}, totals.Errors)

			firstAudio, stages := []float64{}, map[string][]float64{}
			answered, fallbacks := 0, 0
			for _, record := range records {
				if record.FirstAudioMS != nil {
					firstAudio = append(firstAudio, float64(*record.FirstAudioMS))
				}
				for stage, value := range record.Timings {
					stages[stage] = append(stages[stage], float64(value))
				}
				if record.Answered() {
					answered++
				}
				if record.DegradedReason == stats.DegradedLLMUnavailable {
					fallbacks++
				}
			}
			// 5 describe and 2 questions fell back; passive warnings do not call the language model.
			require.Equal(t, 7*runs, fallbacks)
			require.Equal(t, stats.Summarize(firstAudio), totals.FirstAudio)
			require.Equal(t, len(firstAudio), totals.FirstAudio.N)
			require.Len(t, totals.Stages, len(stages))
			for stage, values := range stages {
				require.Equal(t, stats.Summarize(values), totals.Stages[stage], stage)
			}
			require.Equal(t, map[string]stats.Share{
				stats.DegradedLLMTimeout:     {N: 0, Share: 0},
				stats.DegradedLLMUnavailable: {N: fallbacks, Share: float64(fallbacks) / float64(answered)},
				stats.DegradedTTSUnavailable: {N: 0, Share: 0},
			}, totals.Degraded)
			require.NotNil(t, totals.LLMFallbackShare)
			require.Equal(t, 1.0, *totals.LLMFallbackShare)
		}

		pooled := []Record{}
		for i, run := range r.summary.Runs {
			records := []Record{}
			for _, record := range r.records {
				if record.Run == run.Run {
					records = append(records, record)
				}
			}
			check(run.Totals, records, 1)
			pooled = append(pooled, records...)

			startedAt, err := time.Parse(time.RFC3339Nano, run.StartedAt)
			require.NoError(t, err)
			finishedAt, err := time.Parse(time.RFC3339Nano, run.FinishedAt)
			require.NoError(t, err)
			require.False(t, finishedAt.Before(startedAt))
			if i > 0 {
				require.Equal(t, r.summary.Runs[i-1].FinishedAt <= run.StartedAt, true)
			}
		}
		check(r.summary.Pooled.Totals, pooled, 2)
		require.Equal(t, r.summary.Runs[0].StartedAt, r.summary.Pooled.StartedAt)
		require.Equal(t, r.summary.Runs[1].FinishedAt, r.summary.Pooled.FinishedAt)

		first, second := r.summary.Runs[0], r.summary.Runs[1]
		require.Equal(t, Range{
			P50Min: min(first.FirstAudio.P50, second.FirstAudio.P50),
			P50Max: max(first.FirstAudio.P50, second.FirstAudio.P50),
			P95Min: min(first.FirstAudio.P95, second.FirstAudio.P95),
			P95Max: max(first.FirstAudio.P95, second.FirstAudio.P95),
		}, r.summary.Spread.FirstAudio)
		detect := r.summary.Spread.Stages["detect_ms"]
		require.Equal(t, rounded(min(first.Stages["detect_ms"].P95, second.Stages["detect_ms"].P95)), detect.P95Min)
		require.Equal(t, rounded(max(first.Stages["detect_ms"].P95, second.Stages["detect_ms"].P95)), detect.P95Max)
		require.Len(t, r.summary.Spread.Stages, len(r.summary.Pooled.Stages))

		// In the template mode the language model is not called: llm_fallback_share has no data.
		r = runLoadgen(t, url, "--scenarios", "describe", "--describer-mode", "template")

		require.Equal(t, ExitOK, r.code, r.stderr)
		require.Nil(t, r.summary.Pooled.LLMFallbackShare)
		require.Nil(t, r.summary.LLMFallbackShareOK)
		require.Nil(t, r.document["llm_fallback_share_ok"])
		require.Equal(t, map[string]int{ScenarioDescribe: 5, ScenarioAsk: 0, ScenarioPassive: 0}, r.summary.Pooled.Requests)
	})

	t.Run("must_pass_summary_keys", func(t *testing.T) {
		setup(t, fakellm.Config{})

		r := runLoadgen(t, url, "--runs", "2")

		require.Equal(t, ExitOK, r.code, r.stderr)
		require.Equal(t, sorted("config_ids", "sessions", "pause_ms", "runs", "pooled", "spread",
			"llm_fallback_share_limit", "llm_fallback_share_ok"), keys(t, r.document))
		totals := []string{"requests", "errors", "first_audio", "stages", "degraded", "llm_fallback_share"}
		runs := r.document["runs"].([]any)
		require.Len(t, runs, 2)
		pooled := r.document["pooled"]
		require.Equal(t, sorted(append([]string{"started_at", "finished_at"}, totals...)...), keys(t, pooled))
		summaryKeys := sorted("n", "p50", "p95", "mean")
		rangeKeys := sorted("p50_min", "p50_max", "p95_min", "p95_max")
		for _, value := range append(runs, pooled) {
			object := value.(map[string]any)
			if _, ok := object["run"]; ok {
				require.Equal(t, sorted(append([]string{"run", "started_at", "finished_at"}, totals...)...), keys(t, object))
			}
			require.Equal(t, sorted(ScenarioDescribe, ScenarioAsk, ScenarioPassive), keys(t, object["requests"]))
			require.Empty(t, keys(t, object["errors"]))
			require.Equal(t, summaryKeys, keys(t, object["first_audio"]))
			require.Contains(t, keys(t, object["stages"]), "server_total_ms")
			for _, stage := range object["stages"].(map[string]any) {
				require.Equal(t, summaryKeys, keys(t, stage))
			}
			require.Equal(t, sorted(stats.DegradedLLMTimeout, stats.DegradedLLMUnavailable, stats.DegradedTTSUnavailable), keys(t, object["degraded"]))
			for _, share := range object["degraded"].(map[string]any) {
				require.Equal(t, sorted("n", "share"), keys(t, share))
			}
			require.Equal(t, 0.0, object["llm_fallback_share"])
		}
		spread := r.document["spread"]
		require.Equal(t, sorted("first_audio", "stages"), keys(t, spread))
		require.Equal(t, rangeKeys, keys(t, spread.(map[string]any)["first_audio"]))
		for _, stage := range spread.(map[string]any)["stages"].(map[string]any) {
			require.Equal(t, rangeKeys, keys(t, stage))
		}
		require.Equal(t, 0.1, r.document["llm_fallback_share_limit"])
		require.Equal(t, true, r.document["llm_fallback_share_ok"])
	})

	t.Run("must_pass_config_id_change", func(t *testing.T) {
		setup(t, fakellm.Config{})
		before := newGateway(t, fakellm.Config{}, map[string]string{"APP_PLACEMENT": "A"})
		after := newGateway(t, fakellm.Config{}, map[string]string{"APP_PLACEMENT": "B"})
		// Every run opens its own session: the first one meets the gateway before the change, the next ones after it.
		var sessions atomic.Int32
		url = serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sessions.Add(1) == 1 {
				before.handler.ServeHTTP(w, r)
				return
			}
			after.handler.ServeHTTP(w, r)
		}))

		r := runLoadgen(t, url, "--runs", "2")

		require.Equal(t, ExitOK, r.code, r.stderr)
		require.Len(t, r.summary.ConfigIDs, 2)
		require.NotEqual(t, r.summary.ConfigIDs[0], r.summary.ConfigIDs[1])
		for _, record := range r.records {
			require.Equal(t, r.summary.ConfigIDs[record.Run-1], record.ConfigID)
		}
		require.Equal(t, int32(2), sessions.Load())
	})

	t.Run("must_pass_no_client_metrics", func(t *testing.T) {
		setup(t, fakellm.Config{})

		r := runLoadgen(t, url)

		require.Equal(t, ExitOK, r.code, r.stderr)
		// The requests reached the gateway, but no E2E came: alv_e2e_duration_seconds is fed only by client.metrics.
		require.Positive(t, testutil.CollectAndCount(g.app.Metrics().Requests))
		require.Equal(t, 0, testutil.CollectAndCount(g.app.Metrics().E2EDuration))
	})

	t.Run("must_fail_strict_on_error", func(t *testing.T) {
		setup(t, fakellm.Config{})
		g.asr.Fail(true)

		r := runLoadgen(t, url, "--scenarios", "ask,describe", "--strict")

		require.Equal(t, ExitFailed, r.code)
		require.Contains(t, r.stderr, fmt.Sprintf(diagStrictFormat, 2))
		require.Equal(t, map[string]int{"asr_unavailable": 2}, r.summary.Pooled.Errors)
		for _, record := range r.records {
			if record.Kind == ScenarioAsk {
				require.Equal(t, StatusError, record.Status)
				require.Equal(t, "asr_unavailable", record.ErrorCode)
				require.NotNil(t, record.EndMS)
			}
		}

		// The same run without --strict reports the errors but succeeds.
		r = runLoadgen(t, url, "--scenarios", "ask")
		require.Equal(t, ExitOK, r.code, r.stderr)
		require.Equal(t, map[string]int{"asr_unavailable": 2}, r.summary.Pooled.Errors)

		// An answer without audio fails the strict check too.
		require.Equal(t, 1, StrictFailures([]RunResult{{Records: []Record{
			{Status: StatusOK, Text: "Впереди стол.", Timings: map[string]protocol.Millis{"server_total_ms": 1}},
			{Status: StatusOK, Text: "", Intent: "stop"},
			{Status: StatusClear},
		}}}))
	})
}

func TestLoadgenFlags(t *testing.T) {
	var (
		out    string
		stderr *bytes.Buffer
	)

	setup := func() {
		out = filepath.Join(t.TempDir(), "requests.jsonl")
		stderr = &bytes.Buffer{}
	}

	t.Run("must_pass_defaults", func(t *testing.T) {
		setup()
		t.Setenv(tokenEnv, "token-from-env")

		opts, err := parseFlags([]string{"--out", out}, stderr)

		require.NoError(t, err)
		require.Equal(t, &options{
			url:           defaultURL,
			token:         "token-from-env",
			dataset:       defaultDataset,
			scenarios:     []string{ScenarioDescribe, ScenarioAsk, ScenarioPassive},
			sessions:      1,
			runs:          1,
			firstRun:      1,
			warmup:        5,
			timeoutMS:     defaultTimeoutMS,
			describerMode: "llm",
			out:           out,
		}, opts)
	})

	cases := map[string]struct {
		args []string
		err  string
	}{
		"must_fail_url":         {[]string{"--url", "http://host/v1/session"}, fmt.Sprintf(errURLFormat, "http://host/v1/session")},
		"must_fail_out":         {[]string{"--out", ""}, errOutRequired},
		"must_fail_scenario":    {[]string{"--scenarios", "describe,walk"}, fmt.Sprintf(errScenarioFormat, "walk")},
		"must_fail_no_scenario": {[]string{"--scenarios", " , "}, errScenariosEmpty},
		"must_fail_mode":        {[]string{"--describer-mode", "gpt"}, fmt.Sprintf(errModeFormat, "gpt")},
		"must_fail_runs":        {[]string{"--runs", "0"}, fmt.Sprintf(errPositiveFormat, flagRuns, 0)},
		"must_fail_first_run":   {[]string{"--first-run", "0"}, fmt.Sprintf(errPositiveFormat, flagFirstRun, 0)},
		"must_fail_sessions":    {[]string{"--sessions", "0"}, fmt.Sprintf(errPositiveFormat, flagSessions, 0)},
		"must_fail_pause":       {[]string{"--pause-ms", "-1"}, fmt.Sprintf(errNegativeFormat, flagPauseMS, -1)},
		"must_fail_warmup":      {[]string{"--warmup", "-1"}, fmt.Sprintf(errNegativeFormat, flagWarmup, -1)},
		"must_fail_arguments":   {[]string{"extra"}, fmt.Sprintf(errArgumentsFormat, "extra")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setup()

			_, err := parseFlags(append([]string{"--out", out}, tc.args...), stderr)

			require.EqualError(t, err, tc.err)
		})
	}

	t.Run("must_fail_usage_exit_code", func(t *testing.T) {
		setup()

		code := Main(context.Background(), []string{"--runs", "0", "--out", out}, &bytes.Buffer{}, stderr)

		require.Equal(t, ExitUsage, code)
		require.Equal(t, fmt.Sprintf(diagErrorFormat, fmt.Sprintf(errPositiveFormat, flagRuns, 0)), stderr.String())
	})

	t.Run("must_fail_connection", func(t *testing.T) {
		setup()

		code := Main(context.Background(), []string{"--url", "ws://127.0.0.1:1/v1/session", "--dataset", testDataset, "--out", out, "--warmup", "1"}, &bytes.Buffer{}, stderr)

		require.Equal(t, ExitFailed, code)
		require.Contains(t, stderr.String(), "warmup failed: failed to connect to ws://127.0.0.1:1/v1/session")
	})
}

func TestLoadDataset(t *testing.T) {
	var dir string

	setup := func(files map[string]string) {
		dir = t.TempDir()
		for name, content := range files {
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
		}
	}

	t.Run("must_pass_minidataset", func(t *testing.T) {
		dataset, err := LoadDataset(testDataset)

		require.NoError(t, err)
		require.Len(t, dataset.Images, 5)
		scenes := map[string]int{}
		for _, image := range dataset.Images {
			scenes[image.Scene]++
			require.Equal(t, 640, max(image.Width, image.Height))
		}
		// EXP-1 mini data set: at least 2 snapshots indoors and 2 outdoors.
		require.GreaterOrEqual(t, scenes["indoor"], 2)
		require.GreaterOrEqual(t, scenes["outdoor"], 2)
		require.Len(t, dataset.Questions, 2)
		for _, question := range dataset.Questions {
			require.NotNil(t, question.ImageID)
			require.Greater(t, question.AudioMS(), 500.0)
			require.Less(t, question.AudioMS(), 15000.0)
		}
	})

	t.Run("must_pass_scenes_of_fake_detector", func(t *testing.T) {
		dataset, err := LoadDataset(testDataset)
		require.NoError(t, err)

		// The fake detector picks its scene by the first 8 bytes of SHA-256 modulo 3; e2e needs all three.
		scenes := map[int]bool{}
		for _, image := range dataset.Images {
			scenes[fakeScene(image.Data)] = true
		}
		require.Equal(t, map[int]bool{0: true, 1: true, 2: true}, scenes)
	})

	cases := map[string]struct {
		files map[string]string
		err   func() string
	}{
		"must_fail_no_annotations": {nil, func() string {
			path := filepath.Join(dir, annotationsFile)
			return fmt.Errorf(errReadFileFormat, path, fmt.Errorf("open %s: no such file or directory", path)).Error()
		}},
		"must_fail_no_images": {map[string]string{annotationsFile: `{"images":[]}`}, func() string { return errNoImages }},
		"must_fail_image_fields": {map[string]string{annotationsFile: `{"images":[{"id":7,"file_name":"a.jpg"}]}`}, func() string {
			return fmt.Sprintf(errImageFieldsFormat, 7)
		}},
		"must_fail_not_jpeg": {map[string]string{annotationsFile: `{"images":[{"id":1,"file_name":"a.jpg","width":1,"height":1}]}`, "a.jpg": "text"}, func() string {
			return fmt.Sprintf(errNotJPEGFormat, filepath.Join(dir, "a.jpg"))
		}},
		"must_fail_question_image": {map[string]string{
			annotationsFile: `{"images":[{"id":1,"file_name":"a.jpg","width":1,"height":1}]}`,
			"a.jpg":         "\xff\xd8\xff",
			questionsFile:   `{"id":"q1","audio":"q1.wav","image_id":2}`,
		}, func() string { return fmt.Sprintf(errQuestionImageFormat, "q1", 2) }},
		"must_fail_not_wav": {map[string]string{
			annotationsFile: `{"images":[{"id":1,"file_name":"a.jpg","width":1,"height":1}]}`,
			"a.jpg":         "\xff\xd8\xff",
			questionsFile:   `{"id":"q1","audio":"q1.wav"}`,
			"q1.wav":        "not a wave file",
		}, func() string { return fmt.Sprintf(errNotWAVFormat, filepath.Join(dir, "q1.wav")) }},
		"must_fail_wav_format": {map[string]string{
			annotationsFile: `{"images":[{"id":1,"file_name":"a.jpg","width":1,"height":1}]}`,
			"a.jpg":         "\xff\xd8\xff",
			questionsFile:   `{"id":"q1","audio":"q1.wav"}`,
			"q1.wav":        string(wavHeader(44100)),
		}, func() string { return fmt.Sprintf(errWAVFormatFormat, filepath.Join(dir, "q1.wav")) }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setup(tc.files)

			_, err := LoadDataset(dir)

			require.EqualError(t, err, tc.err())
		})
	}
}

// wavHeader is a WAV file of PCM s16le mono with the given rate and 2 samples.
func wavHeader(rate uint32) []byte {
	var buffer bytes.Buffer
	write := func(values ...any) {
		for _, value := range values {
			_ = binary.Write(&buffer, binary.LittleEndian, value)
		}
	}
	buffer.WriteString("RIFF")
	write(uint32(36 + 4))
	buffer.WriteString("WAVEfmt ")
	write(uint32(16), uint16(1), uint16(1), rate, rate*2, uint16(2), uint16(16))
	buffer.WriteString("data")
	write(uint32(4), int16(1), int16(-1))
	return buffer.Bytes()
}

// fakeScene is the scene the fake detector of objects-detector picks for the snapshot.
func fakeScene(data []byte) int {
	sum := sha256.Sum256(data)
	return int(binary.BigEndian.Uint64(sum[:8]) % 3)
}
