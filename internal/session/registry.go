package session

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// Close reason of the sessions closed on shutdown.
const msgGoingAway = "server is going away"

// drainTimeout is how long a closed session waits for its cancelled pipelines and the
// shutdown waits for the closed sessions to record their ends.
const drainTimeout = 2 * time.Second

// Registry keeps the open sessions and alv_sessions_active (PLT-1).
type Registry struct {
	active prometheus.Gauge

	mu       sync.Mutex
	sessions map[uuid.UUID]*Session
	closing  bool

	requests tracker // requests whose pipeline runs, in all sessions
	open     tracker // sessions in the registry
}

// NewRegistry creates an empty registry that reports its size to the gauge.
func NewRegistry(active prometheus.Gauge) *Registry {
	return &Registry{
		active:   active,
		sessions: map[uuid.UUID]*Session{},
	}
}

// Len returns the number of open sessions.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.sessions)
}

// Closing reports whether the registry is shutting down and takes no more sessions or requests.
func (r *Registry) Closing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.closing
}

// add registers the session; false when the registry is shutting down.
func (r *Registry) add(s *Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closing {
		return false
	}
	r.sessions[s.id] = s
	r.open.add()
	r.active.Inc()
	return true
}

// remove forgets the session.
func (r *Registry) remove(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.sessions[s.id]; ok {
		delete(r.sessions, s.id)
		r.open.done()
		r.active.Dec()
	}
}

// beginRequest counts a request whose pipeline is about to run; false when the registry
// is shutting down. The check and the count are one step under r.mu, so Shutdown either
// refuses the request or waits for it.
func (r *Registry) beginRequest() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closing {
		return false
	}
	r.requests.add()
	return true
}

// endRequest is called when the pipeline of a request counted by beginRequest returns.
func (r *Registry) endRequest() {
	r.requests.done()
}

// Shutdown takes no more sessions or requests, waits until the active requests are
// over but not longer than grace or ctx, and closes every session with the code 1001
// after the messages already queued. Then it waits until the closed sessions record
// their ends, at most 2 * drainTimeout. Every wait ends as soon as there is nothing
// left to wait for.
func (r *Registry) Shutdown(ctx context.Context, grace time.Duration) {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	if !r.requests.wait(ctx) {
		logger.Warning("shutdown grace period is over, active requests are cancelled: requests=%d", r.activeRequests())
	}

	r.mu.Lock()
	sessions := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.closeAfterQueue(websocket.CloseGoingAway, msgGoingAway)
		}()
	}
	wg.Wait()

	// The sessions record their cancelled requests and their ends after the close:
	// the journal is stopped after Shutdown, so their records are waited for
	drain, cancelDrain := context.WithTimeout(context.Background(), 2*drainTimeout)
	defer cancelDrain()
	r.open.wait(drain)
	logger.Info("sessions closed: sessions=%d", len(sessions))
}

// activeRequests returns the number of requests running in all sessions.
func (r *Registry) activeRequests() int {
	return r.requests.count()
}
