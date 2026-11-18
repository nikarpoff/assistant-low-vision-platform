package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// passive runs one passive request of the device whose snapshot came at receivedAt and waits for its end.
func (tp *testPipeline) passive(t *testing.T, ctx context.Context, sink *recordingSink, receivedAt time.Time) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tp.pipeline.Passive(ctx, &PassiveRequest{
			RequestID:  uuid.New(),
			SessionID:  testSessionID,
			DeviceID:   testDeviceID,
			Image:      testImage,
			StartedAt:  receivedAt,
			ReceivedAt: receivedAt,
		}, sink)
	}()
	wait(t, done, "passive")
}

// countingCompleter is a language model that only counts its calls.
type countingCompleter struct {
	calls atomic.Int32
}

func (c *countingCompleter) Complete(_ context.Context, _ *llm.Request, onText llm.TextHandler) (*llm.Result, error) {
	c.calls.Add(1)
	return streamPieces(onText, "Впереди стол и два стула.")
}

// passiveObject is an object of the tests of the passive mode.
func passiveObject(label, labelRU string, direction detector.Direction, distance detector.Distance) detector.Object {
	return detector.Object{Label: label, LabelRU: labelRU, Confidence: 0.9, Direction: direction, Distance: distance, Area: 0.2}
}

func TestPassive(t *testing.T) {
	var (
		tp      *testPipeline
		model   *countingCompleter
		clock   time.Time
		getLogs func() string
	)

	setup := func(opts pipelineOptions) {
		getLogs = logger.InitTestLogger()
		model = &countingCompleter{}
		opts.completer = model
		tp = newTestPipelineWith(t, opts)
		clock = time.Now()
		tp.pipeline.now = func() time.Time { return clock }
	}

	// run runs a passive request with the objects and returns what its sink got.
	run := func(t *testing.T, objects ...detector.Object) *recordingSink {
		t.Helper()
		tp.detector.SetObjects(objects)
		sink := &recordingSink{}
		tp.passive(t, context.Background(), sink, time.Now())
		return sink
	}

	requireClear := func(t *testing.T, sink *recordingSink, reason protocol.ClearReason) {
		t.Helper()
		messages := sink.messages()
		require.Len(t, messages, 1)
		require.Equal(t, reason, messages[0].(*protocol.PassiveClear).Reason)
	}

	requireWarning := func(t *testing.T, sink *recordingSink, priority protocol.Priority, texts ...string) *protocol.AnswerEnd {
		t.Helper()
		start := sink.messages()[0].(*protocol.AnswerStart)
		require.Equal(t, protocol.AnswerKindWarning, start.Kind)
		require.Equal(t, priority, start.Priority)
		require.NotNil(t, start.Audio)
		require.Equal(t, texts, sink.texts())
		require.Positive(t, sink.frames())
		return sink.last(t).(*protocol.AnswerEnd)
	}

	carNearLeft := passiveObject("car", "машина", detector.DirectionLeft, detector.DistanceNear)
	carFarLeft := passiveObject("car", "машина", detector.DirectionLeft, detector.DistanceFar)
	carFarAhead := passiveObject("car", "машина", detector.DirectionAhead, detector.DistanceFar)
	stairsNearAhead := passiveObject("stairs", "ступени", detector.DirectionAhead, detector.DistanceNear)
	chairFarLeft := passiveObject("chair", "стул", detector.DirectionLeft, detector.DistanceFar)

	t.Run("must_pass", func(t *testing.T) {
		setup(pipelineOptions{})

		sink := run(t, chairFarLeft)
		requireClear(t, sink, protocol.ClearNoHazard)
		require.Equal(t, StatusClear, tp.lastRecord(t).Status)

		sink = run(t, stairsNearAhead, chairFarLeft)
		end := requireWarning(t, sink, protocol.PriorityHigh, "Осторожно, впереди ступени.")
		require.True(t, end.Hazard)
		require.Equal(t, "Осторожно, впереди ступени.", end.Text)
		require.Equal(t, protocol.Intent(""), end.Intent)
		require.False(t, end.Degraded)
		require.Equal(t, []protocol.SceneObject{
			{Label: "stairs", LabelRU: "ступени", Direction: "ahead", Distance: "near", Hazard: true},
			{Label: "chair", LabelRU: "стул", Direction: "left", Distance: "far"},
		}, end.Objects)
		// The phrase of the rule goes to TTS as the cache is warmed with it: without the period
		require.Equal(t, []string{"Осторожно, впереди ступени"}, tp.tts.Texts())
		// The language model is never called in the passive mode
		require.Equal(t, int32(0), model.calls.Load())
		require.Equal(t, StatusOK, tp.lastRecord(t).Status)
		require.Equal(t, KindPassive, tp.lastRecord(t).Kind)
		require.NotContains(t, getLogs(), "ERROR:")
	})

	t.Run("must_pass_cooldown", func(t *testing.T) {
		setup(pipelineOptions{})

		requireWarning(t, run(t, carNearLeft), protocol.PriorityHigh, "Осторожно, слева близко машина.")

		// The same warning within the period is suppressed
		clock = clock.Add(3 * time.Second)
		requireClear(t, run(t, carNearLeft), protocol.ClearCooldown)

		// The car went far: no hazard; then it came closer again - the cooldown does not hold it back
		clock = clock.Add(3 * time.Second)
		requireClear(t, run(t, carFarLeft), protocol.ClearNoHazard)
		clock = clock.Add(time.Second)
		requireWarning(t, run(t, carNearLeft), protocol.PriorityHigh, "Осторожно, слева близко машина.")

		clock = clock.Add(3 * time.Second)
		requireClear(t, run(t, carNearLeft), protocol.ClearCooldown)

		// After the period the warning sounds again
		clock = clock.Add(10 * time.Second)
		requireWarning(t, run(t, carNearLeft), protocol.PriorityHigh, "Осторожно, слева близко машина.")
		require.Equal(t, int32(0), model.calls.Load())
	})

	t.Run("must_pass_stale", func(t *testing.T) {
		setup(pipelineOptions{passiveDeadlineMS: 300})
		tp.detector.SetObjects([]detector.Object{stairsNearAhead})

		// The snapshot came longer ago than APP_PASSIVE_DEADLINE_MS by the clock of the gateway
		sink := &recordingSink{}
		tp.passive(t, context.Background(), sink, time.Now().Add(-time.Second))
		requireClear(t, sink, protocol.ClearStale)
		require.Equal(t, StatusStale, tp.lastRecord(t).Status)
		require.Equal(t, 0, tp.detector.Requests())

		// The detector answers too late
		tp.detector.Hold(true)
		sink = &recordingSink{}
		started := time.Now()
		tp.passive(t, context.Background(), sink, started)
		requireClear(t, sink, protocol.ClearStale)
		require.Less(t, time.Since(started), time.Second)
		require.Eventually(t, func() bool { return tp.detector.Cancelled() == 1 }, testTimeout, 10*time.Millisecond)
		tp.detector.Hold(false)

		// The warning is synthesized too late: nothing of it is sent
		tp.tts.Delay("Осторожно, впереди ступени", time.Second)
		sink = &recordingSink{}
		tp.passive(t, context.Background(), sink, time.Now())
		requireClear(t, sink, protocol.ClearStale)
		require.Equal(t, 0, sink.frames())
		require.Equal(t, StatusStale, tp.lastRecord(t).Status)
	})

	t.Run("must_pass_skipped", func(t *testing.T) {
		setup(pipelineOptions{})
		tp.detector.SetObjects([]detector.Object{stairsNearAhead})
		tp.detector.Hold(true)

		// A passive request in progress: the next one of the session is skipped
		ctx, cancel := context.WithCancelCause(context.Background())
		first := &recordingSink{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			tp.pipeline.Passive(ctx, &PassiveRequest{RequestID: uuid.New(), SessionID: testSessionID, DeviceID: testDeviceID, Image: testImage, StartedAt: time.Now(), ReceivedAt: time.Now()}, first)
		}()
		<-tp.detector.Arrived()
		sink := &recordingSink{}
		tp.passive(t, context.Background(), sink, time.Now())
		requireClear(t, sink, protocol.ClearSkipped)
		require.Equal(t, StatusSkipped, tp.lastRecord(t).Status)
		require.Equal(t, 1, tp.detector.Requests())
		cancel(ErrCancelled)
		wait(t, done, "passive")
		require.Empty(t, first.messages())

		// A user request in progress: the passive request is skipped too
		ctx, cancel = context.WithCancelCause(context.Background())
		done = make(chan struct{})
		go func() {
			defer close(done)
			tp.pipeline.Describe(ctx, &DescribeRequest{RequestID: uuid.New(), SessionID: testSessionID, DeviceID: testDeviceID, Focus: protocol.FocusAll, Image: testImage, StartedAt: time.Now(), ReceivedAt: time.Now()}, &recordingSink{})
		}()
		<-tp.detector.Arrived()
		sink = &recordingSink{}
		tp.passive(t, context.Background(), sink, time.Now())
		requireClear(t, sink, protocol.ClearSkipped)
		cancel(ErrCancelled)
		wait(t, done, "describe")

		// Nothing is in progress any more: the passive request runs
		tp.detector.Hold(false)
		sink = &recordingSink{}
		tp.passive(t, context.Background(), sink, time.Now())
		requireWarning(t, sink, protocol.PriorityHigh, "Осторожно, впереди ступени.")
	})

	t.Run("must_pass_notice_flag", func(t *testing.T) {
		setup(pipelineOptions{})

		// Without APP_PASSIVE_NOTICES the level notice is silent
		requireClear(t, run(t, carFarAhead), protocol.ClearNoHazard)

		setup(pipelineOptions{passiveNotices: true})
		end := requireWarning(t, run(t, carFarAhead), protocol.PriorityNormal, "Впереди машина.")
		require.False(t, end.Hazard)
		require.Equal(t, []string{"Впереди машина"}, tp.tts.Texts())

		// A danger and a notice together: the danger first, priority high
		clock = clock.Add(time.Minute)
		end = requireWarning(t, run(t, carFarAhead, stairsNearAhead), protocol.PriorityHigh, "Осторожно, впереди ступени.", "Впереди машина.")
		require.True(t, end.Hazard)
		require.ElementsMatch(t, []string{"Впереди машина", "Осторожно, впереди ступени", "Впереди машина"}, tp.tts.Texts())
		require.Equal(t, int32(0), model.calls.Load())
	})
}
