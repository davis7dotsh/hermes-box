package app

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/davis7dotsh/hermes-box/internal/box"
	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/keychain"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func NewDefault(stdin io.Reader, stdout, stderr io.Writer, environ []string) *CLI {
	runner := process.OSRunner{}
	keys, _ := keychain.New("com.highmatter.hermes-box.backup")
	operations := &defaultOperations{runner: runner, stdout: stdout, stderr: stderr, keys: keys}
	return New(Dependencies{
		Loader: &defaultLoader{}, Operations: operations,
		Backups: &defaultBackups{operations: operations}, Locker: defaultLocker{},
	}, stdin, stdout, stderr, environ)
}

type defaultLoader struct{}

func (*defaultLoader) Load(_ context.Context, request LoadRequest) (Definition, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Definition{}, err
	}
	var bundle config.Bundle
	if request.Command == "restore" {
		if err := config.ValidateEnvironment(request.Environ); err != nil {
			return Definition{}, err
		}
		configPath, err := config.ResolvePath(request.ConfigPath, cwd, request.Environ)
		if err != nil {
			return Definition{}, err
		}
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			return Definition{}, err
		}
		bundle = config.Bundle{
			Config: cfg, ConfigPath: configPath, LockPath: filepath.Join(filepath.Dir(configPath), "hermes-box.lock"), Dir: filepath.Dir(configPath),
		}
	} else {
		var err error
		bundle, err = config.Load(request.ConfigPath, cwd, request.Environ)
		if err != nil {
			return Definition{}, err
		}
	}
	home := request.Home
	if !filepath.IsAbs(home) {
		return Definition{}, &Error{Code: "invalid_input", Message: "HERMES_BOX_HOME must be absolute", Status: 2}
	}
	return Definition{
		Name: bundle.Config.Name, ConfigPath: bundle.ConfigPath, ConfigDir: bundle.Dir,
		LockPath: bundle.LockPath, Home: filepath.Clean(home), Bundle: bundle,
	}, nil
}

type defaultLocker struct{}

func identityAccount(def Definition) string {
	return keychain.IdentityAccount(def.ConfigDir, def.Name)
}

func (defaultLocker) Acquire(_ context.Context, definition Definition, command string) (func() error, error) {
	store, err := box.NewStore(definition.Home)
	if err != nil {
		return nil, err
	}
	lock, err := store.Acquire(definition.Name, command)
	if err != nil {
		return nil, err
	}
	return lock.Close, nil
}
