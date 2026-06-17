package process

import (
	"context"
	"io"
	"os"
	"os/exec"
)

type Spec struct {
	Name   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type Runner interface {
	Run(context.Context, Spec) error
	Output(context.Context, Spec) ([]byte, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, spec Spec) error {
	cmd := command(ctx, spec)
	return cmd.Run()
}

func (OSRunner) Output(ctx context.Context, spec Spec) ([]byte, error) {
	cmd := command(ctx, spec)
	return cmd.Output()
}

func command(ctx context.Context, spec Spec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	if spec.Env != nil {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	return cmd
}
