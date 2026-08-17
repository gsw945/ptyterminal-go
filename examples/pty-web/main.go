// pty-web hosts a PTY shell in the browser over WebSocket.
//
// Wire protocol (same as the C# PtyTerminal web demos):
//   - client -> server, text frame   : forwarded to the pty as input bytes
//   - client -> server, binary frame : JSON command, e.g.
//     {"action":"resize","data":{"cols":80,"rows":30}}
//   - server -> client, binary frame : raw pty output
//
// Frontend: xterm.js + attach addon (embedded in the binary via go:embed).
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gsw945/ptyterminal-go/ptygo"
)

//go:embed index.html
var staticFS embed.FS

type resizeCommand struct {
	Action string `json:"action"`
	Data   struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"data"`
}

func main() {
	addr := flag.String("addr", ":8877", "listen address")
	forceWinpty := flag.Bool("winpty", false, "force the winpty backend on Windows")
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
	mux.HandleFunc("/terminal", func(w http.ResponseWriter, r *http.Request) {
		handleTerminal(w, r, *forceWinpty)
	})

	log.Printf("pty-web listening on %s (WebSocket at /terminal)", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func handleTerminal(w http.ResponseWriter, r *http.Request, forceWinpty bool) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusInternalError, "session ended")

	conn, err := spawnTerminal(80, 30, forceWinpty)
	if err != nil {
		log.Printf("spawn failed: %v", err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	pumpDone := make(chan struct{}, 2)

	// Output pump: pty -> client (binary frames).
	go func() {
		defer func() { pumpDone <- struct{}{} }()
		buf := make([]byte, 8192)
		for {
			readCh := make(chan struct {
				n   int
				err error
			}, 1)
			go func() {
				n, err := conn.Read(buf)
				readCh <- struct {
					n   int
					err error
				}{n, err}
			}()
			select {
			case <-ctx.Done():
				return
			case res := <-readCh:
				if res.n > 0 {
					if err := c.Write(ctx, websocket.MessageBinary, buf[:res.n]); err != nil {
						return
					}
				}
				if res.err != nil {
					return
				}
			}
		}
	}()

	// Input pump: client -> pty.
	go func() {
		defer func() { pumpDone <- struct{}{} }()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageText:
				if len(data) > 0 {
					_, _ = conn.Write(data)
				}
			case websocket.MessageBinary:
				handleCommand(data, conn)
			}
		}
	}()

	// End the session when the pty exits or either pump stops.
	select {
	case <-pumpDone:
	case <-conn.Done():
		// The process exited; give the output pump a short grace period to
		// drain trailing output (the ConPTY pipe may not reach EOF promptly).
		select {
		case <-pumpDone:
		case <-time.After(500 * time.Millisecond):
		}
	}
	cancel()

	// Best-effort graceful close.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	_ = c.Close(websocket.StatusNormalClosure, "pty session ended")
	_ = closeCtx
}

func handleCommand(data []byte, conn ptygo.Connection) {
	var cmd resizeCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return
	}
	if strings.EqualFold(cmd.Action, "resize") &&
		cmd.Data.Cols > 0 && cmd.Data.Rows > 0 &&
		cmd.Data.Cols <= 500 && cmd.Data.Rows <= 500 {
		_ = conn.Resize(cmd.Data.Cols, cmd.Data.Rows)
	}
}

func spawnTerminal(cols, rows int, forceWinpty bool) (ptygo.Connection, error) {
	app, args := defaultShell()
	return ptygo.Start(ptygo.Options{
		Name:        "PtyWeb",
		Rows:        rows,
		Cols:        cols,
		Cwd:         getCwd(),
		App:         app,
		Args:        args,
		Env:         []string{"FOO=bar"},
		ForceWinPty: forceWinpty,
	})
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
