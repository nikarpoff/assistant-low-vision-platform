// Package session serves the WebSocket sessions of the application (PLT-1):
// /v1/session with the subprotocol alv.v1, the device token checked before the
// upgrade, hello and welcome, the binary frames and the JSON messages of the
// protocol (PROTO-1..PROTO-13), cancellation (PROTO-8), keepalive (PROTO-9) and
// the graceful shutdown with the close code 1001.
//
// Every session has one goroutine that reads and one that writes. The writer
// drops the messages and frames of a cancelled request, so nothing of it reaches
// the application after the cancellation.
package session

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/pipeline"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
)

// Path is the route of the sessions (PROTO-1).
const Path = "/v1/session"

// Subprotocol is the WebSocket subprotocol of the protocol (PROTO-1).
const Subprotocol = "alv.v1"

// Defaults of the session server (PROTO-9, PLT-1).
const (
	// DefaultPingInterval is how often the gateway pings the application.
	DefaultPingInterval = 15 * time.Second
	// DefaultPongTimeout is how long the gateway waits for a pong before it closes the connection.
	DefaultPongTimeout = 30 * time.Second
	// DefaultShutdownGrace is how long the active requests may finish on shutdown.
	DefaultShutdownGrace = 5 * time.Second
)

// Close codes of the gateway.
const (
	// CloseUnsupportedProtocol closes the session after hello of an unsupported protocol version (PROTO-10).
	CloseUnsupportedProtocol = 4001
)

// Audio input format of the application (PROTO-3).
const (
	audioInSampleRate     = 16000
	audioInBytesPerSecond = audioInSampleRate * 2 // s16le mono
)

// Texts of the HTTP errors before the upgrade, shared by the code and the tests.
const (
	msgUnauthorized        = "unauthorized"
	msgSubprotocolRequired = "subprotocol alv.v1 is required"
	msgShuttingDown        = "server is shutting down"
)

const (
	headerAuthorization   = "Authorization"
	headerWWWAuthenticate = "WWW-Authenticate"
	schemeBearer          = "Bearer"
	bufferSize            = 16 << 10
)

//go:generate moq -out pipelines_moq_test.go . Pipelines

// Pipelines run the requests of the sessions (*pipeline.Pipeline).
type Pipelines interface {
	Describe(ctx context.Context, req *pipeline.DescribeRequest, sink pipeline.Sink)
	Ask(ctx context.Context, req *pipeline.AskRequest, sink pipeline.Sink)
	Passive(ctx context.Context, req *pipeline.PassiveRequest, sink pipeline.Sink)
}

//go:generate moq -out config_source_moq_test.go . ConfigSource

// ConfigSource gives the current configuration snapshot for welcome (*snapshot.Collector).
type ConfigSource interface {
	Current() *snapshot.Snapshot
}

// Config holds the settings of the session server.
type Config struct {
	// AuthDisabled is APP_AUTH_DISABLED: sessions without a device token.
	AuthDisabled bool
	// DeviceTokens is APP_DEVICE_TOKENS.
	DeviceTokens []string
	// ServerVersion goes to welcome.
	ServerVersion string
	// Client is the config of welcome (PROTO-13).
	Client protocol.ClientConfig
	// Limits are the payload limits of the frames (PROTO-3).
	Limits protocol.FrameLimits
	// MaxQuestionMS is APP_MAX_QUESTION_MS: the longest question.
	MaxQuestionMS int
	// PingInterval and PongTimeout keep the connection alive (PROTO-9).
	PingInterval time.Duration
	PongTimeout  time.Duration
	// ShutdownGrace is how long the active requests may finish on shutdown (PLT-1).
	ShutdownGrace time.Duration
}

