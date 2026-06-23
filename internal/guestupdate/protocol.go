package guestupdate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const maximumRequestBytes = 4 << 20
const maximumExecOutputBytes = 16 << 20
const maximumResponseBytes = 40 << 20

func Serve(ctx context.Context, input io.Reader, output, diagnostics io.Writer) error {
	return serve(ctx, input, output, diagnostics, nil)
}

func serve(ctx context.Context, input io.Reader, output, diagnostics io.Writer, runner Runner) error {
	line, reader, err := readRequestLine(input)
	if err != nil {
		return writeFailure(output, "invalid_input", err)
	}
	request, err := decodeRequest(bytes.TrimSpace(line))
	if err != nil {
		return writeFailure(output, "invalid_input", fmt.Errorf("decode request: %w", err))
	}
	if err := request.Validate(); err != nil {
		return writeFailure(output, "invalid_input", err)
	}
	if request.Root != "" && request.Root != os.Getenv("HERMES_BOX_GUEST_TEST_ROOT") {
		return writeFailure(output, "invalid_input", errors.New("root prefix is available only to the test harness"))
	}
	engine := New(request.Root)
	if runner != nil {
		engine.Runner = runner
	}
	engine.Runner = diagnosticRunner{Runner: engine.Runner, Diagnostics: diagnostics}
	engine.Stdout = diagnostics
	engine.Stderr = diagnostics
	switch request.Operation {
	case "apply":
		status, err := engine.Apply(ctx, request.Components, request.Initial, request.SnapshotReady, request.ReviewedLock)
		return writeResult(output, status, err)
	case "rollback":
		status, err := engine.Rollback(ctx, request.Component, request.SnapshotReady, request.ReviewedLock)
		return writeResult(output, status, err)
	case "recover":
		status, err := engine.Recover(ctx)
		return writeResult(output, status, err)
	case "status":
		status, err := engine.Status()
		return writeResult(output, status, err)
	case "exec":
		return serveExec(ctx, engine, request, output)
	case "backup-stream":
		return serveBackup(ctx, engine, request, output)
	case "restore-stream":
		err := engine.RestoreStream(reader, request.ReplaceExisting)
		return writeResult(output, map[string]bool{"restored": err == nil, "replace_existing": request.ReplaceExisting}, err)
	case "restore-paths":
		err := engine.RestorePaths(ctx, request.Component, reader)
		return writeResult(output, map[string]bool{"restored": err == nil}, err)
	default:
		panic("validated operation was not dispatched")
	}
}

// readRequestLine bounds allocation before reading attacker-controlled input.
// The returned reader preserves bytes buffered beyond the JSON line so direct
// restore streams retain their existing JSON-line-plus-raw-tar framing.
func readRequestLine(input io.Reader) ([]byte, io.Reader, error) {
	limited := &io.LimitedReader{R: input, N: maximumRequestBytes + 1}
	buffered := bufio.NewReaderSize(limited, 64*1024)
	line := make([]byte, 0, 64*1024)
	for {
		fragment, err := buffered.ReadSlice('\n')
		if len(line)+len(fragment) > maximumRequestBytes {
			return nil, nil, errors.New("request exceeds 4 MiB")
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, io.MultiReader(buffered, input), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, io.MultiReader(buffered, input), nil
		case errors.Is(err, io.EOF):
			return nil, nil, errors.New("request is empty")
		default:
			return nil, nil, err
		}
	}
}

// diagnosticRunner keeps the process stdout owned by the protocol. Commands
// that intentionally capture output (exec and digest inspection) provide their
// own writers and are left untouched; every other child stream is diagnostic.
type diagnosticRunner struct {
	Runner      Runner
	Diagnostics io.Writer
}

func (runner diagnosticRunner) Run(ctx context.Context, argv []string, options RunOptions) (int, error) {
	if options.Stdout == nil {
		options.Stdout = runner.Diagnostics
	}
	if options.Stderr == nil {
		options.Stderr = runner.Diagnostics
	}
	return runner.Runner.Run(ctx, argv, options)
}

func serveExec(ctx context.Context, engine *Engine, request Request, output io.Writer) error {
	if len(request.Argv) == 0 {
		return writeFailure(output, "invalid_input", errors.New("exec argv must not be empty"))
	}
	stdout := &boundedBuffer{remaining: maximumExecOutputBytes}
	stderr := &boundedBuffer{remaining: maximumExecOutputBytes}
	directory := request.Directory
	if directory == "" {
		directory = "/home/agent/workspace"
	}
	exitCode, err := engine.Runner.Run(ctx, engine.agentArgv(request.Argv), RunOptions{
		Directory: directory, Environment: engine.agentEnvironment(request.Environment),
		Stdout: stdout, Stderr: stderr,
	})
	result := map[string]any{
		"exit_code": exitCode,
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
	}
	if err != nil {
		response := Response{Schema: ProtocolSchema, OK: false, Result: result, Error: &ProtocolError{Code: "external_failed", Message: err.Error()}}
		if encodeErr := encodeResponse(output, response); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	return writeResult(output, result, nil)
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	if len(value) > buffer.remaining {
		return 0, errors.New("exec output exceeds 16 MiB protocol limit")
	}
	buffer.remaining -= len(value)
	return buffer.buffer.Write(value)
}

func (buffer *boundedBuffer) String() string {
	return buffer.buffer.String()
}

func serveBackup(ctx context.Context, engine *Engine, request Request, output io.Writer) error {
	if request.Component != "" && len(request.BackupPaths) != 0 {
		return writeFailure(output, "invalid_input", errors.New("component snapshot paths are fixed by the guest"))
	}
	header := Response{Schema: ProtocolSchema, OK: true, Result: StreamResult{
		Stream: "tar", Framing: "direct", Owner: "guest",
	}}
	if err := encodeResponse(output, header); err != nil {
		return err
	}
	// After the header, stdout is owned exclusively by the raw tar stream. A
	// stream failure is reported through process status and diagnostics; adding
	// a JSON trailer would corrupt the tar framing.
	if request.Component != "" {
		return engine.SnapshotStream(ctx, output, request.Component)
	}
	return engine.BackupStream(ctx, output, request.BackupPaths)
}

func writeResult(output io.Writer, result any, err error) error {
	if err != nil {
		return writeFailure(output, classify(err), err)
	}
	return encodeResponse(output, Response{Schema: ProtocolSchema, OK: true, Result: result})
}

func writeFailure(output io.Writer, code string, err error) error {
	response := Response{Schema: ProtocolSchema, OK: false, Error: &ProtocolError{Code: code, Message: err.Error()}}
	if encodeErr := encodeResponse(output, response); encodeErr != nil {
		return encodeErr
	}
	return err
}

func encodeResponse(output io.Writer, response Response) error {
	if err := response.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(encoded)+1 > maximumResponseBytes {
		return errors.New("response exceeds 40 MiB protocol limit")
	}
	encoded = append(encoded, '\n')
	written, err := output.Write(encoded)
	if err == nil && written != len(encoded) {
		return io.ErrShortWrite
	}
	return err
}

func classify(err error) string {
	switch {
	case errors.Is(err, errBusy):
		return "busy"
	case errors.Is(err, errIntegrity):
		return "integrity_failed"
	case errors.Is(err, errHealth):
		return "health_failed"
	case errors.Is(err, errInvalidInput):
		return "invalid_input"
	default:
		return "external_failed"
	}
}
