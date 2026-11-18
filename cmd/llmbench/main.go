package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/llmbench"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

func main() {
	os.Exit(run())
}

func run() int {
	// stdout carries only the JSON report and the logger writes to stdout, so the
	// log of the client is off; llmbench writes its diagnostics to stderr.
	logger.SetLevel(logger.LevelError + 1)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return llmbench.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