// NewConfig takes the settings of the session server from the gateway configuration.
func NewConfig(vars *env.Vars, serverVersion string) Config {
	return Config{
		AuthDisabled:  vars.AppAuthDisabled,
		DeviceTokens:  vars.AppDeviceTokens,
		ServerVersion: serverVersion,
		Client: protocol.ClientConfig{
			ImageMaxSide:      vars.AppImageMaxSide,
			JPEGQuality:       vars.AppJPEGQuality,
			PassiveIntervalMS: vars.AppPassiveIntervalMS,
			ListenWindowMS:    vars.AppListenWindowMS,
			MaxQuestionMS:     vars.AppMaxQuestionMS,
			AudioIn: protocol.AudioFormat{
				Format:     protocol.AudioFormatPCMS16LE,
				SampleRate: audioInSampleRate,
				Channels:   1,
			},
			AudioOut: protocol.AudioFormat{
				Format:     protocol.AudioFormatPCMS16LE,
				SampleRate: vars.AppAudioOutSampleRate,
				Channels:   1,
			},
		},
		Limits:        protocol.NewFrameLimits(vars.AppMaxImageBytes),
		MaxQuestionMS: vars.AppMaxQuestionMS,
		PingInterval:  DefaultPingInterval,
		PongTimeout:   DefaultPongTimeout,
		ShutdownGrace: DefaultShutdownGrace,
	}
}

// errorResponse is the body of an HTTP error before the upgrade.
type errorResponse struct {
	Error string `json:"error"`
}

// Server accepts the sessions at /v1/session.
type Server struct {
	cfg       Config
	pipelines Pipelines
	config    ConfigSource
	metrics   *metrics.Metrics
	registry  *Registry
	upgrader  websocket.Upgrader
	tokens    [][]byte
}

// NewServer creates the session server.
func NewServer(cfg Config, pipelines Pipelines, config ConfigSource, m *metrics.Metrics) *Server {
	tokens := make([][]byte, 0, len(cfg.DeviceTokens))
	for _, token := range cfg.DeviceTokens {
		if token = strings.TrimSpace(token); token != "" {
			tokens = append(tokens, []byte(token))
		}
	}

	return &Server{
		cfg:       cfg,
		pipelines: pipelines,
		config:    config,
		metrics:   m,
		registry:  NewRegistry(m.SessionsActive),
		tokens:    tokens,
		upgrader: websocket.Upgrader{
			ReadBufferSize:    bufferSize,
			WriteBufferSize:   bufferSize,
			Subprotocols:      []string{Subprotocol},
			EnableCompression: false,
			// The application is not a browser: it sends no Origin, the device token guards the route.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// ServeHTTP handles GET /v1/session: the device token and the subprotocol are checked
// before the upgrade (PROTO-1, PROTO-2), then the session runs until it is closed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.registry.Closing() {
		respondWithJSON(w, http.StatusServiceUnavailable, &errorResponse{Error: msgShuttingDown})
		return
	}
	if !s.cfg.AuthDisabled && !s.authorized(r) {
		logger.Warning("unauthorized session request: remote=%s", r.RemoteAddr)
		w.Header().Set(headerWWWAuthenticate, schemeBearer)
		respondWithJSON(w, http.StatusUnauthorized, &errorResponse{Error: msgUnauthorized})
		return
	}
	if !slices.Contains(websocket.Subprotocols(r), Subprotocol) {
		respondWithJSON(w, http.StatusBadRequest, &errorResponse{Error: msgSubprotocolRequired})
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// The upgrader has answered the client already.
		logger.Warning("failed to upgrade session: remote=%s error=%v", r.RemoteAddr, err)
		return
	}

	session := newSession(s, conn)
	if !s.registry.add(session) {
		session.closeWith(websocket.CloseGoingAway, msgShuttingDown)
		return
	}
	defer s.registry.remove(session)

	session.run()
}

// Shutdown stops accepting sessions, gives the active requests up to ShutdownGrace
// to finish and closes every session with the code 1001 (PLT-1).
func (s *Server) Shutdown(ctx context.Context) {
	logger.Info("stopping sessions...")
	s.registry.Shutdown(ctx, s.cfg.ShutdownGrace)
}

// Sessions returns the number of open sessions.
func (s *Server) Sessions() int {
	return s.registry.Len()
}

// authorized reports whether the request has a device token of APP_DEVICE_TOKENS.
// The tokens are compared in constant time.
func (s *Server) authorized(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get(headerAuthorization), " ")
	if !ok || !strings.EqualFold(scheme, schemeBearer) {
		return false
	}
	presented := []byte(strings.TrimSpace(token))

	matched := 0
	for _, known := range s.tokens {
		matched |= subtle.ConstantTimeCompare(presented, known)
	}
	return matched == 1
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Error(err)
	}
}
