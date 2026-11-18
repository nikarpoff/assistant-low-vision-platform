package hazard

import (
	"sync"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// pair is the key of the cooldown: one warning per class and direction (PLT-6).
type pair struct {
	class     string
	direction detector.Direction
}

// pairState is what the cooldown remembers about a pair.
type pairState struct {
	warned   bool
	warnedAt time.Time
	distance detector.Distance // distance of the pair in the last frame it was seen in
}

// Cooldown suppresses repeated warnings of one device (PLT-6): a warning by the pair
// «class, direction» sounds not more often than once a period (APP_HAZARD_COOLDOWN_MS).
// The exception is an object that came closer: the pair was far in the last frame it was
// seen in and is near now. Each device (or session) keeps its own Cooldown.
type Cooldown struct {
	period time.Duration
	now    func() time.Time

	mu    sync.Mutex
	pairs map[pair]*pairState
}

// NewCooldown creates a cooldown with the period and the clock (time.Now outside the tests).
func NewCooldown(period time.Duration, now func() time.Time) *Cooldown {
	return &Cooldown{
		period: period,
		now:    now,
		pairs:  map[pair]*pairState{},
	}
}

// Apply returns the hazards to speak now, in their order, and marks them as spoken.
// A hazard is suppressed while its pair is in the period, unless the pair came closer (far → near);
// the second hazard of one pair in the same frame is suppressed too. Then Apply remembers the
// distance of every pair of the frame - the objects and the hazards - for the far → near exception,
// so the caller passes all objects of the frame, not only the hazards.
func (c *Cooldown) Apply(hazards []Match, objects []detector.Object) []Match {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	speak := []Match{}
	spoken := map[pair]bool{}
	for _, hazard := range hazards {
		key := pair{class: hazard.Object.Label, direction: hazard.Object.Direction}
		if spoken[key] || !c.allowed(key, hazard.Object.Distance, now) {
			continue
		}
		spoken[key] = true
		speak = append(speak, hazard)
	}

	for key := range spoken {
		state := c.state(key)
		state.warned = true
		state.warnedAt = now
	}
	c.observe(hazards, objects)

	return speak
}

// allowed reports whether a warning by the pair at the distance may sound now.
func (c *Cooldown) allowed(key pair, distance detector.Distance, now time.Time) bool {
	state, ok := c.pairs[key]
	if !ok || !state.warned || now.Sub(state.warnedAt) >= c.period {
		return true
	}
	return state.distance == detector.DistanceFar && distance == detector.DistanceNear
}

// observe remembers the distance of every pair of the frame: near if any object of the pair is near.
func (c *Cooldown) observe(hazards []Match, objects []detector.Object) {
	frame := map[pair]detector.Distance{}
	see := func(object *detector.Object) {
		key := pair{class: object.Label, direction: object.Direction}
		if frame[key] != detector.DistanceNear {
			frame[key] = object.Distance
		}
	}
	for i := range objects {
		see(&objects[i])
	}
	for i := range hazards {
		see(&hazards[i].Object)
	}
	for key, distance := range frame {
		c.state(key).distance = distance
	}
}

// state returns the state of the pair, creating it.
func (c *Cooldown) state(key pair) *pairState {
	state, ok := c.pairs[key]
	if !ok {
		state = &pairState{}
		c.pairs[key] = state
	}
	return state
}
