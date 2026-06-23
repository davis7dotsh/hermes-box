package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/davis7dotsh/hermes-box/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli := app.NewDefault(os.Stdin, os.Stdout, os.Stderr, os.Environ())
	os.Exit(cli.Run(ctx, os.Args[1:]))
}
