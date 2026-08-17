package main

import (
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/alitto/pond"
)

// Manager owns all pty sessions. Sessions outlive their WebSocket connection:
// destroying happens either explicitly (client command) or when the pty
// process exits (e.g. typing `exit`).
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	nextID   int

	pool     *pond.WorkerPool // shared worker pool for output sends
	forceWin bool
	onEnded  func(*Session) // called when a pty exits (usually after removal)
}

func NewManager(forceWinpty bool, onEnded func(*Session)) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		pool:     pond.New(32, 1000),
		forceWin: forceWinpty,
		onEnded:  onEnded,
	}
}

// Close stops the worker pool and waits for pending output sends to finish.
// Call when the manager is no longer needed (mostly for tests/graceful
// shutdown).
func (m *Manager) Close() {
	m.pool.StopAndWait()
}

// List returns the sessions sorted by creation time.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

// Create spawns a new pty session.
func (m *Manager) Create(cols, rows int) (*Session, error) {
	m.mu.Lock()
	m.nextID++
	id := fmt.Sprintf("sess-%d", m.nextID)
	name := fmt.Sprintf("session #%d", m.nextID)
	m.mu.Unlock()

	s, err := newSession(id, name, cols, rows, m.pool, m.forceWin)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	// When the pty exits (e.g. `exit` typed by the client), remove the
	// session and notify.
	go func() {
		<-s.Ended()
		m.remove(s)
		if m.onEnded != nil {
			m.onEnded(s)
		}
	}()

	log.Printf("[%s] session created: %s", id, name)
	return s, nil
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Destroy explicitly kills and removes a session.
func (m *Manager) Destroy(id string) (*Session, bool) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil, false
	}

	s.Destroy()
	log.Printf("[%s] session destroyed", id)
	return s, true
}

// remove drops a session from the registry (called when the pty exits).
func (m *Manager) remove(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		delete(m.sessions, s.ID)
		log.Printf("[%s] session ended (exit code %d)", s.ID, s.ExitCode())
	}
}
