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

// done counts one less; the waiters wake up when none is left. Every done goes after its
// own add: a done without add is a bug of the caller and panics with a clear message.
func (t *tracker) done() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.n <= 0 {
		panic("session: tracker done without add")
	}
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

// wait waits until none is running or ctx is over. It returns true when the count has
// gone down to zero at least once since the call, not that it is zero now: an add after
// that starts a new round the call does not wait for. So the callers forbid new adds
// before they wait. With none running it returns at once.
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
