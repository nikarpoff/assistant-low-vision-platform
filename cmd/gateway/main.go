package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/app"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/utils"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger.InitStdoutLogger()

	healthcheck := flag.Bool("healthcheck", false, "check GET /health of the gateway on 127.0.0.1:APP_SERVER_PORT and exit with 0 or 1")
	flag.Parse()
	if *healthcheck {
		return checkHealth()
	}

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

// checkHealth is the healthcheck of the container: the distroless image has no curl (DEP-7).
func checkHealth() int {
	vars, err := env.Load()
	if err != nil {
		logger.Error(err)
		return 1
	}
	if err := utils.Healthcheck(vars.AppServerPort); err != nil {
		logger.Error(err)
		return 1
	}
	return 0
}
