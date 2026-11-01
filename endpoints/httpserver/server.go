package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// Server is the HTTP transport of the gateway.
type Server struct {
	vars   *env.Vars
	server *http.Server
}

// NewServer registers the routes and configures the HTTP server.
func NewServer(vars *env.Vars) *Server {
	router := mux.NewRouter()

	router.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}).Methods(http.MethodGet)

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
