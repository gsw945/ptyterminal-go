// pty-session demonstrates pty session management over WebSocket:
//
//   - sessions outlive the browser connection (closing the page keeps the pty
//     running); a later visit can list, attach and resume any session
//   - a session is destroyed explicitly by the client, or automatically when
//     the pty process exits (e.g. typing `exit`)
//   - output is buffered per session (64 KB ring) and replayed on attach
//
// Wire protocol (binary frames are JSON commands, text frames are pty input
// while attached):
//
//	client -> server:
//	  {"action":"list"}
//	  {"action":"create","data":{"cols":80,"rows":30}}
//	  {"action":"attach","data":{"id":"sess-1"}}
//	  {"action":"detach"}
//	  {"action":"destroy","data":{"id":"sess-1"}}
//	  {"action":"resize","data":{"cols":120,"rows":40}}
//	  <text frame>                          -> input to the attached session
//
//	server -> client:
//	  {"type":"list","data":[...]}          {"type":"created","data":{...}}
//	  {"type":"attached","data":{...}}      {"type":"destroyed","data":{...}}
//	  {"type":"detached","data":{...}}      {"type":"session-ended","data":{...}}
//	  {"type":"error","data":"..."}
//	  <binary frame>                        -> pty output (live or replay)
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
	"sync"
	"time"

	"github.com/coder/websocket"
)

//go:embed index.html
var staticFS embed.FS

var forceWinpty = flag.Bool("winpty", false, "force the winpty backend on Windows")

// wsConn wraps a websocket connection with a write mutex: coder/websocket
// Conn is not safe for concurrent writes, and session output pumps write
// asynchronously while the read loop answers commands.
type wsConn struct {
	c      *websocket.Conn
	writeMu sync.Mutex
}

func (w *wsConn) send(typ websocket.MessageType, data []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.c.Write(ctx, typ, data)
}

func (w *wsConn) sendJSON(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.send(websocket.MessageText, b)
}

// command is a binary frame from the client.
type command struct {
	Action string `json:"action"`
	Data   struct {
		ID   string `json:"id"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	} `json:"data"`
}

func main() {
	addr := flag.String("addr", ":8879", "listen address")
	flag.Parse()

	manager := NewManager(*forceWinpty, func(s *Session) {
		// The pty exited (e.g. `exit`): tell the attached client so it can
		// refresh the list.
		s.Notify(map[string]interface{}{
			"type": "session-ended",
			"data": map[string]interface{}{"id": s.ID, "exitCode": s.ExitCode()},
		})
	})

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
		handleTerminal(w, r, manager)
	})

	log.Printf("pty-session listening on %s (WebSocket at /terminal)", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func handleTerminal(w http.ResponseWriter, r *http.Request, manager *Manager) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn := &wsConn{c: c}
	defer c.Close(websocket.StatusNormalClosure, "bye")

	ctx := r.Context()
	var current *Session
	defer func() {
		if current != nil {
			current.Detach()
		}
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}

		if typ == websocket.MessageText {
			// Terminal input while attached.
			if current != nil && len(data) > 0 {
				if err := current.Write(data); err != nil {
					log.Printf("[%s] input write failed: %v", current.ID, err)
				}
			}
			continue
		}

		var cmd command
		if err := json.Unmarshal(data, &cmd); err != nil {
			_ = conn.sendJSON(map[string]interface{}{"type": "error", "data": "bad command"})
			continue
		}

		switch cmd.Action {
		case "list":
			_ = conn.sendJSON(map[string]interface{}{"type": "list", "data": manager.List()})

		case "create":
			cols, rows := cmd.Data.Cols, cmd.Data.Rows
			if cols <= 0 {
				cols = 80
			}
			if rows <= 0 {
				rows = 30
			}
			s, err := manager.Create(cols, rows)
			if err != nil {
				_ = conn.sendJSON(map[string]interface{}{"type": "error", "data": err.Error()})
				continue
			}
			if current != nil {
				current.Detach()
			}
			current = s
			s.Attach(conn.send)
			_ = conn.sendJSON(map[string]interface{}{
				"type": "created",
				"data": map[string]interface{}{"id": s.ID, "name": s.Name},
			})
			_ = conn.sendJSON(map[string]interface{}{
				"type": "attached",
				"data": map[string]interface{}{"id": s.ID, "name": s.Name, "running": s.info().Running},
			})

		case "attach":
			s, ok := manager.Get(cmd.Data.ID)
			if !ok {
				_ = conn.sendJSON(map[string]interface{}{"type": "error", "data": "session not found"})
				continue
			}
			if current != nil {
				current.Detach()
			}
			current = s
			history := s.Attach(conn.send)
			_ = conn.sendJSON(map[string]interface{}{
				"type": "attached",
				"data": map[string]interface{}{"id": s.ID, "name": s.Name, "running": s.info().Running},
			})
			if len(history) > 0 {
				_ = conn.send(websocket.MessageBinary, history)
			}

		case "detach":
			if current != nil {
				current.Detach()
				current = nil
			}
			_ = conn.sendJSON(map[string]interface{}{"type": "detached", "data": map[string]interface{}{"reason": "client detach"}})

		case "destroy":
			s, ok := manager.Destroy(cmd.Data.ID)
			if !ok {
				_ = conn.sendJSON(map[string]interface{}{"type": "error", "data": "session not found"})
				continue
			}
			if current == s {
				current = nil
			}
			_ = conn.sendJSON(map[string]interface{}{"type": "destroyed", "data": map[string]interface{}{"id": s.ID}})

		case "resize":
			if current != nil && cmd.Data.Cols > 0 && cmd.Data.Rows > 0 &&
				cmd.Data.Cols <= 500 && cmd.Data.Rows <= 500 {
				_ = current.Resize(cmd.Data.Cols, cmd.Data.Rows)
			}
		}
	}
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
