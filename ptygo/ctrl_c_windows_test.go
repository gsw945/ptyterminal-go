//go:build windows

package ptygo

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConPTYCtrlCInterruptsPing(t *testing.T) {
	if !conptyAvailable() {
		t.Skip("ConPTY is not available on this system")
	}
	testCtrlC(t, Options{})
}

func TestWinPtyCtrlCInterruptsPing(t *testing.T) {
	if _, err := findWinptyDll(); err != nil {
		t.Skipf("winpty deps unavailable: %v", err)
	}
	testCtrlC(t, Options{ForceWinPty: true})
}

// testCtrlC starts cmd, runs ping, sends ETX (what a terminal sends for
// Ctrl+C) and verifies the shell takes the next command, which is only
// possible when ping received CTRL_C_EVENT.
func testCtrlC(t *testing.T, opts Options) {
	t.Helper()
	cmdPath := os.Getenv("ComSpec")
	if cmdPath == "" {
		cmdPath = `C:\Windows\System32\cmd.exe`
	}
	if _, err := os.Stat(cmdPath); err != nil {
		t.Skipf("cmd.exe unavailable: %v", err)
	}

	opts.Rows = 24
	opts.Cols = 80
	opts.Cwd = t.TempDir()
	opts.App = cmdPath

	conn, err := Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var mu sync.Mutex
	var output strings.Builder
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				mu.Lock()
				output.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return output.String()
	}
	waitFor := func(what string, cond func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond(snapshot()) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s; output:\n%s", what, snapshot())
	}

	waitFor("cmd prompt", func(s string) bool { return strings.Contains(s, ">") })
	if _, err := conn.Write([]byte("ping -t 127.0.0.1\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor("ping output", func(s string) bool { return strings.Contains(s, "TTL=") })

	if _, err := conn.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	// If Ctrl+C reached ping, the shell takes the next command and runs it.
	// Counting two occurrences distinguishes "executed" from "typed into ping".
	time.Sleep(300 * time.Millisecond)
	if _, err := conn.Write([]byte("echo CTRL_C_OK\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor("command executed after Ctrl+C", func(s string) bool {
		return strings.Count(s, "CTRL_C_OK") >= 2
	})

	_ = conn.Close()
	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("read loop did not stop after Close")
	}
}
