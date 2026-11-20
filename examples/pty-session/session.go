package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/alitto/pond"
	"github.com/coder/websocket"
	"github.com/gsw945/ptyterminal-go/ptygo"
)

// historySize is how much pty output is buffered per session, so a browser
// that reconnects can replay the screen content.
const historySize = 64 * 1024

// sendFn writes a message to the attached client. The closure must be
// serialized by the connection's own write mutex (coder/websocket Conn is
// not safe for concurrent writes).
type sendFn func(websocket.MessageType, []byte) error

// Session is one pty session. It lives independently of any browser
// connection: when the WebSocket closes the pty keeps running and its output
// is buffered in history until a client attaches again.
type Session struct {
	ID      string
	Name    string
	Created time.Time

	conn   ptygo.Connection
	cancel context.CancelFunc

	mu       sync.Mutex
	sendFn   sendFn // currently attached client (nil when detached)
	ctx      context.Context
	exited   bool
	exitCode int
	attachID int // increments on every attach; invalidates stale sends

	history *historyBuffer
	pool    *pond.WorkerPool // shared worker pool for output sends

	ended chan struct{} // closed when the pty exits
}

// historyBuffer is a simple fixed-size ring buffer of the raw pty output.
type historyBuffer struct {
	mu     sync.Mutex
	buf    []byte
	start  int
	length int
}

func newHistoryBuffer(size int) *historyBuffer {
	return &historyBuffer{buf: make([]byte, size)}
}

func (h *historyBuffer) Write(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range p {
		h.buf[h.start] = b
		h.start = (h.start + 1) % len(h.buf)
		if h.length < len(h.buf) {
			h.length++
		}
	}
}

// Bytes returns the buffered output in chronological order.
func (h *historyBuffer) Bytes() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]byte, h.length)
	if h.length == 0 {
		return out
	}
	begin := h.start - h.length
	if begin < 0 {
		begin += len(h.buf)
	}
	for i := 0; i < h.length; i++ {
		out[i] = h.buf[(begin+i)%len(h.buf)]
	}
	return out
}

// newSession spawns the pty. The caller must add it to the manager.
func newSession(id, name string, cols, rows int, pool *pond.WorkerPool, forceWinpty bool) (*Session, error) {
	app, args := defaultShell()
	conn, err := ptygo.Start(ptygo.Options{
		Rows:        rows,
		Cols:        cols,
		Cwd:         getCwd(),
		App:         app,
		Args:        args,
		Env:         []string{"FOO=bar"},
		ForceWinPty: forceWinpty,
	})
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:      id,
		Name:    name,
		Created: time.Now(),
		conn:    conn,
		history: newHistoryBuffer(historySize),
		pool:    pool,
		ended:   make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.cancel = cancel

	go s.outputPump()
	go func() {
		<-conn.Done()
		s.mu.Lock()
		s.exited = true
		s.exitCode = conn.ExitCode()
		s.mu.Unlock()
		cancel()
		close(s.ended)
	}()
	return s, nil
}

// outputPump reads pty output, appends it to the history buffer and forwards
// it to the currently attached client. Sends run on the shared pond pool so
// a slow client cannot stall the pty read loop.
func (s *Session) outputPump() {
	buf := make([]byte, 8192)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			s.history.Write(data)

			s.mu.Lock()
			send := s.sendFn
			attachID := s.attachID
			s.mu.Unlock()
			if send != nil {
				s.pool.Submit(func() {
					s.mu.Lock()
					stillCurrent := attachID == s.attachID
					s.mu.Unlock()
					if !stillCurrent {
						return
					}
					if err := send(websocket.MessageBinary, data); err != nil {
						log.Printf("[%s] output send failed: %v", s.ID, err)
					}
				})
			}
		}
		if err != nil {
			return
		}
	}
}

// Attach binds a client to the session and returns the buffered history to
// replay. A previous client is detached and notified.
func (s *Session) Attach(send sendFn) []byte {
	s.mu.Lock()
	old := s.sendFn
	s.sendFn = send
	s.attachID++
	s.mu.Unlock()

	if old != nil {
		notify := map[string]interface{}{
			"type": "detached",
			"data": map[string]interface{}{"id": s.ID, "reason": "attached elsewhere"},
		}
		if b, err := json.Marshal(notify); err == nil {
			_ = old(websocket.MessageText, b)
		}
	}
	return s.history.Bytes()
}

// Detach unbinds the current client without destroying the session.
func (s *Session) Detach() {
	s.mu.Lock()
	s.sendFn = nil
	s.attachID++
	s.mu.Unlock()
}

// Notify sends a JSON event to the attached client, if any.
func (s *Session) Notify(v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	send := s.sendFn
	s.mu.Unlock()
	if send != nil {
		_ = send(websocket.MessageText, b)
	}
}

// Write forwards client input to the pty. The ptygo winpty backend gates the
// first write on the shell's first output, so no caller-side delay is needed.
func (s *Session) Write(data []byte) error {
	n, err := s.conn.Write(data)
	log.Printf("[%s] input write n=%d err=%v", s.ID, n, err)
	return err
}

// Resize resizes the pty.
func (s *Session) Resize(cols, rows int) error {
	return s.conn.Resize(cols, rows)
}

// ExitCode returns the pty exit code (0 while running).
func (s *Session) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

// Ended is closed when the pty process exits.
func (s *Session) Ended() <-chan struct{} {
	return s.ended
}

// Destroy kills the pty and releases the session resources.
func (s *Session) Destroy() {
	s.cancel()
	_ = s.conn.Close()
}

// Info is the session descriptor sent in list responses.
type Info struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Created  int64  `json:"created"`
	Running  bool   `json:"running"`
	ExitCode int    `json:"exitCode,omitempty"`
}

func (s *Session) info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{
		ID:       s.ID,
		Name:     s.Name,
		Created:  s.Created.UnixMilli(),
		Running:  !s.exited,
		ExitCode: s.exitCode,
	}
}
