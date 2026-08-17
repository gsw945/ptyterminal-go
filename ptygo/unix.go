//go:build unix

package ptygo

import (
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// unixConnection is the Unix backend based on github.com/creack/pty.
type unixConnection struct {
	cmd  *exec.Cmd
	f    *os.File
	done chan struct{}

	once     sync.Once
	closeErr error
	waitErr  error
	exitCode int
}

func startUnix(opts Options) (Connection, error) {
	cmd := exec.Command(opts.App, opts.Args...)
	cmd.Dir = opts.Cwd

	env := buildEnv(opts)
	// Sanitize the environment the same way the C# PtyTerminal does: make sure
	// we did not start from inside tmux/screen and the terminal type is known.
	sanitized := make([]string, 0, len(env)+8)
	sanitized = append(sanitized, env...)
	sanitized = append(sanitized,
		"TERM=xterm-256color",
		"TMUX=",
		"TMUX_PANE=",
		"STY=",
		"WINDOW=",
		"WINDOWID=",
		"TERMCAP=",
		"COLUMNS=",
		"LINES=",
	)
	cmd.Env = sanitized

	f, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(opts.Rows),
		Cols: uint16(opts.Cols),
	})
	if err != nil {
		return nil, err
	}

	c := &unixConnection{
		cmd:  cmd,
		f:    f,
		done: make(chan struct{}),
	}
	go c.waitLoop()
	return c, nil
}

func (c *unixConnection) waitLoop() {
	c.waitErr = c.cmd.Wait()
	if c.cmd.ProcessState != nil {
		c.exitCode = c.cmd.ProcessState.ExitCode()
	}
	close(c.done)
}

func (c *unixConnection) Read(p []byte) (int, error) {
	return c.f.Read(p)
}

func (c *unixConnection) Write(p []byte) (int, error) {
	return c.f.Write(p)
}

func (c *unixConnection) Close() error {
	c.once.Do(func() {
		c.closeErr = c.f.Close()
		// Ensure the process is not left behind when the session is closed
		// while the child is still running.
		select {
		case <-c.done:
		default:
			_ = c.Kill()
		}
	})
	return c.closeErr
}

func (c *unixConnection) Pid() int {
	return c.cmd.Process.Pid
}

func (c *unixConnection) ExitCode() int {
	select {
	case <-c.done:
		return c.exitCode
	default:
		return 0
	}
}

func (c *unixConnection) Resize(cols, rows int) error {
	return pty.Setsize(c.f, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
}

func (c *unixConnection) Kill() error {
	select {
	case <-c.done:
		return nil
	default:
	}

	// The child is the session leader of the pty (setsid in forkpty), so
	// signalling the process group reaches the whole tree. Fall back to a
	// plain kill of the process itself.
	pid := c.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGHUP); err == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

func (c *unixConnection) Wait() error {
	<-c.done
	return c.waitErr
}

func (c *unixConnection) Done() <-chan struct{} {
	return c.done
}
