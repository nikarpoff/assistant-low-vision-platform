// Package llm is the client of the language model provider (PLT-15, LLM-1).
//
// One client works with any OpenAI-compatible provider: Polza.ai directly or
// GigaChat through the gpt2giga proxy. It sends POST <APP_LLM_URL>/chat/completions
// with stream=true, reads the SSE stream, passes the text to the caller as it
// comes and classifies every failure by the outcome table of PLT-15. There are
// no retries: a user request has a deadline (PLT-9).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
)

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Content part types of a multimodal message.
const (
	ContentPartText     = "text"
	ContentPartImageURL = "image_url"
)

// Request and header constants.
const (
	pathChatCompletions    = "/chat/completions"
	headerAuthorization    = "Authorization"
	headerContentType      = "Content-Type"
	headerAccept           = "Accept"
	contentTypeJSON        = "application/json"
	contentTypeEventStream = "text/event-stream"
	bearerPrefix           = "Bearer "
	upstreamLabel          = "llm"
	finishReasonError      = "error"
	maxErrorBodyBytes      = 64 << 10
)

// Body fields the client sets itself. APP_LLM_EXTRA_BODY must not set them, nor
// "user": the provider gets no user identifier (NFR-8).
const (
	fieldModel       = "model"
	fieldMessages    = "messages"
	fieldStream      = "stream"
	fieldTemperature = "temperature"
	fieldSeed        = "seed"
	fieldMaxTokens   = "max_tokens"
	fieldUser        = "user"
)

var reservedFields = []string{fieldModel, fieldMessages, fieldStream, fieldTemperature, fieldSeed, fieldMaxTokens, fieldUser}

// Connection pool settings: keep-alive connections to the provider (PLT-15).
const (
	dialTimeout           = 5 * time.Second
	dialKeepAlive         = 30 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	maxIdleConns          = 32
	maxIdleConnsPerHost   = 16
	idleConnTimeout       = 90 * time.Second
	expectContinueTimeout = time.Second
)

// Texts of the errors and log messages, shared by the code and the tests.
const (
	errURLFormat               = "language model URL must be an absolute http or https URL: %q"
	errTimeoutsFormat          = "language model timeouts must be positive: ttft=%s total=%s"
	errExtraBodyFormat         = "failed to parse APP_LLM_EXTRA_BODY: %w"
	errExtraBodyReservedFormat = "APP_LLM_EXTRA_BODY must not set the reserved field %q"
	errModelRequired           = "language model request without a model"
	errBuildRequestFormat      = "failed to build language model request: %w"
	errHandleTextFormat        = "failed to handle language model text: %w"
	errDecodeChunkFormat       = "failed to decode language model chunk: %w"
	errCanceledFormat          = "%w: %w"
	logFailureFormat           = "language model request failed: request_id=%s reason=%s status=%d code=%s trace_id=%s"
	logTransportFailureFormat  = "language model request failed: request_id=%s reason=%s status=%d error=%v"
	logAnsweredFormat          = "language model answered: request_id=%s model=%s ttft_ms=%d duration_ms=%d stopped=%t"
)

// Causes of the timeouts of PLT-8, attached to the request context.
var (
	errTTFTTimeout  = errors.New("no first token within APP_LLM_TTFT_TIMEOUT_MS")
	errTotalTimeout = errors.New("generation longer than APP_LLM_TOTAL_TIMEOUT_MS")
)

// ErrStop is returned by a TextHandler to close the stream early, for example
// when the answer limit is reached (PLT-7). Complete then ends without an error.
var ErrStop = errors.New("stop language model stream")

// TextHandler receives the text of the answer piece by piece as the provider
// streams it. ErrStop closes the stream early; any other error aborts the request
// and is returned by Complete.
type TextHandler func(text string) error

// Config holds the provider settings of the client.
type Config struct {
	// URL is the base URL of the OpenAI-compatible API (APP_LLM_URL).
	URL string
	// APIKey is the provider key (APP_LLM_API_KEY); empty - no Authorization header.
	APIKey string
	// Temperature, Seed and MaxTokens are the generation parameters (DSC-5).
	Temperature float64
	Seed        int
	MaxTokens   int
	// ExtraBody is a JSON object added to the request body (APP_LLM_EXTRA_BODY).
	ExtraBody string
	// TTFTTimeout limits the time to the first token, TotalTimeout - the whole generation (PLT-8).
	TTFTTimeout  time.Duration
	TotalTimeout time.Duration
}

