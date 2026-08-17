package main

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestManagerNoGoroutineLeak verifies that creating and destroying sessions
// leaves no goroutines behind (pty watcher, output pump, exit notifier).
func TestManagerNoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	m := NewManager(false, func(*Session) {})

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		s, err := m.Create(80, 30)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Write some input so the output pump has something to do.
		_ = s.Write([]byte("echo leak-test\r\n"))
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			select {
			case <-s.Ended():
			case <-time.After(5 * time.Second):
			}
		}(s)
	}
	wg.Wait()

	// Destroy all sessions and give the pumps a moment to wind down.
	for _, s := range m.List() {
		m.Destroy(s.ID)
	}
	time.Sleep(300 * time.Millisecond)

	// Stop the pond worker pool; its purge goroutine lives for the pool's
	// lifetime and would otherwise be reported as a leak.
	m.Close()
}
