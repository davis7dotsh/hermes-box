package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/davis7dotsh/hermes-box/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli, err := app.NewDefault(os.Stdin, os.Stdout, os.Stderr, os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes-box: %v\n", err)
		os.Exit(1)
	}
	os.Exit(cli.Run(ctx, os.Args[1:]))
}
