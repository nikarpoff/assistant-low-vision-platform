package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// Server timeouts. There is no write timeout: a stream with delays may last long.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 5 * time.Second
)

func main() {
	os.Exit(run())
}

func run() int {
	logger.InitStdoutLogger()
	logger.Info("Starting fake LLM")

	stopAppChan := make(chan os.Signal, 1)
	signal.Notify(stopAppChan, syscall.SIGINT, syscall.SIGTERM)

	// Load configuration
	cfg, err := fakellm.LoadConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		logger.Error(err)
		return 1
	}

	logger.Info("Configuration loaded: port=%d model=%s fail=%s first_token_delay_ms=%d token_delay_ms=%d auth=%t",
		cfg.Port, cfg.Model, cfg.Fail, cfg.FirstTokenDelayMS, cfg.TokenDelayMS, cfg.APIKey != "")

	// HTTP server
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           fakellm.NewServer(*cfg),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	go func() {
		logger.Info("Server listening on port %d", cfg.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(fmt.Errorf("failed to serve HTTP: %w", err))
			stopAppChan <- syscall.SIGQUIT
		}
	}()

	// Wait for exit signal
	sig := <-stopAppChan

	logger.Info("stopping HTTP server...")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error(fmt.Errorf("failed to shutdown HTTP server: %w", err))
	}

	if sig == syscall.SIGQUIT {
		return 1
	}
	logger.Info("Received signal %s, fake LLM stopped", sig)
	return 0
}
