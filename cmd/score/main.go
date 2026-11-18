package main

import (
	"os"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/score"
)

func main() {
	os.Exit(score.Main(os.Args[1:], os.Stdout, os.Stderr))
}
