package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/app"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger.InitStdoutLogger()
	logger.Info("Starting gateway")

	stopAppChan := make(chan os.Signal, 1)
	signal.Notify(stopAppChan, syscall.SIGINT, syscall.SIGTERM)

	// Load configuration
	vars, err := env.Load()
	if err != nil {
		logger.Error(err)
		return 1
	}
	logger.SetLevel(vars.LogLevel)

	logger.Info("Configuration loaded: server_port=%d describer_mode=%s placement=%s auth_disabled=%t log_level=%s",
		vars.AppServerPort, vars.AppDescriberMode, vars.AppPlacement, vars.AppAuthDisabled, vars.LogLevel)

	// Gateway
	gateway, err := app.NewApp(vars)
	if err != nil {
		logger.Error(err)
		return 1
	}
	if err := gateway.Start(context.Background()); err != nil {
		logger.Error(err)
		return 1
	}
	go func() {
		if err := gateway.Run(); err != nil {
			stopAppChan <- syscall.SIGQUIT
		}
	}()
	defer gateway.Stop()

	// Wait for exit signal
	sig := <-stopAppChan
	if sig == syscall.SIGQUIT {
		return 1
	}

	logger.Info("Received signal %s, stopping gateway", sig)
	return 0
}
