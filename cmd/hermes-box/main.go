package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/davis7dotsh/hermes-box/internal/app"
	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func main() {
	root, err := app.FindProjectRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hermes-box] ERROR: %v\n", err)
		os.Exit(1)
	}

	cfg, err := config.Load(root, os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hermes-box] ERROR: %v\n", err)
		os.Exit(1)
	}

	application := app.New(root, cfg, process.OSRunner{}, os.Stdout, os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := application.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "[hermes-box] ERROR: %v\n", err)
		if exitErr, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}
