package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/handlers/adminauth"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// Server is the HTTP transport of the gateway.
type Server struct {
	vars   *env.Vars
	server *http.Server
}

// Routes of the gateway (PLT-1, PLT-13).
const (
	healthPath  = "/health"
	readyPath   = "/ready"
	metricsPath = "/metrics"
	sessionPath = "/v1/session"
	statsPath   = "/v1/stats"
	configPath  = "/v1/config"
)

// msgInternalError is the impersonal answer to an internal error (PLT-13, NFR-7).
const msgInternalError = "internal server error"

// Handlers serve the routes of the gateway besides /health.
type Handlers struct {
	// Sessions serves GET /v1/session: the WebSocket sessions of the application.
	Sessions http.Handler
	// Ready serves GET /ready.
	Ready http.Handler
	// Metrics serves GET /metrics.
	Metrics http.Handler
	// Stats serves GET /v1/stats; the server puts it behind the admin token.
	Stats http.Handler
	// Config serves GET /v1/config; the server puts it behind the admin token.
	Config http.Handler
}

// errorResponse is the body of an error answer.
type errorResponse struct {
	Error string `json:"error"`
}

// NewServer registers the routes and configures the HTTP server: GET /health, /ready,
// /metrics, /v1/session, and /v1/stats and /v1/config behind the admin token
// APP_ADMIN_TOKEN. A panic of a handler gets the impersonal 500 answer.
func NewServer(vars *env.Vars, handlers Handlers) *Server {
	router := mux.NewRouter()
	router.Use(recoverPanics)
	admin := adminauth.NewMiddleware(vars)

	router.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}).Methods(http.MethodGet)

	router.Handle(readyPath, handlers.Ready).Methods(http.MethodGet)
	router.Handle(metricsPath, handlers.Metrics).Methods(http.MethodGet)
	router.Handle(sessionPath, handlers.Sessions).Methods(http.MethodGet)
	router.Handle(statsPath, admin.Handle(handlers.Stats)).Methods(http.MethodGet)
	router.Handle(configPath, admin.Handle(handlers.Config)).Methods(http.MethodGet)

	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", vars.AppServerPort),
		Handler:      router,
		ReadTimeout:  time.Duration(vars.AppServerReadTimeout) * time.Second,
		WriteTimeout: time.Duration(vars.AppServerWriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(vars.AppServerIdleTimeout) * time.Second,
	}

	return &Server{
		vars:   vars,
		server: httpServer,
	}
}

// Handler returns the router of the server.
func (s *Server) Handler() http.Handler {
	return s.server.Handler
}

// Run starts the HTTP server and blocks until it is stopped.
func (s *Server) Run() error {
	logger.Info("Server listening on port %d", s.vars.AppServerPort)
	err := s.server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error(fmt.Errorf("failed to serve HTTP: %w", err))
		return err
	}
	return nil
}

// Stop gracefully shuts the HTTP server down within APP_SERVER_SHUTDOWN_TIMEOUT.
func (s *Server) Stop() {
	logger.Info("stopping HTTP server...")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.vars.AppServerShutdownTimeout)*time.Second)
	defer cancel()

	if err := s.server.Shutdown(ctx); err != nil {
		logger.Error(fmt.Errorf("failed to shutdown HTTP server: %w", err))
	}
}

// recoverPanics turns a panic of a handler into the impersonal 500 answer; the panic goes only to the log.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			logger.Error(fmt.Errorf("panic in HTTP handler: method=%s path=%s: %v", r.Method, r.URL.Path, recovered))
			respondWithJSON(w, http.StatusInternalServerError, &errorResponse{Error: msgInternalError})
		}()

		next.ServeHTTP(w, r)
	})
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Error(err)
	}
}
