// Package fakellm is a stub of the language model provider (PLT-19) for tests
// and the end-to-end check without keys and network.
//
// It serves the OpenAI-compatible POST /v1/chat/completions and GET /v1/models,
// streams a deterministic answer in the SSE format of Polza.ai, imitates the
// provider failures of FAKELLM_FAIL in the Polza.ai error format and records the
// requests it receives. The command cmd/fakellm runs it as a server; tests run
// it in process with httptest and read the recorded requests.
package fakellm

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// BasePath is the path of the API: the provider URL of the stub is http://<host>:<port>/v1.
const BasePath = "/v1"

// Paths of the API under BasePath.
const (
	PathChatCompletions = BasePath + "/chat/completions"
	PathModels          = BasePath + "/models"
)

// Values of the stream and of the errors.
const (
	// DefaultModel is the model in the chunks and in /v1/models unless FAKELLM_MODEL sets another one.
	DefaultModel = "fake/fake-llm"
	// SystemFingerprint is the system_fingerprint of every chunk.
	SystemFingerprint = "fp_fake"
	// TraceID is the trace_id of every error.
	TraceID = "fake-trace"
	// DefaultText is the deterministic answer.
	DefaultText = "Впереди стол и два стула. Слева дверь, далеко. Справа шкаф, близко."
)

// Error codes in the Polza.ai format.
const (
	CodeInsufficientBalance = "INSUFFICIENT_BALANCE"
	CodeTooManyRequests     = "TOO_MANY_REQUESTS"
	CodeInternalError       = "INTERNAL_ERROR"
	CodeServiceUnavailable  = "SERVICE_UNAVAILABLE"
	CodeUnauthorized        = "UNAUTHORIZED"
	CodeBadRequest          = "BAD_REQUEST"
)

// Messages, headers, limits and log texts of the stub.
const (
	msgInsufficientBalance  = "Insufficient balance"
	msgTooManyRequests      = "Too many requests"
	msgInternalError        = "Internal server error"
	msgProviderDisconnected = "Provider disconnected unexpectedly"
	msgUnauthorized         = "Invalid API key"
	msgInvalidBody          = "Request body is not valid JSON"
	msgStreamRequired       = "Only stream=true is supported"
	headerAuthorization     = "Authorization"
	headerContentType       = "Content-Type"
	headerRetryAfter        = "Retry-After"
	contentTypeJSON         = "application/json"
	retryAfterSeconds       = "1"
	bearerPrefix            = "Bearer "
	maxRecordedRequests     = 256
	logRequestFormat        = "fake LLM request: method=%s path=%s fail=%s"
	logUnauthorizedFormat   = "fake LLM request without a valid key: path=%s"
	logInvalidRequestFormat = "fake LLM invalid request: %s"
	logWriteFailureFormat   = "failed to write fake LLM response: %v"
)

// RecordedRequest is a request received by the stub.
type RecordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Server is the stub provider. It is an http.Handler safe for concurrent use.
type Server struct {
	model           string
	apiKey          string
	firstTokenDelay time.Duration
	tokenDelay      time.Duration
	fail            FailMode
	text            string
	router          *mux.Router

	mu       sync.Mutex
	requests []RecordedRequest
}

// NewServer registers the routes of the stub. A zero Config gives the normal
// stream of DefaultText from DefaultModel without authentication and delays.
func NewServer(cfg Config) *Server {
	s := &Server{
		model:           cfg.Model,
		apiKey:          cfg.APIKey,
		firstTokenDelay: time.Duration(cfg.FirstTokenDelayMS) * time.Millisecond,
		tokenDelay:      time.Duration(cfg.TokenDelayMS) * time.Millisecond,
		fail:            cfg.Fail,
		text:            cfg.Text,
	}
	if s.model == "" {
		s.model = DefaultModel
	}
	if s.fail == "" {
		s.fail = FailNone
	}
	if s.text == "" {
		s.text = DefaultText
	}

	router := mux.NewRouter()
	router.HandleFunc(PathChatCompletions, s.authorized(s.chatCompletions)).Methods(http.MethodPost)
	router.HandleFunc(PathModels, s.authorized(s.models)).Methods(http.MethodGet)
	s.router = router

	return s
}

// ServeHTTP records the request and serves it.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, msgInvalidBody)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.record(RecordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})

	logger.Debug(logRequestFormat, r.Method, r.URL.Path, s.fail)
	s.router.ServeHTTP(w, r)
}

// Requests returns the received requests in order, at most the last 256.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]RecordedRequest(nil), s.requests...)
}

// Reset forgets the received requests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = nil
}

// record keeps the request; the oldest ones are dropped so that a long-running stub does not grow.
func (s *Server) record(request RecordedRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, request)
	if len(s.requests) > maxRecordedRequests {
		s.requests = append([]RecordedRequest(nil), s.requests[len(s.requests)-maxRecordedRequests:]...)
	}
}

// authorized answers 401 when a key is set and the request has no "Authorization: Bearer <key>".
func (s *Server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			expected := []byte(bearerPrefix + s.apiKey)
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(headerAuthorization)), expected) != 1 {
				logger.Warning(logUnauthorizedFormat, r.URL.Path)
				writeError(w, http.StatusUnauthorized, CodeUnauthorized, msgUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// chatCompletionRequest is the part of the request body the stub reads.
type chatCompletionRequest struct {
	Stream   bool              `json:"stream"`
	Messages []json.RawMessage `json:"messages"`
}

// chatCompletions handles POST /v1/chat/completions.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var request chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		logger.Warning(logInvalidRequestFormat, err)
		writeError(w, http.StatusBadRequest, CodeBadRequest, msgInvalidBody)
		return
	}
	if !request.Stream {
		logger.Warning(logInvalidRequestFormat, msgStreamRequired)
		writeError(w, http.StatusBadRequest, CodeBadRequest, msgStreamRequired)
		return
	}

	// Failures before the stream
	switch s.fail {
	case FailHTTP402:
		writeError(w, http.StatusPaymentRequired, CodeInsufficientBalance, msgInsufficientBalance)
		return
	case FailHTTP429:
		w.Header().Set(headerRetryAfter, retryAfterSeconds)
		writeError(w, http.StatusTooManyRequests, CodeTooManyRequests, msgTooManyRequests)
		return
	case FailHTTP500:
		writeError(w, http.StatusInternalServerError, CodeInternalError, msgInternalError)
		return
	}

	s.stream(w, r, promptTokens(request.Messages))
}

// modelList is the body of GET /v1/models.
type modelList struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

// modelInfo is one model of the list.
type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// models handles GET /v1/models.
func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, modelList{
		Object: "list",
		Data:   []modelInfo{{ID: s.model, Object: "model", Created: 0, OwnedBy: "fakellm"}},
	})
}

// errorObject is the error of the Polza.ai format.
type errorObject struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"`
}

// errorBody is the body of an error before the stream: {"error": {...}, "trace_id": "..."}.
type errorBody struct {
	Error   errorObject `json:"error"`
	TraceID string      `json:"trace_id"`
}

// writeError writes an error before the stream in the Polza.ai format.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{
		Error:   errorObject{Code: code, Message: message, TraceID: TraceID},
		TraceID: TraceID,
	})
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Debug(logWriteFailureFormat, err)
	}
}
