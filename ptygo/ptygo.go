// Package ptygo is a cross-platform pseudo terminal (PTY) library for Go.
//
// Backends:
//   - Unix (Linux/macOS/BSD): github.com/creack/pty (forkpty based)
//   - Windows ConPTY: implemented with kernel32 P/Invoke, modeled after
//     github.com/microsoft/hcsshim/internal/conpty
//   - Windows winpty: implemented with the winpty DLL (see deps/winpty),
//     modeled after github.com/iamacarpet/go-winpty
package ptygo

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Options describes how to spawn a process attached to a pseudo terminal.
type Options struct {
	// Name is an optional display name for the terminal session.
	Name string

	// Rows and Cols are the initial terminal size.
	Rows int
	Cols int

	// Cwd is the working directory of the spawned process.
	Cwd string

	// App is the path to the executable to spawn.
	App string

	// Args are the command line arguments passed to the process.
	// On Windows the arguments are quoted and concatenated; on Unix they are
	// passed to execvp directly.
	Args []string

	// Env contains additional environment variables in "KEY=VALUE" form.
	// They are merged over the current process environment. An empty value
	// removes the variable (mirroring the C# PtyTerminal behavior).
	Env []string

	// ForceWinPty forces the winpty backend on Windows even when ConPTY is
	// available. Ignored on Unix.
	ForceWinPty bool
}

// Connection is a live pseudo terminal session.
type Connection interface {
	// Reader returns a stream of the pty's output.
	io.Reader

	// Writer returns a stream that is fed into the pty's input.
	// Writes are delivered immediately (no internal buffering). One caveat:
	// with the winpty backend the agent needs a moment to become ready after
	// spawn, so input written too early may be lost; the web demos handle this
	// by delaying the first write.
	io.Writer

	// Close releases all resources of the session. It is safe to call
	// multiple times.
	Close() error

	// Pid returns the process id of the spawned process.
	Pid() int

	// ExitCode returns the exit code of the process, or 0 while it is
	// still running.
	ExitCode() int

	// Resize changes the terminal size of the pty.
	Resize(cols, rows int) error

	// Kill immediately terminates the process.
	Kill() error

	// Wait blocks until the process exits and returns the wait error.
	Wait() error

	// Done is closed when the process exits.
	Done() <-chan struct{}
}

// Start spawns a new process attached to a pseudo terminal and returns the
// connection to it.
func Start(opts Options) (Connection, error) {
	if opts.Rows <= 0 {
		opts.Rows = 24
	}
	if opts.Cols <= 0 {
		opts.Cols = 80
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	return startPlatform(opts)
}

func validateOptions(opts Options) error {
	if opts.App == "" {
		return fmt.Errorf("ptygo: App must be specified")
	}
	if opts.Cwd == "" {
		return fmt.Errorf("ptygo: Cwd must be specified")
	}
	return nil
}

// buildEnv merges the current process environment with the variables given in
// opts.Env. The result is a flat "KEY=VALUE" slice.
func buildEnv(opts Options) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for _, kv := range opts.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if v == "" {
				// Empty value removes the variable (C# PtyTerminal semantics).
				delete(env, k)
			} else {
				env[k] = v
			}
		}
	}
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result
}
