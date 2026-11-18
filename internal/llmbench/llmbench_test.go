package llmbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// testPromptPath is the reference prompt from the package directory.
const testPromptPath = "../../testdata/llmbench/prompt.json"

// Settings of the tests.
const (
	testModel    = "qwen/qwen3-8b"
	testVLMModel = "qwen/qwen2.5-vl-7b-instruct"
	testAPIKey   = "sk-test-key"
)

// testImage is a small fake JPEG snapshot: SOI, a marker and EOI.
var testImage = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x02, 0x03, 0xFF, 0xD9}

// reportKeys are the keys of the report: EXP-2 and the fields of PLT-19.
var reportKeys = []string{
	"service", "backend", "model", "device", "git_sha", "n", "warmup",
	"p50_ms", "p95_ms", "mean_ms", "std_ms", "hardware", "started_at",
	"first_sentence_p50_ms", "first_sentence_p95_ms", "tokens_per_s", "prompt_tokens",
	"response_models", "errors", "measured_from",
}

// benchRun is the outcome of one run of the command.
type benchRun struct {
	code   int
	stdout string
	stderr string
	report map[string]any
}

// requestBody is the part of a recorded request body the tests check.
type requestBody struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func TestLLMBench(t *testing.T) {
	var (
		stub     *fakellm.Server
		server   *httptest.Server
		requests atomic.Int64
		prompt   *PromptFile
	)

	// setup starts the stub behind the handler made by wrap (nil - the stub itself)
	setup := func(cfg fakellm.Config, wrap func(http.Handler) http.Handler) {
		logger.InitTestLogger()
		requests.Store(0)

		cfg.APIKey = testAPIKey
		stub = fakellm.NewServer(cfg)
		var handler http.Handler = stub
		if wrap != nil {
			handler = wrap(stub)
		}
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)

		t.Setenv("APP_LLM_URL", server.URL+fakellm.BasePath)
		t.Setenv("APP_LLM_API_KEY", testAPIKey)
		t.Setenv("APP_LLM_MODEL", testModel)
		t.Setenv("APP_VLM_MODEL", "")
		t.Setenv("APP_LLM_EXTRA_BODY", "{}")
		t.Setenv("APP_LLM_TTFT_TIMEOUT_MS", "900")
		t.Setenv("APP_LLM_TOTAL_TIMEOUT_MS", "5000")

		var err error
		prompt, err = LoadPromptFile(testPromptPath)
		require.NoError(t, err)
	}

	// runBench runs the command and decodes the report from stdout, if there is one
	runBench := func(args ...string) benchRun {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr, testPromptPath)
		result := benchRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
		if stdout.Len() > 0 {
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &result.report), stdout.String())
		}
		return result
	}

	// number reads a number field of the report
	number := func(report map[string]any, key string) float64 {
		value, ok := report[key].(float64)
		require.True(t, ok, "%s is not a number: %v", key, report[key])
		return value
	}

	// bodies decodes the bodies of the chat completion requests the stub received
	bodies := func() []requestBody {
		var result []requestBody
		for _, recorded := range stub.Requests() {
			if recorded.Path != fakellm.PathChatCompletions {
				continue
			}
			body := requestBody{}
			require.NoError(t, json.Unmarshal(recorded.Body, &body))
			result = append(result, body)
			require.Equal(t, "Bearer "+testAPIKey, recorded.Header.Get("Authorization"))
		}
		return result
	}

	t.Run("must_pass_json_schema", func(t *testing.T) {
		setup(fakellm.Config{}, nil)

		result := runBench("--warmup", "1", "--n", "3")
		require.Equal(t, ExitOK, result.code, result.stderr)
		require.Empty(t, result.stderr)
		require.Equal(t, 1, strings.Count(result.stdout, "\n"))
		require.True(t, strings.HasSuffix(result.stdout, "\n"))

		// Exactly the keys of EXP-2 and PLT-19
		keys := make([]string, 0, len(result.report))
		for key := range result.report {
			keys = append(keys, key)
		}
		expected := append([]string(nil), reportKeys...)
		sort.Strings(keys)
		sort.Strings(expected)
		require.Equal(t, expected, keys)

		report := result.report
		require.Equal(t, "llm", report["service"])
		require.Equal(t, "127.0.0.1", report["backend"])
		require.Equal(t, testModel, report["model"])
		require.Nil(t, report["device"])
		require.NotEmpty(t, report["git_sha"])
		require.Equal(t, 3.0, number(report, "n"))
		require.Equal(t, 1.0, number(report, "warmup"))
		for _, key := range []string{"p50_ms", "p95_ms", "mean_ms", "std_ms", "first_sentence_p50_ms", "first_sentence_p95_ms"} {
			require.GreaterOrEqual(t, number(report, key), 0.0, key)
		}
		require.LessOrEqual(t, number(report, "p50_ms"), number(report, "p95_ms"))
		require.LessOrEqual(t, number(report, "first_sentence_p50_ms"), number(report, "first_sentence_p95_ms"))
		require.Greater(t, number(report, "tokens_per_s"), 0.0)
		require.Greater(t, number(report, "prompt_tokens"), 0.0)

		hardware, ok := report["hardware"].(map[string]any)
		require.True(t, ok)
		require.Len(t, hardware, 2)
		require.NotEmpty(t, hardware["cpu"])
		require.Contains(t, hardware, "gpu")
		require.Nil(t, hardware["gpu"])

		startedAt, ok := report["started_at"].(string)
		require.True(t, ok)
		parsed, err := time.Parse(time.RFC3339, startedAt)
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(startedAt, "Z"))
		require.WithinDuration(t, time.Now(), parsed, time.Minute)

		require.Equal(t, []any{fakellm.DefaultModel}, report["response_models"])
		expectedErrors := map[string]any{}
		for _, reason := range allReasons {
			expectedErrors[string(reason)] = 0.0
		}
		require.Equal(t, expectedErrors, report["errors"])
		hostname, err := os.Hostname()
		require.NoError(t, err)
		require.Equal(t, hostname, report["measured_from"])

		// Every request is a stream of the llm prompt: the reference messages, no image
		received := bodies()
		require.Len(t, received, 4)
		for _, body := range received {
			require.Equal(t, testModel, body.Model)
			require.True(t, body.Stream)
			require.Len(t, body.Messages, len(prompt.Messages))
			for i, message := range body.Messages {
				var content string
				require.NoError(t, json.Unmarshal(message.Content, &content))
				require.Equal(t, prompt.Messages[i].Role, message.Role)
				require.Equal(t, prompt.Messages[i].Content, content)
			}
		}
		for _, recorded := range stub.Requests() {
			require.NotContains(t, string(recorded.Body), "image_url")
		}

		// --url and --model override APP_LLM_URL and APP_LLM_MODEL; --out writes the report to a file
		t.Setenv("APP_LLM_URL", "http://invalid.invalid/v1")
		out := filepath.Join(t.TempDir(), "llmbench.json")
		result = runBench("--url", server.URL+fakellm.BasePath+"/", "--model", "other/model", "--warmup", "0", "--n", "1", "--out", out)
		require.Equal(t, ExitOK, result.code, result.stderr)
		require.Empty(t, result.stdout)
		written, err := os.ReadFile(out)
		require.NoError(t, err)
		fileReport := map[string]any{}
		require.NoError(t, json.Unmarshal(written, &fileReport))
		require.Equal(t, "other/model", fileReport["model"])
		require.Equal(t, "127.0.0.1", fileReport["backend"])
	})

	t.Run("must_pass_warmup_excluded", func(t *testing.T) {
		const (
			warmup     = 2
			n          = 3
			warmupWait = 300 * time.Millisecond
		)
		var calls atomic.Int64
		// The warmup requests are slow; the measured ones are not
		setup(fakellm.Config{}, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) <= warmup {
					time.Sleep(warmupWait)
				}
				next.ServeHTTP(w, r)
			})
		})

		result := runBench("--warmup", fmt.Sprint(warmup), "--n", fmt.Sprint(n))
		require.Equal(t, ExitOK, result.code, result.stderr)
		require.Equal(t, int64(warmup+n), requests.Load())
		require.Len(t, bodies(), warmup+n)

		report := result.report
		require.Equal(t, float64(n), number(report, "n"))
		require.Equal(t, float64(warmup), number(report, "warmup"))
		require.Less(t, number(report, "p95_ms"), float64(warmupWait.Milliseconds()))
		require.Less(t, number(report, "mean_ms"), float64(warmupWait.Milliseconds()))
	})

	t.Run("must_pass_first_sentence", func(t *testing.T) {
		const (
			firstTokenDelayMS = 60
			tokenDelayMS      = 40
			// The first sentence is two tokens: its end comes one token delay after the first token
			text   = "Впереди переход. Слева человек, близко, справа машина, далеко."
			tokens = 8
		)
		setup(fakellm.Config{FirstTokenDelayMS: firstTokenDelayMS, TokenDelayMS: tokenDelayMS, Text: text}, nil)

		result := runBench("--warmup", "0", "--n", "3")
		require.Equal(t, ExitOK, result.code, result.stderr)

		report := result.report
		require.GreaterOrEqual(t, number(report, "p50_ms"), float64(firstTokenDelayMS))
		require.GreaterOrEqual(t, number(report, "first_sentence_p50_ms"), float64(tokenDelayMS))
		require.GreaterOrEqual(t, number(report, "first_sentence_p95_ms"), number(report, "first_sentence_p50_ms"))
		// Far from the end of the whole answer, seven token delays after the first token
		require.Less(t, number(report, "first_sentence_p95_ms"), float64((tokens-1)*tokenDelayMS))
		// The generation of 8 tokens takes at least 7 token delays
		require.Greater(t, number(report, "tokens_per_s"), 0.0)
		require.LessOrEqual(t, number(report, "tokens_per_s"), float64(tokens)/(float64((tokens-1)*tokenDelayMS)/1000)+0.1)

		// The end of the first sentence: a final mark followed by a space or the end of the text
		require.Equal(t, len("Впереди переход."), firstSentenceEnd(text))
		require.Equal(t, len("Дверь в 2.5 метрах."), firstSentenceEnd("Дверь в 2.5 метрах. Слева стул."))
		require.Equal(t, len("Слева стул"), firstSentenceEnd("Слева стул"))
		require.Equal(t, len("Осторожно!"), firstSentenceEnd("Осторожно!"))
	})

	t.Run("must_pass_image_flag", func(t *testing.T) {
		setup(fakellm.Config{}, nil)
		t.Setenv("APP_VLM_MODEL", testVLMModel)
		imagePath := filepath.Join(t.TempDir(), "scene.jpg")
		require.NoError(t, os.WriteFile(imagePath, testImage, 0o600))

		// Without --model the vlm model is APP_VLM_MODEL
		result := runBench("--warmup", "0", "--n", "2", "--image", imagePath)
		require.Equal(t, ExitOK, result.code, result.stderr)
		require.Equal(t, testVLMModel, result.report["model"])

		dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(testImage)
		received := bodies()
		require.Len(t, received, 2)
		for i, body := range received {
			require.Equal(t, testVLMModel, body.Model)
			require.Equal(t, 1, strings.Count(string(stub.Requests()[i].Body), `"image_url":{`))
			require.Len(t, body.Messages, len(prompt.ImageMessages))
			last := len(body.Messages) - 1
			for j := 0; j < last; j++ {
				var content string
				require.NoError(t, json.Unmarshal(body.Messages[j].Content, &content))
				require.Equal(t, prompt.ImageMessages[j].Content, content)
			}

			// The user message carries the vlm text and the snapshot as a data URL, as the describer sends them
			var parts []llm.ContentPart
			require.NoError(t, json.Unmarshal(body.Messages[last].Content, &parts))
			require.Equal(t, []llm.ContentPart{
				{Type: llm.ContentPartText, Text: prompt.ImageMessages[last].Content},
				{Type: llm.ContentPartImageURL, ImageURL: &llm.ImageURL{URL: dataURL}},
			}, parts)
		}

		// --model wins over APP_VLM_MODEL
		stub.Reset()
		result = runBench("--warmup", "0", "--n", "1", "--image", imagePath, "--model", "other/vl")
		require.Equal(t, ExitOK, result.code, result.stderr)
		require.Equal(t, "other/vl", bodies()[0].Model)

		// Not a JPEG: no request, a non-zero exit code
		stub.Reset()
		textPath := filepath.Join(t.TempDir(), "scene.txt")
		require.NoError(t, os.WriteFile(textPath, []byte("not an image"), 0o600))
		result = runBench("--warmup", "0", "--n", "1", "--image", textPath)
		require.Equal(t, ExitFailed, result.code)
		require.Empty(t, result.stdout)
		require.Equal(t, fmt.Sprintf(diagErrorFormat, fmt.Errorf(errImageFormat, textPath)), result.stderr)
		require.Empty(t, stub.Requests())
	})

	t.Run("must_fail_exit_code_on_error", func(t *testing.T) {
		cases := []struct {
			fail   fakellm.FailMode
			reason llm.Reason
		}{
			{fakellm.FailHTTP402, llm.ReasonInsufficientBalance},
			{fakellm.FailHTTP429, llm.ReasonHTTP429},
			{fakellm.FailHTTP500, llm.ReasonHTTP5xx},
			{fakellm.FailStreamErrorBeforeText, llm.ReasonStreamError},
			{fakellm.FailStreamErrorAfterSentence, llm.ReasonStreamError},
			{fakellm.FailStream402AfterSentence, llm.ReasonInsufficientBalance},
			{fakellm.FailDisconnectAfterSentence, llm.ReasonDisconnect},
		}
		for _, c := range cases {
			setup(fakellm.Config{Fail: c.fail}, nil)

			result := runBench("--warmup", "1", "--n", "2")
			require.Equal(t, ExitFailed, result.code, c.fail)
			errorCounts, ok := result.report["errors"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, 3.0, errorCounts[string(c.reason)], c.fail)
			require.Nil(t, result.report["p95_ms"], c.fail)
			require.Equal(t, []any{}, result.report["response_models"], c.fail)
			require.Contains(t, result.stderr, fmt.Sprintf(diagFailedFormat, 3, 3))
			require.Contains(t, result.stderr, "llmbench: request 1 (warmup) failed: language model request failed: reason="+string(c.reason))
			require.Contains(t, result.stderr, "llmbench: request 3 (measured) failed: language model request failed: reason="+string(c.reason))
		}

		// A first token later than APP_LLM_TTFT_TIMEOUT_MS is a failure, as in the gateway
		setup(fakellm.Config{FirstTokenDelayMS: 200}, nil)
		t.Setenv("APP_LLM_TTFT_TIMEOUT_MS", "50")
		result := runBench("--warmup", "0", "--n", "1")
		require.Equal(t, ExitFailed, result.code)
		require.Equal(t, 1.0, result.report["errors"].(map[string]any)[string(llm.ReasonTimeoutTTFT)])

		// A failure of the warmup alone also gives a non-zero code; the measured requests are reported
		var calls atomic.Int64
		setup(fakellm.Config{}, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		result = runBench("--warmup", "1", "--n", "2")
		require.Equal(t, ExitFailed, result.code)
		require.Equal(t, 1.0, result.report["errors"].(map[string]any)[string(llm.ReasonHTTP5xx)])
		require.NotNil(t, result.report["p95_ms"])
		require.Contains(t, result.stderr, fmt.Sprintf(diagFailedFormat, 1, 3))

		// The provider is not reachable
		setup(fakellm.Config{}, nil)
		server.Close()
		result = runBench("--warmup", "0", "--n", "1")
		require.Equal(t, ExitFailed, result.code)
		require.Equal(t, 1.0, result.report["errors"].(map[string]any)[string(llm.ReasonDisconnect)])

		// Invalid flags and settings: no request, a non-zero code
		setup(fakellm.Config{}, nil)
		result = runBench("--n", "0")
		require.Equal(t, ExitUsage, result.code)
		require.Equal(t, fmt.Sprintf(diagErrorFormat, fmt.Errorf(errNFormat, 0)), result.stderr)
		result = runBench("--warmup", "-1")
		require.Equal(t, ExitUsage, result.code)
		require.Equal(t, fmt.Sprintf(diagErrorFormat, fmt.Errorf(errWarmupFormat, -1)), result.stderr)
		result = runBench("extra")
		require.Equal(t, ExitUsage, result.code)
		require.Equal(t, fmt.Sprintf(diagErrorFormat, fmt.Errorf(errArgumentsFormat, "extra")), result.stderr)
		t.Setenv("APP_LLM_MODEL", "")
		result = runBench("--n", "1")
		require.Equal(t, ExitFailed, result.code)
		require.Equal(t, fmt.Sprintf(diagErrorFormat, errors.New(errModelRequired)), result.stderr)
		result = runBench("--n", "1", "--model", testModel, "--url", "ftp://example.org")
		require.Equal(t, ExitFailed, result.code)
		require.Contains(t, result.stderr, "language model URL must be an absolute http or https URL")
		require.Empty(t, stub.Requests())
	})

	t.Run("must_fail_empty_response", func(t *testing.T) {
		setup(fakellm.Config{Fail: fakellm.FailEmpty}, nil)

		result := runBench("--warmup", "1", "--n", "2")
		require.Equal(t, ExitFailed, result.code)
		require.Equal(t, 3.0, result.report["errors"].(map[string]any)[string(llm.ReasonEmpty)])
		require.Nil(t, result.report["p50_ms"])
		require.Nil(t, result.report["first_sentence_p95_ms"])
		require.Nil(t, result.report["tokens_per_s"])
		require.Contains(t, result.stderr, "reason="+string(llm.ReasonEmpty))
		require.Contains(t, result.stderr, fmt.Sprintf(diagFailedFormat, 3, 3))

		// A text of spaces only is empty too
		setup(fakellm.Config{Text: "   "}, nil)
		result = runBench("--warmup", "0", "--n", "1")
		require.Equal(t, ExitFailed, result.code)
		require.Equal(t, 1.0, result.report["errors"].(map[string]any)[string(llm.ReasonEmpty)])
	})
}
