// Package history keeps the history of the assistant by device (PLT-10):
// the last scenes (objects, description, time) and the last turns of the
// dialogue (questions and answers). The history is kept by device_id, not by
// connection, so it survives a reconnect; it lives in memory and is cleared
// APP_HISTORY_TTL_S after the last activity of the device.
package history

import (
	"fmt"
	"sync"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// Texts of the errors, shared by the code and the tests.
const (
	errScenesFormat = "invalid APP_HISTORY_SCENES: a non-negative number is required: value=%d"
	errTurnsFormat  = "invalid APP_HISTORY_TURNS: a non-negative number is required: value=%d"
	errTTLFormat    = "invalid APP_HISTORY_TTL_S: a positive duration is required: value=%s"
)

// sweepInterval is how often a write also removes the expired histories of the other devices.
const sweepInterval = time.Minute

// Role is the author of a turn.
type Role string

// Roles.
const (
	RoleQuestion Role = "question" // the user
	RoleAnswer   Role = "answer"   // the assistant
)

// Scene is a scene of the device: the detected objects and the description.
type Scene struct {
	Objects     []detector.Object
	Description string
	At          time.Time // when the snapshot was received; zero - the time of AddScene
}

// Turn is one turn of the dialogue: a question or an answer.
type Turn struct {
	Role Role
	Text string
	At   time.Time // zero - the time of AddTurn
}

// History is the history of a device, the oldest first.
type History struct {
	Scenes []Scene
	Turns  []Turn
}

// LastScene returns the latest scene.
func (h History) LastScene() (Scene, bool) {
	if len(h.Scenes) == 0 {
		return Scene{}, false
	}
	return h.Scenes[len(h.Scenes)-1], true
}

// device is the stored history of a device.
type device struct {
	history      History
	lastActivity time.Time
}

// Store keeps the histories of the devices.
type Store struct {
	scenes int
	turns  int
	ttl    time.Duration
	now    func() time.Time

	mu        sync.Mutex
	devices   map[string]*device
	lastSweep time.Time
}

// NewStore creates a store that keeps the last scenes and turns of every device and forgets
// a device ttl after its last activity. now is the clock (time.Now outside the tests).
func NewStore(scenes, turns int, ttl time.Duration, now func() time.Time) (*Store, error) {
	if scenes < 0 {
		return nil, fmt.Errorf(errScenesFormat, scenes)
	}
	if turns < 0 {
		return nil, fmt.Errorf(errTurnsFormat, turns)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf(errTTLFormat, ttl)
	}
	return &Store{
		scenes:    scenes,
		turns:     turns,
		ttl:       ttl,
		now:       now,
		devices:   map[string]*device{},
		lastSweep: now(),
	}, nil
}

// AddScene adds a scene of the device, dropping the oldest above the limit. It is an activity of the device.
func (s *Store) AddScene(deviceID string, scene Scene) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if scene.At.IsZero() {
		scene.At = now
	}
	scene.Objects = append([]detector.Object(nil), scene.Objects...)

	d := s.touch(deviceID, now)
	d.history.Scenes = keepLast(append(d.history.Scenes, scene), s.scenes)
}

// AddTurn adds turns of the device (usually a question and its answer), dropping the oldest above the limit.
// It is an activity of the device.
func (s *Store) AddTurn(deviceID string, turns ...Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	d := s.touch(deviceID, now)
	for _, turn := range turns {
		if turn.At.IsZero() {
			turn.At = now
		}
		d.history.Turns = append(d.history.Turns, turn)
	}
	d.history.Turns = keepLast(d.history.Turns, s.turns)
}

// Touch marks an activity of the device without new data, for example a reconnect.
func (s *Store) Touch(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.touch(deviceID, s.now())
}

// Get returns a copy of the history of the device; an expired or unknown device has an empty history.
// Reading is not an activity.
func (s *Store) Get(deviceID string) History {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.alive(deviceID, s.now())
	if !ok {
		return History{}
	}
	history := History{
		Scenes: make([]Scene, len(d.history.Scenes)),
		Turns:  append([]Turn(nil), d.history.Turns...),
	}
	for i, scene := range d.history.Scenes {
		scene.Objects = append([]detector.Object(nil), scene.Objects...)
		history.Scenes[i] = scene
	}
	return history
}

// Devices returns the number of devices with a history that has not expired yet.
func (s *Store) Devices() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweep(s.now())
	return len(s.devices)
}

// touch returns the history of the device, a new one if it is unknown or expired, and marks the activity.
func (s *Store) touch(deviceID string, now time.Time) *device {
	if now.Sub(s.lastSweep) >= sweepInterval {
		s.sweep(now)
	}
	d, ok := s.alive(deviceID, now)
	if !ok {
		d = &device{}
		s.devices[deviceID] = d
	}
	d.lastActivity = now
	return d
}

// alive returns the history of the device if it has not expired; an expired one is removed.
func (s *Store) alive(deviceID string, now time.Time) (*device, bool) {
	d, ok := s.devices[deviceID]
	if !ok {
		return nil, false
	}
	if s.expired(d, now) {
		delete(s.devices, deviceID)
		return nil, false
	}
	return d, true
}

// sweep removes the expired histories of all devices.
func (s *Store) sweep(now time.Time) {
	for id, d := range s.devices {
		if s.expired(d, now) {
			delete(s.devices, id)
		}
	}
	s.lastSweep = now
}

// expired reports whether ttl has passed since the last activity of the device.
func (s *Store) expired(d *device, now time.Time) bool {
	return now.Sub(d.lastActivity) >= s.ttl
}

// keepLast returns the last limit items of the slice.
func keepLast[T any](items []T, limit int) []T {
	if len(items) <= limit {
		return items
	}
	return append([]T(nil), items[len(items)-limit:]...)
}
