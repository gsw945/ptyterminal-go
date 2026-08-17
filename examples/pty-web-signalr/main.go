// pty-web-signalr hosts a PTY shell in the browser over SignalR.
//
// Hub at /terminalHub, protocol aligned with the C# PtyWeb.SignalR demo:
//   - client -> server: input(string) / resize(cols, rows)
//   - server -> client: Output(byte[]) / Closed(exitCode)
//
// Frontend: xterm.js + @microsoft/signalr (embedded via go:embed).
package main

import (
	"context"
	"embed"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gsw945/ptyterminal-go/ptygo"
	"github.com/philippseith/signalr"
)

//go:embed index.html
var staticFS embed.FS

var forceWinpty = flag.Bool("winpty", false, "force the winpty backend on Windows")

// TerminalHub runs one pty session per SignalR connection.
// A new hub instance is created for every connection.
type TerminalHub struct {
	signalr.Hub

	conn       ptygo.Connection
	cancel     context.CancelFunc
	inputOnce  sync.Once
	resizeMu   sync.Mutex
	latestCols int
	latestRows int
}

func (h *TerminalHub) OnConnected(connectionID string) {
	app, args := defaultShell()
	conn, err := ptygo.Start(ptygo.Options{
		Rows:        30,
		Cols:        80,
		Cwd:         getCwd(),
		App:         app,
		Args:        args,
		Env:         []string{"FOO=bar"},
		ForceWinPty: *forceWinpty,
	})
	if err != nil {
		log.Printf("[%s] spawn failed: %v", connectionID, err)
		_ = h.Close("spawn failed", false)
		return
	}
	h.conn = conn

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.outputPump(ctx)

	// Stop the pump when the pty exits.
	go func() {
		<-conn.Done()
		cancel()
	}()
}

func (h *TerminalHub) OnDisconnected(connectionID string) {
	if h.cancel != nil {
		h.cancel()
	}
	if h.conn != nil {
		_ = h.conn.Close()
	}
}

// Input forwards keystrokes/text from the client (UTF-8 encoded).
// Client invokes "input". Note: hub methods must not take a context
// parameter in philippseith/signalr; use h.Context() if needed.
func (h *TerminalHub) Input(data string) {
	if data == "" {
		return
	}

	// The client can invoke a hub method before OnConnected finished
	// spawning the pty; wait for it instead of dropping the input.
	deadline := time.Now().Add(5 * time.Second)
	for h.conn == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if h.conn == nil {
		log.Printf("input dropped: pty never became ready")
		return
	}

	if *forceWinpty {
		// winpty quirk: input written before the agent finished attaching is
		// lost entirely (observed in the web/signalr demos). Delay only the
		// first write; the conpty backend has its own readiness gate.
		h.inputOnce.Do(func() { time.Sleep(2 * time.Second) })
	}

	n, err := h.conn.Write([]byte(data))
	if err != nil {
		log.Printf("input write failed: %v", err)
	} else if n != len(data) {
		log.Printf("input short write: %d/%d", n, len(data))
	}
}

// Resize resizes the pty. Client invokes "resize".
func (h *TerminalHub) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 || cols > 500 || rows > 500 {
		return
	}

	// The client can invoke a hub method before OnConnected finished
	// spawning the pty; wait for it instead of dropping the resize.
	deadline := time.Now().Add(5 * time.Second)
	for h.conn == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if h.conn == nil {
		log.Printf("resize dropped: pty never became ready")
		return
	}

	h.resizeMu.Lock()
	h.latestCols = cols
	h.latestRows = rows
	h.resizeMu.Unlock()
	h.applyResize(cols, rows)

	// The ConPTY host may ignore a resize issued right after the spawn
	// returns (S_OK but no effect); re-apply shortly afterwards. A newer
	// resize cancels the retry, so only the latest size is re-applied.
	go func() {
		time.Sleep(600 * time.Millisecond)
		h.resizeMu.Lock()
		latest := h.latestCols == cols && h.latestRows == rows
		h.resizeMu.Unlock()
		if latest {
			h.applyResize(cols, rows)
		}
	}()
}

func (h *TerminalHub) applyResize(cols, rows int) {
	if h.conn == nil {
		return
	}
	_ = h.conn.Resize(cols, rows)
}

// outputPump streams pty output to the calling client as "Output" events
// ([]byte is serialized as base64 by the default JSON protocol), then sends
// "Closed" with the exit code when the session ends.
func (h *TerminalHub) outputPump(ctx context.Context) {
	buf := make([]byte, 8192)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := h.conn.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			h.Clients().Caller().Send("Output", data)
		}
		if err != nil {
			break
		}
	}

	exitCode := h.conn.ExitCode()
	h.Clients().Caller().Send("Closed", exitCode)
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

func main() {
	addr := flag.String("addr", ":8878", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := staticFS.ReadFile("index.html")
		if err != nil {
			http.Error(w, "index.html missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	server, err := signalr.NewServer(context.Background(),
		signalr.UseHub(&TerminalHub{}),
		signalr.KeepAliveInterval(15*time.Second),
	)
	if err != nil {
		log.Fatalf("signalr.NewServer: %v", err)
	}
	// Registers /terminalHub (WebSocket) and /terminalHub/negotiate.
	server.MapHTTP(signalr.WithHTTPServeMux(mux), "/terminalHub")

	log.Printf("pty-web-signalr listening on %s (hub at /terminalHub)", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