// Message is one message of the prompt.
type Message struct {
	Role string `json:"role"`
	// Content is a string or, for the vlm mode, a list of ContentPart.
	Content any `json:"content"`
}

// ContentPart is a part of a multimodal message: a text or an image.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is an image of a multimodal message, a data URL for a JPEG snapshot.
type ImageURL struct {
	URL string `json:"url"`
}

// Request is one chat completion request.
type Request struct {
	// RequestID identifies the user request in the log; it is never sent to the provider (NFR-8).
	RequestID string
	// Model is the model identifier (APP_LLM_MODEL or APP_VLM_MODEL).
	Model    string
	Messages []Message
}

// Result is what the provider streamed. Complete returns it also with an error:
// then it holds what came before the failure.
type Result struct {
	// Text is the whole text passed to the TextHandler.
	Text string
	// Model and SystemFingerprint come from the first chunk that has them; they go to the journal (PLT-11).
	Model             string
	SystemFingerprint string
	// FinishReason is the finish_reason of the last choice that had one.
	FinishReason string
	// Usage is the token count from the usage chunk, nil if the provider sent none.
	Usage *Usage
	// TTFT is the time from sending the request to the first token, 0 without a token.
	TTFT time.Duration
	// Duration is the time from sending the request to the end of the stream.
	Duration time.Duration
	// Stopped is true when the TextHandler closed the stream with ErrStop.
	Stopped bool
}

// Client sends chat completion requests to the language model provider. It is safe for concurrent use.
type Client struct {
	httpClient     *http.Client
	completionsURL string
	apiKey         string
	temperature    float64
	seed           int
	maxTokens      int
	extraBody      map[string]any
	ttftTimeout    time.Duration
	totalTimeout   time.Duration
	upstreamErrors *prometheus.CounterVec
}

// NewConfig takes the provider settings from the gateway configuration.
func NewConfig(vars *env.Vars) Config {
	return Config{
		URL:          vars.AppLLMURL,
		APIKey:       vars.AppLLMAPIKey,
		Temperature:  vars.AppLLMTemperature,
		Seed:         vars.AppLLMSeed,
		MaxTokens:    vars.AppLLMMaxTokens,
		ExtraBody:    vars.AppLLMExtraBody,
		TTFTTimeout:  time.Duration(vars.AppLLMTTFTTimeoutMS) * time.Millisecond,
		TotalTimeout: time.Duration(vars.AppLLMTotalTimeoutMS) * time.Millisecond,
	}
}

// NewClient checks the settings and creates a client with its own keep-alive
// connection pool. TLS certificates are always verified against the system root
// certificates. Failures are counted in alv_upstream_errors_total{upstream="llm"}.
func NewClient(cfg Config, m *metrics.Metrics) (*Client, error) {
	baseURL, err := url.Parse(cfg.URL)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, fmt.Errorf(errURLFormat, cfg.URL)
	}
	if cfg.TTFTTimeout <= 0 || cfg.TotalTimeout <= 0 {
		return nil, fmt.Errorf(errTimeoutsFormat, cfg.TTFTTimeout, cfg.TotalTimeout)
	}
	extraBody, err := parseExtraBody(cfg.ExtraBody)
	if err != nil {
		return nil, err
	}

	return &Client{
		httpClient:     newHTTPClient(),
		completionsURL: strings.TrimRight(cfg.URL, "/") + pathChatCompletions,
		apiKey:         cfg.APIKey,
		temperature:    cfg.Temperature,
		seed:           cfg.Seed,
		maxTokens:      cfg.MaxTokens,
		extraBody:      extraBody,
		ttftTimeout:    cfg.TTFTTimeout,
		totalTimeout:   cfg.TotalTimeout,
		upstreamErrors: m.UpstreamErrors,
	}, nil
}

