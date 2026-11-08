package session

import (
	"time"

	"github.com/gorilla/websocket"
)

// writeTimeout limits every write to the connection.
const writeTimeout = 10 * time.Second

// startKeepalive arms the pong timeout: every pong moves the read deadline PongTimeout
// ahead, so the connection is closed when no pong comes within PongTimeout (PROTO-9).
// The pong handler runs in the reader goroutine.
func (s *Session) startKeepalive() {
	timeout := s.server.cfg.PongTimeout
	_ = s.conn.SetReadDeadline(time.Now().Add(timeout))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(timeout))
	})
	go s.ping()
}

// ping sends a WebSocket ping every PingInterval until the session is closed (PROTO-9).
func (s *Session) ping() {
	ticker := time.NewTicker(s.server.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				s.close()
				return
			}
		}
	}
}
