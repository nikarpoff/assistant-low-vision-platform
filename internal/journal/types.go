// Package journal is the request journal of the gateway in PostgreSQL (PLT-11).
//
// Records go through a bounded buffer to one writer goroutine that writes them
// in batches, so a request never waits for the database (NFR-4). When the
// buffer is full or the database is unavailable the records are dropped and
// counted in alv_journal_dropped_total. Images and audio are never stored
// (NFR-8): a request record holds only the size of the received image.
package journal

import (
	"encoding/json"
	"time"
)

// States of the journal for /ready.
const (
	StateOK       = "ok"
	StateDown     = "down"
	StateDisabled = "disabled"
)

// Record is one entry of the journal: *Session, *SessionEnd, *Request, *ClientMetrics or *ConfigSnapshot.
type Record interface {
	isRecord()
}

// Session is the start of a WebSocket session (hello); it also records the device.
type Session struct {
	SessionID  string // UUID given by the gateway
	DeviceID   string // pseudonym of the device: random UUID of the application
	AppVersion string
	Network    string // wifi, cellular, other
	Label      string // participant code (EXP-6), may be empty
	StartedAt  time.Time
}

// SessionEnd is the end of a WebSocket session; it also moves the last appearance of the device.
type SessionEnd struct {
	SessionID string
	EndedAt   time.Time
}

// Request is a finished request with the durations of its stages.
type Request struct {
	RequestID            string // UUID given by the application
	SessionID            string
	Kind                 string // describe, ask, passive
	DescriberMode        string // template, llm, vlm
	Intent               string
	Status               string // ok, degraded, error, cancelled, skipped, stale, clear
	ErrorCode            string // code of PROTO-11 for the status error
	DegradedReason       string // llm_timeout, llm_unavailable, tts_unavailable or empty
	ImageBytes           int    // size of the received image; the image itself is never stored
	Transcript           string
	AnswerText           string
	Hazard               bool
	Objects              json.RawMessage // objects of answer.end; empty is []
	ConfigID             string
	LLMModel             string // model from the provider response
	LLMSystemFingerprint string // system_fingerprint, if the provider sent it
	LLMError             string // reason of PLT-15 or empty
	CreatedAt            time.Time
	Timings              map[string]float64 // stage of PLT-14 -> duration, ms
}

// ClientMetrics are the metrics of client.metrics for a request.
type ClientMetrics struct {
	RequestID string
	Metrics   map[string]float64 // metric of PLT-14 -> value
}

// ConfigSnapshot is a configuration snapshot of PLT-12, without secrets.
type ConfigSnapshot struct {
	ConfigID    string
	Config      json.RawMessage
	CollectedAt time.Time
}

func (*Session) isRecord()        {}
func (*SessionEnd) isRecord()     {}
func (*Request) isRecord()        {}
func (*ClientMetrics) isRecord()  {}
func (*ConfigSnapshot) isRecord() {}
