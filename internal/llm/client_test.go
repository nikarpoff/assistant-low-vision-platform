package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
)

const (
	testAPIKey        = "test-key"
	testRequestID     = "req-7f3c"
	testModel         = "qwen/qwen3-8b"
	testExtraBody     = `{"reasoning":{"enabled":false},"top_p":0.9}`
	testFirstSentence = "Впереди стол и два стула."
	testTraceID       = "test-trace"
)

// testErrorEventFormat is an error event of the stream with a code given as JSON (a string or a number).
const testErrorEventFormat = `data: {"id":"error_test","object":"chat.completion.chunk","error":{"code":%s,"message":"Недостаточно средств"},"trace_id":"test-trace","choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}` + "\n\n"

func TestLLMClient(t *testing.T) {
	var (
		m        *metrics.Metrics
		cfg      Config
		client   *Client
		stub     *fakellm.Server
		server   *httptest.Server
		requests atomic.Int32
		request  *Request
		texts    []string
		getLogs  func() string
	)

	// setup serves handler (the stub when it is nil) and creates the client; configure changes its settings
	setup := func(t *testing.T, stubCfg fakellm.Config, handler http.Handler, configure func(*Config)) {
		getLogs = logger.InitTestLogger()
		m = metrics.NewMetrics()
		stub = fakellm.NewServer(stubCfg)
		if handler == nil {
			handler = stub
		}
		requests.Store(0)
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)

		cfg = Config{
			URL:          server.URL + fakellm.BasePath,
			APIKey:       testAPIKey,
			Temperature:  0,
			Seed:         42,
			MaxTokens:    96,
			ExtraBody:    testExtraBody,
			TTFTTimeout:  900 * time.Millisecond,
			TotalTimeout: 5 * time.Second,
		}
		if configure != nil {
			configure(&cfg)
		}
		var err error
		client, err = NewClient(cfg, m)
		require.NoError(t, err)

		request = &Request{
			RequestID: testRequestID,
			Model:     testModel,
			Messages: []Message{
				{Role: RoleSystem, Content: "Ты помощник незрячего человека."},
				{Role: RoleUser, Content: "Опиши обстановку"},
			},
		}
		texts = nil
	}

	collect := func(text string) error {
		texts = append(texts, text)
		return nil
	}

	// requireFailure checks the error, the single request, the metric and the log line of a failure
	requireFailure := func(t *testing.T, err error, category error, reason Reason, status int, code, traceID string) {
		var failure *Error
		require.ErrorAs(t, err, &failure)
		require.Equal(t, reason, failure.Reason)
		require.Equal(t, status, failure.StatusCode)
		require.Equal(t, code, failure.Code)
		require.Equal(t, traceID, failure.TraceID)
		require.ErrorIs(t, err, category)
		require.EqualError(t, err, fmt.Sprintf(errorFormat, reason, status, code, traceID))
		require.Equal(t, int32(1), requests.Load())
		require.Equal(t, 1, testutil.CollectAndCount(m.UpstreamErrors))
		require.Equal(t, 1.0, testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(upstreamLabel, string(reason))))
		require.Contains(t, getLogs(), fmt.Sprintf("WARNING: language model request failed: request_id=%s reason=%s status=%d", testRequestID, reason, status))
	}

	t.Run("must_pass_request_body_and_headers", func(t *testing.T) {
		setup(t, fakellm.Config{APIKey: testAPIKey}, nil, nil)

		result, err := client.Complete(context.Background(), request, collect)

		require.NoError(t, err)
		require.Equal(t, fakellm.DefaultText, result.Text)
		recorded := stub.Requests()
		require.Len(t, recorded, 1)
		require.Equal(t, http.MethodPost, recorded[0].Method)
		require.Equal(t, fakellm.PathChatCompletions, recorded[0].Path)
		require.JSONEq(t, `{
			"model": "qwen/qwen3-8b",
			"messages": [
				{"role": "system", "content": "Ты помощник незрячего человека."},
				{"role": "user", "content": "Опиши обстановку"}
			],
			"stream": true,
			"temperature": 0,
			"seed": 42,
			"max_tokens": 96,
			"reasoning": {"enabled": false},
			"top_p": 0.9
		}`, string(recorded[0].Body))
		require.Equal(t, "Bearer "+testAPIKey, recorded[0].Header.Get("Authorization"))
		require.Equal(t, "application/json", recorded[0].Header.Get("Content-Type"))
		require.Equal(t, "text/event-stream", recorded[0].Header.Get("Accept"))
		for name := range recorded[0].Header {
			require.Contains(t, []string{"Authorization", "Content-Type", "Accept", "Content-Length", "User-Agent"}, name)
		}
		require.Empty(t, recorded[0].Header.Get("X-Request-ID"))
		require.Empty(t, recorded[0].Header.Get("Accept-Encoding"))
		require.NotContains(t, string(recorded[0].Body), testRequestID)
		var body map[string]any
		require.NoError(t, json.Unmarshal(recorded[0].Body, &body))
		require.NotContains(t, body, "user")
		require.NotContains(t, getLogs(), testAPIKey)
	})

	t.Run("must_pass_no_authorization_without_key", func(t *testing.T) {
		setup(t, fakellm.Config{}, nil, func(c *Config) { c.APIKey = "" })

		_, err := client.Complete(context.Background(), request, collect)

		require.NoError(t, err)
		recorded := stub.Requests()
		require.Len(t, recorded, 1)
		_, ok := recorded[0].Header["Authorization"]
		require.False(t, ok)
	})

	t.Run("must_pass_sse_with_comments_and_usage", func(t *testing.T) {
		setup(t, fakellm.Config{}, sseHandler(
			": PROCESSING\r\n\r\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\r\n\r\n",
			": keep-alive\n\n",
			"event: message\n",
			`data:{"id":"c1","object":"chat.completion.chunk","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":"Впереди"},"finish_reason":null}]}`+"\n\n",
			": PROCESSING\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":null},"finish_reason":null}]}`+"\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":" стол."},"finish_reason":null}]}`+"\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen/qwen3-8b","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":3,"total_tokens":123}}`+"\n\n",
			"data: [DONE]\n\n",
		), nil)

		result, err := client.Complete(context.Background(), request, collect)

		require.NoError(t, err)
		require.Equal(t, []string{"Впереди", " стол."}, texts)
		require.Equal(t, "Впереди стол.", result.Text)
		require.Equal(t, &Usage{PromptTokens: 120, CompletionTokens: 3, TotalTokens: 123}, result.Usage)
		require.Equal(t, "stop", result.FinishReason)
		require.Positive(t, result.TTFT)
		require.GreaterOrEqual(t, result.Duration, result.TTFT)
		require.False(t, result.Stopped)
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_pass_stream_from_stub", func(t *testing.T) {
		setup(t, fakellm.Config{TokenDelayMS: 1}, nil, nil)

		result, err := client.Complete(context.Background(), request, collect)

		require.NoError(t, err)
		require.Greater(t, len(texts), 1)
		require.Equal(t, fakellm.DefaultText, strings.Join(texts, ""))
		require.Equal(t, fakellm.DefaultModel, result.Model)
		require.Equal(t, fakellm.SystemFingerprint, result.SystemFingerprint)
		require.Equal(t, len(texts), result.Usage.CompletionTokens)
		require.Equal(t, "stop", result.FinishReason)
		require.Contains(t, getLogs(), fmt.Sprintf("DEBUG: language model answered: request_id=%s model=%s", testRequestID, fakellm.DefaultModel))
	})

	t.Run("must_pass_model_and_fingerprint", func(t *testing.T) {
		setup(t, fakellm.Config{}, sseHandler(
			`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n",
			`data: {"id":"c1","model":"qwen/qwen3-8b-2025","choices":[{"index":0,"delta":{"content":"Слева"},"finish_reason":null}]}`+"\n\n",
			`data: {"id":"c1","model":"other/model","system_fingerprint":"fp_first","choices":[{"index":0,"delta":{"content":" дверь."},"finish_reason":null}]}`+"\n\n",
			`data: {"id":"c1","model":"third/model","system_fingerprint":"fp_second","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`+"\n\n",
			"data: [DONE]\n\n",
		), nil)

		result, err := client.Complete(context.Background(), request, collect)

		require.NoError(t, err)
		require.Equal(t, "qwen/qwen3-8b-2025", result.Model)
		require.Equal(t, "fp_first", result.SystemFingerprint)
		require.Equal(t, "length", result.FinishReason)
		require.Equal(t, "Слева дверь.", result.Text)
	})

	t.Run("must_pass_end_after_finish_reason_without_done", func(t *testing.T) {
		setup(t, fakellm.Config{}, sseHandler(
			`data: {"id":"c1","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":"Впереди стол."},"finish_reason":null}]}`+"\n\n",
			`data: {"id":"c1","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`+"\n\n",
		), nil)

		result, err := client.Complete(context.Background(), request, collect)

		require.NoError(t, err)
		require.Equal(t, "content_filter", result.FinishReason)
		require.Equal(t, "Впереди стол.", result.Text)
	})

	t.Run("must_pass_stop_by_handler", func(t *testing.T) {
		setup(t, fakellm.Config{TokenDelayMS: 20}, nil, nil)

		result, err := client.Complete(context.Background(), request, func(text string) error {
			texts = append(texts, text)
			return ErrStop
		})

		require.NoError(t, err)
		require.True(t, result.Stopped)
		require.Len(t, texts, 1)
		require.Equal(t, texts[0], result.Text)
		require.Equal(t, int32(1), requests.Load())
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_fail_handler_error", func(t *testing.T) {
		setup(t, fakellm.Config{}, nil, nil)

		result, err := client.Complete(context.Background(), request, func(string) error {
			return errors.New("tts queue closed")
		})

		require.EqualError(t, err, "failed to handle language model text: tts queue closed")
		require.NotNil(t, result)
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_fail_canceled_by_caller", func(t *testing.T) {
		setup(t, fakellm.Config{TokenDelayMS: 200}, nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		result, err := client.Complete(ctx, request, func(text string) error {
			texts = append(texts, text)
			cancel()
			return nil
		})

		require.ErrorIs(t, err, ErrCanceled)
		require.ErrorIs(t, err, context.Canceled)
		require.EqualError(t, err, "language model request canceled: context canceled")
		require.Len(t, texts, 1)
		require.Equal(t, texts[0], result.Text)
		require.Equal(t, 0, testutil.CollectAndCount(m.UpstreamErrors))
	})

	t.Run("must_fail_model_required", func(t *testing.T) {
		setup(t, fakellm.Config{}, nil, nil)
		request.Model = ""

		result, err := client.Complete(context.Background(), request, collect)

		require.Nil(t, result)
		require.EqualError(t, err, errModelRequired)
		require.Equal(t, int32(0), requests.Load())
	})

	t.Run("must_fail_stream_error_before_text", func(t *testing.T) {
		setup(t, fakellm.Config{Fail: fakellm.FailStreamErrorBeforeText}, nil, nil)

		result, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrUnavailable, ReasonStreamError, http.StatusOK, fakellm.CodeServiceUnavailable, fakellm.TraceID)
		require.Empty(t, texts)
		require.Empty(t, result.Text)
		require.NotContains(t, getLogs(), "Provider disconnected unexpectedly")
	})

	t.Run("must_fail_stream_error_after_sentence", func(t *testing.T) {
		setup(t, fakellm.Config{Fail: fakellm.FailStreamErrorAfterSentence}, nil, nil)

		result, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrUnavailable, ReasonStreamError, http.StatusOK, fakellm.CodeServiceUnavailable, fakellm.TraceID)
		require.Equal(t, testFirstSentence, strings.Join(texts, ""))
		require.Equal(t, testFirstSentence, result.Text)
		require.Equal(t, fakellm.DefaultModel, result.Model)
		require.Equal(t, fakellm.SystemFingerprint, result.SystemFingerprint)
	})

	t.Run("must_fail_stream_error_finish_reason_only", func(t *testing.T) {
		setup(t, fakellm.Config{}, sseHandler(
			`data: {"id":"c1","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":"Впереди стол."},"finish_reason":null}]}`+"\n\n",
			`data: {"id":"c1","model":"qwen/qwen3-8b","trace_id":"test-trace","choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`+"\n\n",
			"data: [DONE]\n\n",
		), nil)

		result, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrUnavailable, ReasonStreamError, http.StatusOK, "", testTraceID)
		require.Equal(t, "Впереди стол.", result.Text)
	})

	t.Run("must_fail_stream_insufficient_balance", func(t *testing.T) {
		t.Run("must_fail_code_insufficient_balance", func(t *testing.T) {
			setup(t, fakellm.Config{Fail: fakellm.FailStream402AfterSentence}, nil, nil)

			result, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonInsufficientBalance, http.StatusOK, fakellm.CodeInsufficientBalance, fakellm.TraceID)
			require.Equal(t, testFirstSentence, strings.Join(texts, ""))
			require.Equal(t, testFirstSentence, result.Text)
		})

		for name, code := range map[string]string{
			"must_fail_code_402_number":       `402`,
			"must_fail_code_402_string":       `"402"`,
			"must_fail_code_payment_required": `"PAYMENT_REQUIRED"`,
		} {
			t.Run(name, func(t *testing.T) {
				setup(t, fakellm.Config{}, sseHandler(
					": PROCESSING\n\n",
					`data: {"id":"c1","object":"chat.completion.chunk","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":"Впереди стол и два стула."},"finish_reason":null}]}`+"\n\n",
					fmt.Sprintf(testErrorEventFormat, code),
					"data: [DONE]\n\n",
				), nil)

				result, err := client.Complete(context.Background(), request, collect)

				requireFailure(t, err, ErrUnavailable, ReasonInsufficientBalance, http.StatusOK, strings.Trim(code, `"`), testTraceID)
				require.Equal(t, []string{testFirstSentence}, texts)
				require.Equal(t, testFirstSentence, result.Text)
				require.NotContains(t, getLogs(), "Недостаточно средств")
			})
		}
	})

	t.Run("must_fail_http_402", func(t *testing.T) {
		setup(t, fakellm.Config{Fail: fakellm.FailHTTP402}, nil, nil)

		result, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrUnavailable, ReasonInsufficientBalance, http.StatusPaymentRequired, fakellm.CodeInsufficientBalance, fakellm.TraceID)
		require.Empty(t, texts)
		require.Empty(t, result.Text)
		require.Contains(t, getLogs(), fmt.Sprintf(logFailureFormat, testRequestID, ReasonInsufficientBalance, http.StatusPaymentRequired, fakellm.CodeInsufficientBalance, fakellm.TraceID))
		require.NotContains(t, getLogs(), "Insufficient balance")
	})

	t.Run("must_fail_http_429", func(t *testing.T) {
		setup(t, fakellm.Config{Fail: fakellm.FailHTTP429}, nil, nil)

		_, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrUnavailable, ReasonHTTP429, http.StatusTooManyRequests, fakellm.CodeTooManyRequests, fakellm.TraceID)
		require.Empty(t, texts)
	})

	t.Run("must_fail_http_5xx", func(t *testing.T) {
		t.Run("must_fail_status_500", func(t *testing.T) {
			setup(t, fakellm.Config{Fail: fakellm.FailHTTP500}, nil, nil)

			_, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonHTTP5xx, http.StatusInternalServerError, fakellm.CodeInternalError, fakellm.TraceID)
		})

		for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, 599} {
			t.Run(fmt.Sprintf("must_fail_status_%d", status), func(t *testing.T) {
				setup(t, fakellm.Config{}, errorHandler(status, "SERVICE_UNAVAILABLE"), nil)

				_, err := client.Complete(context.Background(), request, collect)

				requireFailure(t, err, ErrUnavailable, ReasonHTTP5xx, status, "SERVICE_UNAVAILABLE", testTraceID)
				require.Empty(t, texts)
			})
		}
	})

	t.Run("must_fail_http_4xx", func(t *testing.T) {
		t.Run("must_fail_status_401", func(t *testing.T) {
			setup(t, fakellm.Config{APIKey: testAPIKey}, nil, func(c *Config) { c.APIKey = "wrong-key" })

			_, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonHTTP4xx, http.StatusUnauthorized, fakellm.CodeUnauthorized, fakellm.TraceID)
			require.NotContains(t, getLogs(), "wrong-key")
		})

		for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout, http.StatusRequestEntityTooLarge} {
			t.Run(fmt.Sprintf("must_fail_status_%d", status), func(t *testing.T) {
				setup(t, fakellm.Config{}, errorHandler(status, "BAD_REQUEST"), nil)

				_, err := client.Complete(context.Background(), request, collect)

				requireFailure(t, err, ErrUnavailable, ReasonHTTP4xx, status, "BAD_REQUEST", testTraceID)
			})
		}

		t.Run("must_fail_status_302_not_followed", func(t *testing.T) {
			setup(t, fakellm.Config{}, http.RedirectHandler("/v1/other", http.StatusFound), nil)

			_, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonHTTP4xx, http.StatusFound, "", "")
		})

		t.Run("must_fail_body_not_json", func(t *testing.T) {
			setup(t, fakellm.Config{}, http.NotFoundHandler(), nil)

			_, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonHTTP4xx, http.StatusNotFound, "", "")
		})
	})

	t.Run("must_fail_disconnect", func(t *testing.T) {
		t.Run("must_fail_connection_closed", func(t *testing.T) {
			setup(t, fakellm.Config{Fail: fakellm.FailDisconnectAfterSentence}, nil, nil)

			result, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonDisconnect, http.StatusOK, "", "")
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			require.Equal(t, testFirstSentence, strings.Join(texts, ""))
			require.Equal(t, testFirstSentence, result.Text)
		})

		t.Run("must_fail_end_without_done", func(t *testing.T) {
			setup(t, fakellm.Config{}, sseHandler(
				`data: {"id":"c1","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":"Впереди стол."},"finish_reason":null}]}`+"\n\n",
			), nil)

			result, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonDisconnect, http.StatusOK, "", "")
			require.Equal(t, "Впереди стол.", result.Text)
		})

		t.Run("must_fail_malformed_chunk", func(t *testing.T) {
			setup(t, fakellm.Config{}, sseHandler(
				`data: {"id":"c1","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":"Впереди"`+"\n\n",
				"data: [DONE]\n\n",
			), nil)

			_, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonDisconnect, http.StatusOK, "", "")
			require.ErrorContains(t, errors.Unwrap(err), "failed to decode language model chunk: ")
		})

		t.Run("must_fail_untrusted_certificate", func(t *testing.T) {
			setup(t, fakellm.Config{}, nil, nil)
			tlsServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				stub.ServeHTTP(w, r)
			}))
			tlsServer.Config.ErrorLog = log.New(io.Discard, "", 0)
			tlsServer.StartTLS()
			t.Cleanup(tlsServer.Close)
			cfg.URL = tlsServer.URL + fakellm.BasePath
			var err error
			client, err = NewClient(cfg, m)
			require.NoError(t, err)

			_, err = client.Complete(context.Background(), request, collect)

			var failure *Error
			require.ErrorAs(t, err, &failure)
			require.Equal(t, ReasonDisconnect, failure.Reason)
			require.Equal(t, 0, failure.StatusCode)
			require.ErrorContains(t, failure.Err, "certificate")
			require.Equal(t, int32(0), requests.Load())
			require.Equal(t, 1.0, testutil.ToFloat64(m.UpstreamErrors.WithLabelValues(upstreamLabel, string(ReasonDisconnect))))
		})
	})

	t.Run("must_fail_empty", func(t *testing.T) {
		t.Run("must_fail_no_text", func(t *testing.T) {
			setup(t, fakellm.Config{Fail: fakellm.FailEmpty}, nil, nil)

			result, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonEmpty, http.StatusOK, "", "")
			require.Empty(t, texts)
			require.Equal(t, fakellm.DefaultModel, result.Model)
			require.Equal(t, "stop", result.FinishReason)
		})

		t.Run("must_fail_whitespace_only", func(t *testing.T) {
			setup(t, fakellm.Config{}, sseHandler(
				`data: {"id":"c1","model":"qwen/qwen3-8b","choices":[{"index":0,"delta":{"content":" \n"},"finish_reason":"stop"}]}`+"\n\n",
				"data: [DONE]\n\n",
			), nil)

			_, err := client.Complete(context.Background(), request, collect)

			requireFailure(t, err, ErrUnavailable, ReasonEmpty, http.StatusOK, "", "")
		})
	})

	t.Run("must_fail_timeout_ttft", func(t *testing.T) {
		setup(t, fakellm.Config{FirstTokenDelayMS: 3000}, nil, func(c *Config) { c.TTFTTimeout = 50 * time.Millisecond })
		started := time.Now()

		result, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrTimeout, ReasonTimeoutTTFT, http.StatusOK, "", "")
		require.ErrorIs(t, err, errTTFTTimeout)
		require.Less(t, time.Since(started), 3*time.Second)
		require.Empty(t, texts)
		require.Zero(t, result.TTFT)
		require.Equal(t, fakellm.DefaultModel, result.Model)
	})

	t.Run("must_fail_timeout_ttft_before_headers", func(t *testing.T) {
		release := make(chan struct{})
		setup(t, fakellm.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}), func(c *Config) { c.TTFTTimeout = 50 * time.Millisecond })
		defer close(release)

		_, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrTimeout, ReasonTimeoutTTFT, 0, "", "")
	})

	t.Run("must_fail_timeout_total", func(t *testing.T) {
		setup(t, fakellm.Config{TokenDelayMS: 100}, nil, func(c *Config) { c.TotalTimeout = 250 * time.Millisecond })

		result, err := client.Complete(context.Background(), request, collect)

		requireFailure(t, err, ErrTimeout, ReasonTimeoutTotal, http.StatusOK, "", "")
		require.ErrorIs(t, err, errTotalTimeout)
		require.NotEmpty(t, texts)
		require.Less(t, len(texts), len(strings.Fields(fakellm.DefaultText)))
		require.Positive(t, result.TTFT)
		require.Equal(t, strings.Join(texts, ""), result.Text)
	})
}

