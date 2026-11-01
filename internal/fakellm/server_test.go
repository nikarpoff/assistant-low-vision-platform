package fakellm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

const (
	testBody          = `{"model":"qwen/qwen3-8b","stream":true,"messages":[{"role":"system","content":"Ты помощник."},{"role":"user","content":"Опиши обстановку"}]}`
	testFirstSentence = "Впереди стол и два стула."
	// testErrorEventFormat is the error event of the spec (LLM-1) with the code and the message.
	testErrorEventFormat = `data: {"id":"error_fake","object":"chat.completion.chunk","error":{"code":"%s","message":"%s"},"trace_id":"fake-trace","choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`
	// testErrorBodyFormat is the error body before the stream in the Polza.ai format.
	testErrorBodyFormat = `{"error":{"code":"%s","message":"%s","trace_id":"fake-trace"},"trace_id":"fake-trace"}`
)

// testChunk is a chunk of the stream as the tests read it.
type testChunk struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint"`
	Choices           []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chunkUsage `json:"usage"`
}

// testStream is a stream read by the tests.
type testStream struct {
	raw     string
	lines   []string
	chunks  []testChunk
	text    string
	readErr error
}

func TestFakeLLM(t *testing.T) {
	var (
		stub    *Server
		server  *httptest.Server
		getLogs func() string
	)

	setup := func(t *testing.T, cfg Config) {
		getLogs = logger.InitTestLogger()
		stub = NewServer(cfg)
		server = httptest.NewServer(stub)
		t.Cleanup(server.Close)
	}

	post := func(t *testing.T, body, key string) *http.Response {
		request, err := http.NewRequest(http.MethodPost, server.URL+PathChatCompletions, strings.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}

	requireStreamEnd := func(t *testing.T, stream *testStream) {
		require.NoError(t, stream.readErr)
		require.Equal(t, ": PROCESSING", stream.lines[0])
		require.Equal(t, "data: [DONE]", stream.lines[len(stream.lines)-1])
		require.True(t, strings.HasSuffix(stream.raw, "data: [DONE]\n\n"))
	}

	requireErrorBody := func(t *testing.T, response *http.Response, status int, code, message string) {
		require.Equal(t, status, response.StatusCode)
		require.Equal(t, "application/json", response.Header.Get("Content-Type"))
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.JSONEq(t, fmt.Sprintf(testErrorBodyFormat, code, message), string(body))
		require.Len(t, stub.Requests(), 1)
	}

	t.Run("must_pass_stream_polza_format", func(t *testing.T) {
		setup(t, Config{})

		response := post(t, testBody, "")
		stream := readStream(t, response.Body)

		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
		requireStreamEnd(t, stream)
		for _, line := range stream.lines[1 : len(stream.lines)-1] {
			require.True(t, strings.HasPrefix(line, "data: {"), line)
			require.Contains(t, stream.raw, line+"\n\n")
		}
		for _, chunk := range stream.chunks {
			require.Equal(t, "chat.completion.chunk", chunk.Object)
			require.Equal(t, DefaultModel, chunk.Model)
			require.Equal(t, SystemFingerprint, chunk.SystemFingerprint)
		}
		first := stream.chunks[0]
		require.Equal(t, "assistant", first.Choices[0].Delta.Role)
		require.Nil(t, first.Choices[0].FinishReason)
		finish := stream.chunks[len(stream.chunks)-2]
		require.Equal(t, "stop", *finish.Choices[0].FinishReason)
		usage := stream.chunks[len(stream.chunks)-1]
		require.NotNil(t, usage.Choices)
		require.Empty(t, usage.Choices)
		require.NotNil(t, usage.Usage)
		require.Equal(t, len(splitTokens(DefaultText)), usage.Usage.CompletionTokens)
		require.Equal(t, usage.Usage.PromptTokens+usage.Usage.CompletionTokens, usage.Usage.TotalTokens)
		require.Equal(t, DefaultText, stream.text)
		require.Greater(t, len(stream.chunks), 4)
	})

	t.Run("must_pass_models", func(t *testing.T) {
		setup(t, Config{Model: "qwen/qwen3-test"})

		response, err := http.Get(server.URL + PathModels)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)

		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Equal(t, "application/json", response.Header.Get("Content-Type"))
		require.JSONEq(t, `{"object":"list","data":[{"id":"qwen/qwen3-test","object":"model","created":0,"owned_by":"fakellm"}]}`, string(body))
	})

	t.Run("must_pass_model_and_text", func(t *testing.T) {
		setup(t, Config{Model: "qwen/qwen3-test", Text: "Справа дверь! Больше ничего."})

		stream := readStream(t, post(t, testBody, "").Body)

		requireStreamEnd(t, stream)
		require.Equal(t, "Справа дверь! Больше ничего.", stream.text)
		for _, chunk := range stream.chunks {
			require.Equal(t, "qwen/qwen3-test", chunk.Model)
		}
	})

	t.Run("must_pass_records_requests", func(t *testing.T) {
		setup(t, Config{APIKey: "test-key"})

		response := post(t, testBody, "test-key")
		_ = readStream(t, response.Body)
		requests := stub.Requests()

		require.Len(t, requests, 1)
		require.Equal(t, http.MethodPost, requests[0].Method)
		require.Equal(t, PathChatCompletions, requests[0].Path)
		require.Equal(t, "Bearer test-key", requests[0].Header.Get("Authorization"))
		require.Equal(t, testBody, string(requests[0].Body))

		stub.Reset()

		require.Empty(t, stub.Requests())
	})

	t.Run("must_pass_recorded_requests_bounded", func(t *testing.T) {
		setup(t, Config{})

		for i := 0; i < maxRecordedRequests+10; i++ {
			stub.record(RecordedRequest{Path: fmt.Sprintf("/%d", i)})
		}
		requests := stub.Requests()

		require.Len(t, requests, maxRecordedRequests)
		require.Equal(t, "/10", requests[0].Path)
		require.Equal(t, fmt.Sprintf("/%d", maxRecordedRequests+9), requests[len(requests)-1].Path)
	})

	t.Run("must_pass_first_token_delay", func(t *testing.T) {
		setup(t, Config{FirstTokenDelayMS: 100, TokenDelayMS: 10})
		started := time.Now()

		response := post(t, testBody, "")
		reader := bufio.NewReader(response.Body)
		var roleAt, contentAt time.Duration
		for contentAt == 0 {
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			switch {
			case strings.Contains(line, `"role":"assistant"`):
				roleAt = time.Since(started)
			case strings.Contains(line, `"content":"`):
				contentAt = time.Since(started)
			}
		}

		require.NotZero(t, roleAt)
		require.Greater(t, contentAt, roleAt)
		require.GreaterOrEqual(t, contentAt, 100*time.Millisecond)
	})

	t.Run("must_fail_unauthorized", func(t *testing.T) {
		setup(t, Config{APIKey: "test-key"})

		noKey := post(t, testBody, "")
		wrongKey := post(t, testBody, "wrong-key")
		models, err := http.Get(server.URL + PathModels)
		require.NoError(t, err)
		defer models.Body.Close()
		withKey := post(t, testBody, "test-key")

		for _, response := range []*http.Response{noKey, wrongKey, models} {
			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.JSONEq(t, fmt.Sprintf(testErrorBodyFormat, CodeUnauthorized, msgUnauthorized), string(body))
		}
		require.Equal(t, http.StatusOK, withKey.StatusCode)
		require.Len(t, stub.Requests(), 4)
		require.Contains(t, getLogs(), "WARNING: "+fmt.Sprintf(logUnauthorizedFormat, PathChatCompletions))
		require.NotContains(t, getLogs(), "test-key")
	})

	t.Run("must_fail_invalid_body", func(t *testing.T) {
		setup(t, Config{})

		response := post(t, `{"stream":`, "")

		requireErrorBody(t, response, http.StatusBadRequest, CodeBadRequest, msgInvalidBody)
	})

	t.Run("must_fail_stream_false", func(t *testing.T) {
		setup(t, Config{})

		response := post(t, `{"stream":false,"messages":[]}`, "")

		requireErrorBody(t, response, http.StatusBadRequest, CodeBadRequest, msgStreamRequired)
	})

	// One case per value of FAKELLM_FAIL
	t.Run("must_pass_fail_none", func(t *testing.T) {
		setup(t, Config{Fail: FailNone})

		response := post(t, testBody, "")
		stream := readStream(t, response.Body)

		require.Equal(t, http.StatusOK, response.StatusCode)
		requireStreamEnd(t, stream)
		require.Equal(t, DefaultText, stream.text)
		require.NotContains(t, stream.raw, `"error"`)
	})

	t.Run("must_fail_http_402", func(t *testing.T) {
		setup(t, Config{Fail: FailHTTP402})

		response := post(t, testBody, "")

		requireErrorBody(t, response, http.StatusPaymentRequired, CodeInsufficientBalance, msgInsufficientBalance)
	})

	t.Run("must_fail_http_429", func(t *testing.T) {
		setup(t, Config{Fail: FailHTTP429})

		response := post(t, testBody, "")

		require.Equal(t, "1", response.Header.Get("Retry-After"))
		requireErrorBody(t, response, http.StatusTooManyRequests, CodeTooManyRequests, msgTooManyRequests)
	})

	t.Run("must_fail_http_500", func(t *testing.T) {
		setup(t, Config{Fail: FailHTTP500})

		response := post(t, testBody, "")

		requireErrorBody(t, response, http.StatusInternalServerError, CodeInternalError, msgInternalError)
	})

	t.Run("must_fail_stream_error_before_text", func(t *testing.T) {
		setup(t, Config{Fail: FailStreamErrorBeforeText})

		response := post(t, testBody, "")
		stream := readStream(t, response.Body)

		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Equal(t, []string{
			": PROCESSING",
			fmt.Sprintf(testErrorEventFormat, CodeServiceUnavailable, msgProviderDisconnected),
			"data: [DONE]",
		}, stream.lines)
		require.Empty(t, stream.text)
	})

	t.Run("must_fail_stream_error_after_sentence", func(t *testing.T) {
		setup(t, Config{Fail: FailStreamErrorAfterSentence})

		response := post(t, testBody, "")
		stream := readStream(t, response.Body)

		require.Equal(t, http.StatusOK, response.StatusCode)
		requireStreamEnd(t, stream)
		require.Equal(t, testFirstSentence, stream.text)
		require.Equal(t, fmt.Sprintf(testErrorEventFormat, CodeServiceUnavailable, msgProviderDisconnected), stream.lines[len(stream.lines)-2])
		require.NotContains(t, stream.raw, `"finish_reason":"stop"`)
	})

	t.Run("must_fail_stream_402_after_sentence", func(t *testing.T) {
		setup(t, Config{Fail: FailStream402AfterSentence})

		response := post(t, testBody, "")
		stream := readStream(t, response.Body)

		require.Equal(t, http.StatusOK, response.StatusCode)
		requireStreamEnd(t, stream)
		require.Equal(t, testFirstSentence, stream.text)
		require.Equal(t, fmt.Sprintf(testErrorEventFormat, CodeInsufficientBalance, msgInsufficientBalance), stream.lines[len(stream.lines)-2])
	})

	t.Run("must_fail_disconnect_after_sentence", func(t *testing.T) {
		setup(t, Config{Fail: FailDisconnectAfterSentence})

		response := post(t, testBody, "")
		stream := readStream(t, response.Body)

		require.Equal(t, http.StatusOK, response.StatusCode)
		require.ErrorIs(t, stream.readErr, io.ErrUnexpectedEOF)
		require.Equal(t, testFirstSentence, stream.text)
		require.NotContains(t, stream.raw, "[DONE]")
		require.NotContains(t, stream.raw, `"finish_reason":"`)
	})

	t.Run("must_fail_empty", func(t *testing.T) {
		setup(t, Config{Fail: FailEmpty})

		response := post(t, testBody, "")
		stream := readStream(t, response.Body)

		require.Equal(t, http.StatusOK, response.StatusCode)
		requireStreamEnd(t, stream)
		require.Len(t, stream.lines, 3)
		require.Len(t, stream.chunks, 1)
		require.Equal(t, "stop", *stream.chunks[0].Choices[0].FinishReason)
		require.Nil(t, stream.chunks[0].Choices[0].Delta.Content)
		require.Empty(t, stream.text)
	})
}