// Complete sends the request with stream=true and passes the text to onText as
// it comes. A provider failure is an *Error with the reason of PLT-15 that
// matches ErrTimeout or ErrUnavailable; it is counted in the metrics and logged
// without the provider message. The end of ctx gives ErrCanceled. The request is
// never repeated.
func (c *Client) Complete(ctx context.Context, req *Request, onText TextHandler) (*Result, error) {
	if req.Model == "" {
		return nil, errors.New(errModelRequired)
	}
	body, err := c.requestBody(req)
	if err != nil {
		return nil, fmt.Errorf(errBuildRequestFormat, err)
	}

	// Timeouts of the whole generation and of the first token (PLT-8)
	ctx, cancelTotal := context.WithTimeoutCause(ctx, c.totalTimeout, errTotalTimeout)
	defer cancelTotal()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.completionsURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf(errBuildRequestFormat, err)
	}
	httpRequest.Header.Set(headerContentType, contentTypeJSON)
	httpRequest.Header.Set(headerAccept, contentTypeEventStream)
	if c.apiKey != "" {
		httpRequest.Header.Set(headerAuthorization, bearerPrefix+c.apiKey)
	}

	s := &stream{
		started:   time.Now(),
		ttftTimer: time.AfterFunc(c.ttftTimeout, func() { cancel(errTTFTTimeout) }),
		onText:    onText,
		result:    &Result{},
	}
	defer s.ttftTimer.Stop()

	err = s.run(ctx, c.httpClient, httpRequest)
	s.result.Text = s.text.String()
	s.result.Duration = time.Since(s.started)

	var failure *Error
	if errors.As(err, &failure) {
		c.upstreamErrors.WithLabelValues(upstreamLabel, string(failure.Reason)).Inc()
		if failure.Err != nil && !failure.Reason.IsTimeout() {
			logger.Warning(logTransportFailureFormat, req.RequestID, failure.Reason, failure.StatusCode, failure.Err)
		} else {
			logger.Warning(logFailureFormat, req.RequestID, failure.Reason, failure.StatusCode, failure.Code, failure.TraceID)
		}
		return s.result, err
	}
	if err != nil {
		return s.result, err
	}

	logger.Debug(logAnsweredFormat, req.RequestID, s.result.Model, s.result.TTFT.Milliseconds(), s.result.Duration.Milliseconds(), s.result.Stopped)
	return s.result, nil
}

// requestBody builds the JSON body: the extra body plus the fields set by the client.
func (c *Client) requestBody(req *Request) ([]byte, error) {
	body := make(map[string]any, len(c.extraBody)+len(reservedFields))
	for key, value := range c.extraBody {
		body[key] = value
	}
	body[fieldModel] = req.Model
	body[fieldMessages] = req.Messages
	body[fieldStream] = true
	body[fieldTemperature] = c.temperature
	body[fieldSeed] = c.seed
	body[fieldMaxTokens] = c.maxTokens

	return json.Marshal(body)
}

// stream is the state of one request.
type stream struct {
	started   time.Time
	ttftTimer *time.Timer
	onText    TextHandler
	result    *Result
	text      strings.Builder
	finished  bool
}

// run sends the request and reads the stream to its end, the first failure or ErrStop.
func (s *stream) run(ctx context.Context, httpClient *http.Client, httpRequest *http.Request) error {
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return contextFailure(ctx, 0, err)
	}
	defer response.Body.Close()

	// Failure before the stream: status and the provider error body
	if response.StatusCode < 200 || response.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
		code, traceID := parseErrorBody(errBody)
		return &Error{Reason: reasonForStatus(response.StatusCode), StatusCode: response.StatusCode, Code: code, TraceID: traceID}
	}

	reader := newSSEReader(response.Body)
	for {
		payload, done, err := reader.next()
		switch {
		case err != nil && s.finished:
			// The connection closed after a chunk with finish_reason: a normal end
			return s.checkEmpty(response.StatusCode)
		case errors.Is(err, io.EOF):
			return &Error{Reason: ReasonDisconnect, StatusCode: response.StatusCode, Err: io.ErrUnexpectedEOF}
		case err != nil:
			return contextFailure(ctx, response.StatusCode, err)
		case done:
			return s.checkEmpty(response.StatusCode)
		}

		stop, err := s.handleChunk(payload, response.StatusCode)
		if err != nil || stop {
			return err
		}
	}
}