func TestNewClient(t *testing.T) {
	var (
		m   *metrics.Metrics
		cfg Config
	)

	setup := func(configure func(*Config)) {
		m = metrics.NewMetrics()
		cfg = Config{
			URL:          "https://polza.ai/api/v1/",
			APIKey:       testAPIKey,
			Seed:         42,
			MaxTokens:    96,
			ExtraBody:    "{}",
			TTFTTimeout:  900 * time.Millisecond,
			TotalTimeout: 5 * time.Second,
		}
		if configure != nil {
			configure(&cfg)
		}
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(nil)

		client, err := NewClient(cfg, m)

		require.NoError(t, err)
		require.Equal(t, "https://polza.ai/api/v1/chat/completions", client.completionsURL)
		require.Empty(t, client.extraBody)
		require.Equal(t, 900*time.Millisecond, client.ttftTimeout)
		require.Equal(t, 5*time.Second, client.totalTimeout)
	})

	t.Run("must_pass_tls_verified_and_no_compression", func(t *testing.T) {
		setup(nil)

		client, err := NewClient(cfg, m)

		require.NoError(t, err)
		transport, ok := client.httpClient.Transport.(*http.Transport)
		require.True(t, ok)
		require.Nil(t, transport.TLSClientConfig)
		require.True(t, transport.DisableCompression)
		require.Zero(t, client.httpClient.Timeout)
		require.ErrorIs(t, client.httpClient.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	})

	t.Run("must_pass_empty_extra_body", func(t *testing.T) {
		setup(func(c *Config) { c.ExtraBody = " " })

		client, err := NewClient(cfg, m)

		require.NoError(t, err)
		require.Empty(t, client.extraBody)
	})

	t.Run("must_fail_url", func(t *testing.T) {
		for _, rawURL := range []string{"", "polza.ai/api/v1", "ftp://polza.ai/api/v1", "https://", "://bad"} {
			setup(func(c *Config) { c.URL = rawURL })

			client, err := NewClient(cfg, m)

			require.Nil(t, client)
			require.EqualError(t, err, fmt.Sprintf(errURLFormat, rawURL))
		}
	})

	t.Run("must_fail_timeouts", func(t *testing.T) {
		setup(func(c *Config) { c.TTFTTimeout = 0 })

		client, err := NewClient(cfg, m)

		require.Nil(t, client)
		require.EqualError(t, err, fmt.Sprintf(errTimeoutsFormat, time.Duration(0), 5*time.Second))
	})

	t.Run("must_fail_extra_body_not_object", func(t *testing.T) {
		setup(func(c *Config) { c.ExtraBody = `[1, 2]` })

		client, err := NewClient(cfg, m)

		require.Nil(t, client)
		require.ErrorContains(t, err, "failed to parse APP_LLM_EXTRA_BODY: ")
	})

	t.Run("must_fail_extra_body_reserved_field", func(t *testing.T) {
		for _, field := range reservedFields {
			setup(func(c *Config) { c.ExtraBody = fmt.Sprintf(`{%q: 1}`, field) })

			client, err := NewClient(cfg, m)

			require.Nil(t, client)
			require.EqualError(t, err, fmt.Sprintf(errExtraBodyReservedFormat, field))
		}
	})
}

