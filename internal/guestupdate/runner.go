package guestupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type RunOptions struct {
	Directory   string
	Environment map[string]string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

type Runner interface {
	Run(context.Context, []string, RunOptions) (int, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, argv []string, options RunOptions) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 2, errors.New("argv must not be empty")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = options.Directory
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	if command.Stdout == nil {
		command.Stdout = os.Stderr
	}
	command.Stderr = options.Stderr
	if command.Stderr == nil {
		command.Stderr = os.Stderr
	}
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			environment[entry[:index]] = entry[index+1:]
		}
	}
	for key, value := range options.Environment {
		environment[key] = value
	}
	command.Env = make([]string, 0, len(environment))
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	err := command.Run()
	if err == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode(), fmt.Errorf("%s exited %d", argv[0], exitError.ExitCode())
	}
	return 1, err
}
