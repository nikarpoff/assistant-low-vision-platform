package session

import (
	"context"
	"sync"
)

// tracker counts running things and lets a goroutine wait until there are none: the
// wait wakes up on the event, without polling.
type tracker struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n goes back to 0; nil while n is 0
}

// add counts one more.
func (t *tracker) add() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.n == 0 {
		t.idle = make(chan struct{})
	}
	t.n++
}

// done counts one less; the waiters wake up when none is left.
func (t *tracker) done() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.n--
	if t.n == 0 {
		close(t.idle)
		t.idle = nil
	}
}

// count returns how many are running.
func (t *tracker) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.n
}

// wait waits until none is running or ctx is over; true when none is running. With none
// running it returns at once.
func (t *tracker) wait(ctx context.Context) bool {
	t.mu.Lock()
	idle := t.idle
	t.mu.Unlock()

	if idle == nil {
		return true
	}
	select {
	case <-idle:
		return true
	case <-ctx.Done():
		return false
	}
}
