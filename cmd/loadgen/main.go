package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/loadgen"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return loadgen.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