func TestNewConfig(t *testing.T) {
	var (
		vars *env.Vars
	)

	setup := func() {
		vars = &env.Vars{
			AppLLMURL:            "https://polza.ai/api/v1",
			AppLLMAPIKey:         testAPIKey,
			AppLLMTemperature:    0.2,
			AppLLMSeed:           42,
			AppLLMMaxTokens:      96,
			AppLLMExtraBody:      testExtraBody,
			AppLLMTTFTTimeoutMS:  900,
			AppLLMTotalTimeoutMS: 5000,
		}
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()

		cfg := NewConfig(vars)

		require.Equal(t, Config{
			URL:          "https://polza.ai/api/v1",
			APIKey:       testAPIKey,
			Temperature:  0.2,
			Seed:         42,
			MaxTokens:    96,
			ExtraBody:    testExtraBody,
			TTFTTimeout:  900 * time.Millisecond,
			TotalTimeout: 5 * time.Second,
		}, cfg)
	})
}

func TestOutcome(t *testing.T) {
	t.Run("must_pass_reason_for_status", func(t *testing.T) {
		cases := map[int]Reason{
			http.StatusPaymentRequired:       ReasonInsufficientBalance,
			http.StatusTooManyRequests:       ReasonHTTP429,
			http.StatusInternalServerError:   ReasonHTTP5xx,
			http.StatusBadGateway:            ReasonHTTP5xx,
			http.StatusServiceUnavailable:    ReasonHTTP5xx,
			http.StatusBadRequest:            ReasonHTTP4xx,
			http.StatusUnauthorized:          ReasonHTTP4xx,
			http.StatusForbidden:             ReasonHTTP4xx,
			http.StatusNotFound:              ReasonHTTP4xx,
			http.StatusRequestTimeout:        ReasonHTTP4xx,
			http.StatusRequestEntityTooLarge: ReasonHTTP4xx,
			http.StatusFound:                 ReasonHTTP4xx,
		}

		for status, reason := range cases {
			require.Equal(t, reason, reasonForStatus(status), status)
		}
	})

	t.Run("must_pass_reason_for_stream_error", func(t *testing.T) {
		cases := map[string]Reason{
			"INSUFFICIENT_BALANCE": ReasonInsufficientBalance,
			"insufficient_balance": ReasonInsufficientBalance,
			"402":                  ReasonInsufficientBalance,
			"PAYMENT_REQUIRED":     ReasonInsufficientBalance,
			"SERVICE_UNAVAILABLE":  ReasonStreamError,
			"TOO_MANY_REQUESTS":    ReasonStreamError,
			"":                     ReasonStreamError,
		}

		for code, reason := range cases {
			require.Equal(t, reason, reasonForStreamError(code), code)
		}
	})

	t.Run("must_pass_error_categories", func(t *testing.T) {
		for _, reason := range []Reason{ReasonTimeoutTTFT, ReasonTimeoutTotal} {
			err := &Error{Reason: reason}
			require.ErrorIs(t, err, ErrTimeout)
			require.NotErrorIs(t, err, ErrUnavailable)
		}
		for _, reason := range []Reason{ReasonInsufficientBalance, ReasonHTTP429, ReasonHTTP5xx, ReasonHTTP4xx, ReasonStreamError, ReasonDisconnect, ReasonEmpty} {
			err := &Error{Reason: reason}
			require.ErrorIs(t, err, ErrUnavailable)
			require.NotErrorIs(t, err, ErrTimeout)
			require.NotErrorIs(t, err, ErrCanceled)
		}
	})

	t.Run("must_pass_error_text_without_message", func(t *testing.T) {
		err := &Error{Reason: ReasonHTTP429, StatusCode: 429, Code: "TOO_MANY_REQUESTS", TraceID: "t-1"}

		require.EqualError(t, err, "language model request failed: reason=http_429 status=429 code=TOO_MANY_REQUESTS trace_id=t-1")
	})
}

