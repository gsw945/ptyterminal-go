// pty-cli hosts a PTY shell inside the current console.
//
// Input is forwarded as raw bytes (raw console mode), exactly like the web
// demos forward xterm.js keystrokes, so every key works (arrows, function
// keys, Ctrl/Alt combos, IME input, ...).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gsw945/ptyterminal-go/ptygo"
	"golang.org/x/term"
)

func main() {
	forceWinpty := flag.Bool("winpty", false, "force the winpty backend on Windows")
	flag.Parse()

	cols, rows := getSize()

	app, args := defaultShell()
	conn, err := ptygo.Start(ptygo.Options{
		Name:        "PtyCli",
		Rows:        rows,
		Cols:        cols,
		Cwd:         getCwd(),
		App:         app,
		Args:        args,
		Env:         []string{"FOO=bar"},
		ForceWinPty: *forceWinpty,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pty-cli: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Raw input mode: every keystroke is delivered as raw bytes and forwarded
	// verbatim to the pty. No-op when stdin is redirected (e.g. piped scripts).
	if restore, err := enterRawMode(); err == nil {
		defer restore()
	}

	// Keep the pty size in sync with the console window.
	go resizeLoop(conn)

	// stdin -> pty
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				_, _ = conn.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// pty -> stdout (drains until the pty closes, then exits)
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(os.Stdout, conn)
		close(copyDone)
	}()

	select {
	case <-conn.Done():
		// Process exited. On Windows the ConPTY host may keep the output pipe
		// open until the pseudo console is closed, so give the copy loop a
		// short grace period to drain trailing output.
		select {
		case <-copyDone:
		case <-time.After(500 * time.Millisecond):
		}
	case <-copyDone:
		// Output pipe reached EOF.
	}

	_ = conn.Wait()

	fmt.Printf("\r\n[pty process (%d) exited with code %d]\r\n", conn.Pid(), conn.ExitCode())
}

func defaultShell() (string, []string) {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), nil
	}
	return "/usr/bin/bash", []string{"--login"}
}

func getCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func getSize() (int, int) {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols <= 0 || rows <= 0 {
		return 80, 24
	}
	return cols, rows
}

func resizeLoop(conn ptygo.Connection) {
	cols, rows := getSize()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		newCols, newRows := getSize()
		if newCols != cols || newRows != rows {
			cols, rows = newCols, newRows
			_ = conn.Resize(cols, rows)
		}
	}
}