func TestSplitTokens(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected []string
	}{
		{name: "words", text: "Впереди стол.", expected: []string{"Впереди", " стол."}},
		{name: "spaces", text: " Слева  дверь ", expected: []string{" Слева", "  дверь", " "}},
		{name: "empty", text: "", expected: nil},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("must_pass_%s", tt.name), func(t *testing.T) {
			tokens := splitTokens(tt.text)

			require.Equal(t, tt.expected, tokens)
			require.Equal(t, tt.text, strings.Join(tokens, ""))
		})
	}
}

func TestFirstSentence(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected string
	}{
		{name: "dot", text: DefaultText, expected: testFirstSentence},
		{name: "ellipsis", text: "Темно… Ничего не вижу.", expected: "Темно…"},
		{name: "abbreviation_without_space", text: "Стол 1.5 метра. Дверь.", expected: "Стол 1.5 метра."},
		{name: "no_end", text: "Впереди стол", expected: "Впереди стол"},
		{name: "single_sentence", text: "Впереди стол.", expected: "Впереди стол."},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("must_pass_%s", tt.name), func(t *testing.T) {
			require.Equal(t, tt.expected, firstSentence(tt.text))
		})
	}
}

// readStream reads the whole SSE body: the non-empty lines, the data chunks and the joined text.
func readStream(t *testing.T, body io.Reader) *testStream {
	raw, readErr := io.ReadAll(body)
	stream := &testStream{raw: string(raw), readErr: readErr}

	for _, line := range strings.Split(stream.raw, "\n") {
		if line == "" {
			continue
		}
		stream.lines = append(stream.lines, line)
		if !strings.HasPrefix(line, "data: {") || strings.Contains(line, `"error"`) {
			continue
		}

		var chunk testChunk
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk))
		stream.chunks = append(stream.chunks, chunk)
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				stream.text += *choice.Delta.Content
			}
		}
	}
	return stream
}