func TestSSE(t *testing.T) {
	t.Run("must_pass_code_string", func(t *testing.T) {
		cases := map[string]string{
			`"INSUFFICIENT_BALANCE"`: "INSUFFICIENT_BALANCE",
			`"402"`:                  "402",
			`402`:                    "402",
			`null`:                   "",
			`{}`:                     "",
			``:                       "",
		}

		for raw, expected := range cases {
			require.Equal(t, expected, codeString(json.RawMessage(raw)), raw)
		}
	})

	t.Run("must_pass_provider_error", func(t *testing.T) {
		code, traceID, ok := parseProviderError(json.RawMessage(`{"code":"SERVICE_UNAVAILABLE","message":"x","trace_id":"inner"}`), "outer")
		require.True(t, ok)
		require.Equal(t, "SERVICE_UNAVAILABLE", code)
		require.Equal(t, "inner", traceID)

		code, traceID, ok = parseProviderError(json.RawMessage(`"provider failed"`), "outer")
		require.True(t, ok)
		require.Empty(t, code)
		require.Equal(t, "outer", traceID)

		for _, raw := range []string{``, `null`, ` null `} {
			_, _, ok = parseProviderError(json.RawMessage(raw), "outer")
			require.False(t, ok, raw)
		}
	})

	t.Run("must_pass_error_body", func(t *testing.T) {
		code, traceID := parseErrorBody([]byte(`{"error":{"code":"TOO_MANY_REQUESTS","message":"x"},"trace_id":"outer"}`))
		require.Equal(t, "TOO_MANY_REQUESTS", code)
		require.Equal(t, "outer", traceID)

		code, traceID = parseErrorBody([]byte(`<html>Bad Gateway</html>`))
		require.Empty(t, code)
		require.Empty(t, traceID)
	})

	t.Run("must_fail_line_too_long", func(t *testing.T) {
		reader := newSSEReader(strings.NewReader("data: " + strings.Repeat("x", sseMaxLine+1) + "\n"))

		_, _, err := reader.next()

		require.ErrorContains(t, err, "token too long")
	})
}

// sseHandler answers 200 with an SSE body made of the given pieces, flushing each one.
func sseHandler(pieces ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, piece := range pieces {
			_, _ = io.WriteString(w, piece)
			flusher.Flush()
		}
	}
}

// errorHandler answers the status with an error body in the Polza.ai format.
func errorHandler(status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"Ошибка поставщика"},"trace_id":%q}`, code, testTraceID)
	}
}