// handleChunk takes model, fingerprint, usage, errors and text from one chunk.
// stop is true when the TextHandler closed the stream with ErrStop.
func (s *stream) handleChunk(payload []byte, statusCode int) (stop bool, err error) {
	var parsed chunk
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return false, &Error{Reason: ReasonDisconnect, StatusCode: statusCode, Err: fmt.Errorf(errDecodeChunkFormat, err)}
	}

	if s.result.Model == "" {
		s.result.Model = parsed.Model
	}
	if s.result.SystemFingerprint == "" {
		s.result.SystemFingerprint = parsed.SystemFingerprint
	}
	if parsed.Usage != nil {
		s.result.Usage = parsed.Usage
	}

	// An error event ends the stream even though [DONE] follows it (LLM-1)
	if code, traceID, ok := parseProviderError(parsed.Error, parsed.TraceID); ok {
		return false, &Error{Reason: reasonForStreamError(code), StatusCode: statusCode, Code: code, TraceID: traceID}
	}

	for _, choice := range parsed.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			stop, err := s.handleText(*choice.Delta.Content, statusCode)
			if err != nil || stop {
				return stop, err
			}
		}
		if choice.FinishReason == nil || *choice.FinishReason == "" {
			continue
		}
		if *choice.FinishReason == finishReasonError {
			return false, &Error{Reason: ReasonStreamError, StatusCode: statusCode, TraceID: parsed.TraceID}
		}
		s.result.FinishReason = *choice.FinishReason
		s.finished = true
	}
	return false, nil
}

// handleText stops the first token timer and passes the text to the TextHandler.
func (s *stream) handleText(text string, statusCode int) (stop bool, err error) {
	if s.result.TTFT == 0 {
		if !s.ttftTimer.Stop() {
			// The timer fired before this token arrived
			return false, &Error{Reason: ReasonTimeoutTTFT, StatusCode: statusCode, Err: errTTFTTimeout}
		}
		s.result.TTFT = max(time.Since(s.started), time.Nanosecond)
	}

	s.text.WriteString(text)
	if err := s.onText(text); err != nil {
		if errors.Is(err, ErrStop) {
			s.result.Stopped = true
			return true, nil
		}
		return false, fmt.Errorf(errHandleTextFormat, err)
	}
	return false, nil
}

// checkEmpty turns a normal end without any text into the "empty" outcome.
func (s *stream) checkEmpty(statusCode int) error {
	if strings.TrimSpace(s.text.String()) == "" {
		return &Error{Reason: ReasonEmpty, StatusCode: statusCode}
	}
	return nil
}

// contextFailure classifies a transport or read error: one of the timeouts, the
// end of the caller context or a broken connection.
func contextFailure(ctx context.Context, statusCode int, err error) error {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errTTFTTimeout):
		return &Error{Reason: ReasonTimeoutTTFT, StatusCode: statusCode, Err: cause}
	case errors.Is(cause, errTotalTimeout):
		return &Error{Reason: ReasonTimeoutTotal, StatusCode: statusCode, Err: cause}
	case cause != nil:
		return fmt.Errorf(errCanceledFormat, ErrCanceled, cause)
	default:
		return &Error{Reason: ReasonDisconnect, StatusCode: statusCode, Err: err}
	}
}

// parseExtraBody parses APP_LLM_EXTRA_BODY: a JSON object without the reserved fields.
func parseExtraBody(raw string) (map[string]any, error) {
	extraBody := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return extraBody, nil
	}
	if err := json.Unmarshal([]byte(raw), &extraBody); err != nil {
		return nil, fmt.Errorf(errExtraBodyFormat, err)
	}
	for _, field := range reservedFields {
		if _, ok := extraBody[field]; ok {
			return nil, fmt.Errorf(errExtraBodyReservedFormat, field)
		}
	}
	return extraBody, nil
}

// newHTTPClient creates an HTTP client with a keep-alive pool and without
// redirects: a redirect would be a second request to the provider. The response
// is not compressed, so every SSE event arrives as soon as it is sent. The TLS
// configuration is the default one: certificates are verified against the
// system root certificates of the image.
func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: dialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		DisableCompression:    true,
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
