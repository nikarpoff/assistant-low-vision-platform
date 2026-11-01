// Package upstream is the HTTP client of the model services: the detector,
// ASR and TTS (PLT-15).
//
// Every service gets its own Client with its own keep-alive connection pool,
// token and timeout. A call sends a binary body with the Authorization and
// X-Request-ID headers, turns every failure into one of the typed errors of
// errors.go and counts it in alv_upstream_errors_total{upstream, reason}.
// The packages detector, asr and tts build the calls of their APIs on it.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
)

// Upstream names: the values of the upstream label of alv_upstream_errors_total.
const (
	NameDetector = "detector"
	NameASR      = "asr"
	NameTTS      = "tts"
)

// Reasons of the failed calls: the values of the reason label of alv_upstream_errors_total.
const (
	ReasonBusy            = "busy"
	ReasonUnavailable     = "unavailable"
	ReasonTimeout         = "timeout"
	ReasonUnauthorized    = "unauthorized"
	ReasonRejected        = "rejected"
	ReasonInvalidResponse = "invalid_response"
)

// ContentTypeJSON is the media type of the JSON bodies.
const ContentTypeJSON = "application/json"

// MetadataTimeout limits the calls that are not on the path of a user request: /v1/info and /v1/vocabulary.
const MetadataTimeout = 2 * time.Second

const (
	headerAuthorization = "Authorization"
	headerRequestID     = "X-Request-ID"
	headerContentType   = "Content-Type"
	bearerPrefix        = "Bearer "

	pathInfo = "/v1/info"

	maxResponseBytes = 1 << 20  // JSON answers of the services are a few kilobytes
	maxErrorBytes    = 4 << 10  // {"detail": "..."} of SVC-6
	streamChunkBytes = 16 << 10 // a chunk of a stream: about 340 ms of 24 kHz PCM s16le

	// connection pool of one service
	dialTimeout         = 5 * time.Second
	keepAlivePeriod     = 30 * time.Second
	maxIdleConnsPerHost = 16 // up to 2 TTS calls per request (PLT-7) in every session
	idleConnTimeout     = 90 * time.Second
)

// Config is the address, token and timeout of one model service.
type Config struct {
	// Name is the upstream label of the service: detector, asr or tts.
	Name string
	// BaseURL is the address of the service (APP_DETECTOR_URL, APP_ASR_URL, APP_TTS_URL).
	BaseURL string
	// Token is the token of the service (APP_*_TOKEN); empty - the Authorization header is not sent.
	Token string
	// Timeout limits a call (PLT-8); for a stream it limits the wait for the first chunk.
	Timeout time.Duration
}

// Request is one call to a model service.
type Request struct {
	Method string
	Path   string
	// RequestID is sent as X-Request-ID (SVC-4); empty - the header is not sent.
	RequestID   string
	ContentType string
	Body        []byte
	// Timeout overrides the timeout of the client; zero - the timeout of the client.
	Timeout time.Duration
}

// Info is the body of GET /v1/info (SVC-2): the configuration of a service that goes into config_id (PLT-12).
//
// Params differ between the services and are kept as they come: the detector
// may or may not list params.unsupported_classes (DET-3).
type Info struct {
	Service  string         `json:"service"`
	Version  string         `json:"version"`
	GitSHA   string         `json:"git_sha"`
	Backend  string         `json:"backend"`
	Model    string         `json:"model"`
	Device   string         `json:"device"`
	WarmedUp bool           `json:"warmed_up"`
	Params   map[string]any `json:"params"`
}

// Client calls one model service.
type Client struct {
	name       string
	baseURL    string
	token      string
	timeout    time.Duration
	httpClient *http.Client
	errors     *prometheus.CounterVec
}

