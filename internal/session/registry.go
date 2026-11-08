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

// shutdownPoll is how often Shutdown checks whether the active requests are over.
const shutdownPoll = 10 * time.Millisecond

// Registry keeps the open sessions and alv_sessions_active (PLT-1).
type Registry struct {
	active prometheus.Gauge

	mu       sync.Mutex
	sessions map[uuid.UUID]*Session
	closing  bool
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
	r.active.Inc()
	return true
}

// remove forgets the session.
func (r *Registry) remove(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.sessions[s.id]; ok {
		delete(r.sessions, s.id)
		r.active.Dec()
	}
}

// Shutdown takes no more sessions or requests, waits until the active requests are
// over but not longer than grace or ctx, and closes every session with the code 1001
// after the messages already queued.
func (r *Registry) Shutdown(ctx context.Context, grace time.Duration) {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	ticker := time.NewTicker(shutdownPoll)
	defer ticker.Stop()
wait:
	for r.activeRequests() > 0 {
		select {
		case <-ctx.Done():
			logger.Warning("shutdown grace period is over, active requests are cancelled: requests=%d", r.activeRequests())
			break wait
		case <-ticker.C:
		}
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
	logger.Info("sessions closed: sessions=%d", len(sessions))
}

// activeRequests returns the number of requests running in all sessions.
func (r *Registry) activeRequests() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := 0
	for _, s := range r.sessions {
		total += int(s.active.Load())
	}
	return total
}