// NewClient creates a client of one model service with its own keep-alive connection pool.
func NewClient(config Config, m *metrics.Metrics) (*Client, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, fmt.Errorf(errBaseURLFormat, config.Name, config.BaseURL)
	}
	if config.Timeout <= 0 {
		return nil, fmt.Errorf(errTimeoutValueFormat, config.Name, config.Timeout)
	}

	// The services live in the internal network or behind the tunnel, so no proxy;
	// compression is off because the bodies are JPEG and PCM.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: keepAlivePeriod,
		}).DialContext,
		MaxIdleConns:        maxIdleConnsPerHost,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
		TLSHandshakeTimeout: dialTimeout,
		DisableCompression:  true,
	}

	return &Client{
		name:       config.Name,
		baseURL:    strings.TrimRight(config.BaseURL, "/"),
		token:      config.Token,
		timeout:    config.Timeout,
		httpClient: &http.Client{Transport: transport},
		errors:     m.UpstreamErrors,
	}, nil
}

// Name returns the upstream label of the service.
func (c *Client) Name() string {
	return c.name
}

// Info returns the configuration of the service (GET /v1/info).
func (c *Client) Info(ctx context.Context) (*Info, error) {
	info := &Info{}
	request := &Request{
		Method:  http.MethodGet,
		Path:    pathInfo,
		Timeout: MetadataTimeout,
	}
	if err := c.Call(ctx, request, info); err != nil {
		return nil, fmt.Errorf("failed to get service info: %w", err)
	}
	return info, nil
}

// Call sends the request, reads the whole JSON answer within the timeout and decodes it into response.
func (c *Client) Call(ctx context.Context, request *Request, response any) error {
	timeout := c.requestTimeout(request)
	callCtx, cancel := context.WithTimeoutCause(ctx, timeout, errCallTimeout)
	defer cancel()

	started := time.Now()
	httpResponse, err := c.send(ctx, callCtx, request, timeout, ContentTypeJSON)
	if err != nil {
		return err
	}
	defer closeBody(httpResponse.Body)

	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxResponseBytes+1))
	if err != nil {
		return c.transportError(ctx, callCtx, timeout, err)
	}
	if len(body) > maxResponseBytes {
		return c.count(ReasonInvalidResponse, fmt.Errorf(errResponseTooLargeFormat, ErrInvalidResponse, c.name, maxResponseBytes))
	}
	if err := json.Unmarshal(body, response); err != nil {
		return c.count(ReasonInvalidResponse, fmt.Errorf(errDecodeFormat, ErrInvalidResponse, c.name, err))
	}

	logger.Debug("upstream call: upstream=%s path=%s request_id=%s status=%d duration_ms=%.1f",
		c.name, request.Path, request.RequestID, httpResponse.StatusCode, milliseconds(time.Since(started)))
	return nil
}

// Stream sends the request and waits for the first chunk of the answer within the timeout (PLT-8).
// After the first chunk the stream is limited only by ctx. The stream must be closed.
func (c *Client) Stream(ctx context.Context, request *Request, responseType string) (*Stream, error) {
	timeout := c.requestTimeout(request)
	streamCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(timeout, func() { cancel(errCallTimeout) })

	started := time.Now()
	httpResponse, err := c.send(ctx, streamCtx, request, timeout, responseType)
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, err
	}

	stream := &Stream{
		client: c,
		parent: ctx,
		ctx:    streamCtx,
		cancel: cancel,
		header: httpResponse.Header,
		body:   httpResponse.Body,
		buffer: make([]byte, streamChunkBytes),
	}

	first, err := stream.read()
	timedOut := !timer.Stop()
	if timedOut || err != nil {
		stream.Close()
		switch {
		case timedOut:
			return nil, c.transportError(ctx, streamCtx, timeout, context.Cause(streamCtx))
		case errors.Is(err, io.EOF):
			return nil, c.count(ReasonInvalidResponse, fmt.Errorf(errEmptyStreamFormat, ErrInvalidResponse, c.name))
		default:
			return nil, c.transportError(ctx, streamCtx, timeout, err)
		}
	}
	stream.first = first

	logger.Debug("upstream stream: upstream=%s path=%s request_id=%s status=%d first_chunk_ms=%.1f",
		c.name, request.Path, request.RequestID, httpResponse.StatusCode, milliseconds(time.Since(started)))
	return stream, nil
}

// send sends the request with the call context and returns the answer with a 2xx status and the expected media type.
// Every failure is already typed and counted.
func (c *Client) send(parent, call context.Context, request *Request, timeout time.Duration, responseType string) (*http.Response, error) {
	httpRequest, err := http.NewRequestWithContext(call, request.Method, c.baseURL+request.Path, bytes.NewReader(request.Body))
	if err != nil {
		return nil, fmt.Errorf(errBuildRequestFormat, c.name, err)
	}
	if request.ContentType != "" {
		httpRequest.Header.Set(headerContentType, request.ContentType)
	}
	if c.token != "" {
		httpRequest.Header.Set(headerAuthorization, bearerPrefix+c.token)
	}
	if request.RequestID != "" {
		httpRequest.Header.Set(headerRequestID, request.RequestID)
	}

	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return nil, c.transportError(parent, call, timeout, err)
	}

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		defer closeBody(httpResponse.Body)
		return nil, c.statusError(httpResponse)
	}

	contentType := httpResponse.Header.Get(headerContentType)
	if !hasMediaType(contentType, responseType) {
		closeBody(httpResponse.Body)
		return nil, c.count(ReasonInvalidResponse, fmt.Errorf(errContentTypeFormat, ErrInvalidResponse, c.name, contentType, responseType))
	}

	return httpResponse, nil
}

// statusError converts a non-2xx answer to a typed error with the detail of SVC-6.
func (c *Client) statusError(httpResponse *http.Response) error {
	var detail struct {
		Detail string `json:"detail"`
	}
	body, _ := io.ReadAll(io.LimitReader(httpResponse.Body, maxErrorBytes))
	_ = json.Unmarshal(body, &detail)

	status := httpResponse.StatusCode
	sentinel, reason := ErrRejected, ReasonRejected
	switch {
	case status == http.StatusServiceUnavailable:
		sentinel, reason = ErrBusy, ReasonBusy
	case status >= http.StatusInternalServerError:
		sentinel, reason = ErrUnavailable, ReasonUnavailable
	case status == http.StatusUnauthorized:
		sentinel, reason = ErrUnauthorized, ReasonUnauthorized
	}

	return c.count(reason, fmt.Errorf(errStatusFormat, sentinel, c.name, status, detail.Detail))
}

// transportError converts a failure to send the request or to read the answer to a typed error.
// A call interrupted by the caller (cancel or the request deadline) is not a failure of the service and is not counted.
func (c *Client) transportError(parent, call context.Context, timeout time.Duration, err error) error {
	if parent.Err() != nil {
		return fmt.Errorf(errInterruptedFormat, c.name, parent.Err())
	}
	if errors.Is(context.Cause(call), errCallTimeout) {
		return c.count(ReasonTimeout, fmt.Errorf(errTimeoutFormat, ErrTimeout, c.name, timeout))
	}
	return c.count(ReasonUnavailable, fmt.Errorf(errTransportFormat, ErrUnavailable, c.name, err))
}

// count increments alv_upstream_errors_total{upstream, reason} and returns err.
func (c *Client) count(reason string, err error) error {
	c.errors.WithLabelValues(c.name, reason).Inc()
	return err
}

// requestTimeout returns the timeout of the request or, if it is not set, of the client.
func (c *Client) requestTimeout(request *Request) time.Duration {
	if request.Timeout > 0 {
		return request.Timeout
	}
	return c.timeout
}

// hasMediaType reports whether the Content-Type has the expected media type; parameters are ignored.
func hasMediaType(contentType, expected string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, expected)
}

// closeBody reads the rest of a short body and closes it, so the connection goes back to the pool.
func closeBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes))
	_ = body.Close()
}

// milliseconds converts a duration to milliseconds for the log.
func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
