package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/journal"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/servicestub"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/tts"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Settings of the tests.
const (
	testModel     = "qwen/qwen3-8b"
	testDeviceID  = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	testSessionID = "8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b"
	testConfigID  = "c9f2a1d0e3b4"
	testTimeout   = 5 * time.Second
)

// testImage is a small fake JPEG snapshot.
var testImage = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xFF, 0xD9}

// event is a message or an AUDIO_OUT frame the sink got.
type event struct {
	message protocol.Message
	seq     uint32
	payload []byte
}

// recordingSink keeps what the pipeline sends, in order.
type recordingSink struct {
	mu     sync.Mutex
	events []event
}

func (s *recordingSink) Send(message protocol.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event{message: message})
}

func (s *recordingSink) SendAudio(seq uint32, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event{seq: seq, payload: append([]byte(nil), payload...)})
}

func (s *recordingSink) all() []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event(nil), s.events...)
}

// texts returns the texts of answer.text in order.
func (s *recordingSink) texts() []string {
	texts := []string{}
	for _, e := range s.all() {
		if m, ok := e.message.(*protocol.AnswerText); ok {
			texts = append(texts, m.Text)
		}
	}
	return texts
}

// scriptedCompleter is a language model whose stream the test writes.
type scriptedCompleter struct {
	stream func(ctx context.Context, onText llm.TextHandler) (*llm.Result, error)
}

func (c *scriptedCompleter) Complete(ctx context.Context, _ *llm.Request, onText llm.TextHandler) (*llm.Result, error) {
	return c.stream(ctx, onText)
}

// streamPieces passes the pieces to onText as a provider would; it stops at ErrStop.
func streamPieces(onText llm.TextHandler, pieces ...string) (*llm.Result, error) {
	result := &llm.Result{Model: testModel, TTFT: time.Millisecond}
	for _, piece := range pieces {
		result.Text += piece
		if err := onText(piece); err != nil {
			result.Stopped = true
			return result, nil
		}
	}
	return result, nil
}

// readVocabulary reads the vocabulary of the detector contract.
func readVocabulary(t *testing.T) *vocabulary.Vocabulary {
	t.Helper()
	body, err := os.ReadFile(servicestub.ContractPath("objects-detector", "vocabulary.response.json"))
	require.NoError(t, err)
	document := &vocabulary.Document{}
	require.NoError(t, json.Unmarshal(body, document))
	vocab, err := vocabulary.NewVocabulary(document)
	require.NoError(t, err)
	return vocab
}

// testPipeline is a pipeline over the service stubs and a scripted language model.
type testPipeline struct {
	pipeline *Pipeline
	detector *servicestub.Detector
	tts      *servicestub.TTS
	metrics  *metrics.Metrics
	recorder *RecorderMock
	sink     *recordingSink
}

// newTestPipeline builds the pipeline in the llm mode with the scripted completer.
func newTestPipeline(t *testing.T, completer describer.Completer, maxAnswerChars int) *testPipeline {
	t.Helper()
	detectorStub := servicestub.NewDetector()
	ttsStub := servicestub.NewTTS()
	t.Cleanup(detectorStub.Close)
	t.Cleanup(ttsStub.Close)

	vars := &env.Vars{
		AppDescriberMode:          env.DescriberModeLLM,
		AppLLMModel:               testModel,
		AppDetectorURL:            detectorStub.URL,
		AppDetectTimeoutMS:        1000,
		AppTTSURL:                 ttsStub.URL,
		AppTTSFirstChunkTimeoutMS: 1000,
		AppAudioOutSampleRate:     24000,
		AppMaxAnswerChars:         maxAnswerChars,
		AppRequestDeadlineMS:      8000,
	}
	m := metrics.NewMetrics()
	detectorClient, err := detector.NewClient(vars, m)
	require.NoError(t, err)
	ttsClient, err := tts.NewClient(vars, m)
	require.NoError(t, err)

	vocab := readVocabulary(t)
	rules, err := hazard.LoadRules("")
	require.NoError(t, err)
	policy, err := hazard.NewPolicy(rules, vocab)
	require.NoError(t, err)
	prompt, err := describer.LoadPrompt("default")
	require.NoError(t, err)
	sceneDescriber, err := describer.NewDescriber(describer.NewConfig(vars), vocab, policy, prompt, completer)
	require.NoError(t, err)
	store, err := history.NewStore(5, 6, time.Minute, time.Now)
	require.NoError(t, err)

	recorder := &RecorderMock{WriteFunc: func(journal.Record) {}}
	p := NewPipeline(NewConfig(vars), Dependencies{
		Detector:    detectorClient,
		Synthesizer: ttsClient,
		Describer:   sceneDescriber,
		History:     store,
		Policy:      policy,
		Recorder:    recorder,
		Config: &ConfigSourceMock{CurrentFunc: func() *snapshot.Snapshot {
			return &snapshot.Snapshot{ConfigID: testConfigID}
		}},
		Metrics: m,
	})
	return &testPipeline{pipeline: p, detector: detectorStub, tts: ttsStub, metrics: m, recorder: recorder, sink: &recordingSink{}}
}

// describe runs one describe request and waits for its end.
func (tp *testPipeline) describe(t *testing.T) {
	t.Helper()
	now := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tp.pipeline.Describe(context.Background(), &DescribeRequest{
			RequestID:  uuid.New(),
			SessionID:  testSessionID,
			DeviceID:   testDeviceID,
			Focus:      protocol.FocusAll,
			Image:      testImage,
			StartedAt:  now,
			ReceivedAt: now,
		}, tp.sink)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("describe did not finish")
	}
}

func TestSentenceStreaming(t *testing.T) {
	var (
		tp      *testPipeline
		script  func(ctx context.Context, onText llm.TextHandler) (*llm.Result, error)
		getLogs func() string
	)

	setup := func(maxAnswerChars int) {
		getLogs = logger.InitTestLogger()
		tp = newTestPipeline(t, &scriptedCompleter{stream: func(ctx context.Context, onText llm.TextHandler) (*llm.Result, error) {
			return script(ctx, onText)
		}}, maxAnswerChars)
	}

	t.Run("must_pass_first_sentence_before_stream_end", func(t *testing.T) {
		setup(220)
		first := "Впереди стол и два стула."
		ttsBeforeEnd := false
		script = func(_ context.Context, onText llm.TextHandler) (*llm.Result, error) {
			require.NoError(t, onText(first+" "))
			select {
			case text := <-tp.tts.Arrived():
				ttsBeforeEnd = text == first
			case <-time.After(testTimeout):
			}
			return streamPieces(onText, "Слева дверь, ", "далеко.")
		}

		tp.describe(t)

		require.True(t, ttsBeforeEnd)
		require.ElementsMatch(t, []string{first, "Слева дверь, далеко."}, tp.tts.Texts())
		require.Equal(t, []string{first, "Слева дверь, далеко."}, tp.sink.texts())
		require.NotContains(t, getLogs(), "ERROR:")
	})

	t.Run("must_pass_audio_in_order", func(t *testing.T) {
		setup(220)
		sentences := []string{"Осторожно, впереди близко ступени.", "Слева стул, далеко.", "Справа дверь, близко."}
		// The first sentence is synthesized the slowest: its audio still goes first.
		tp.tts.Delay(sentences[0], 150*time.Millisecond)
		script = func(_ context.Context, onText llm.TextHandler) (*llm.Result, error) {
			return streamPieces(onText, sentences[0]+" ", sentences[1]+" ", sentences[2])
		}

		tp.describe(t)

		events := tp.sink.all()
		require.IsType(t, &protocol.AnswerStart{}, events[0].message)
		require.Equal(t, 24000, events[0].message.(*protocol.AnswerStart).Audio.SampleRate)
		index := 1
		frame := uint32(0)
		for seq, sentence := range sentences {
			require.Equal(t, &protocol.AnswerText{RequestID: events[0].message.(*protocol.AnswerStart).RequestID, Seq: seq, Text: sentence}, events[index].message)
			index++
			audio := 0
			for ; events[index].message == nil; index++ {
				require.Equal(t, frame, events[index].seq)
				for _, b := range events[index].payload {
					require.Equal(t, servicestub.Marker(sentence), b)
				}
				audio += len(events[index].payload)
				frame++
			}
			require.Equal(t, servicestub.ChunksPerSentence*servicestub.ChunkBytes, audio)
		}
		require.IsType(t, &protocol.AnswerEnd{}, events[index].message)
		require.Len(t, events, index+1)
		require.Equal(t, MaxParallelSynthesis, tp.tts.MaxActive())
		times := tp.tts.Times()
		require.Len(t, times, 3)
	})

	t.Run("must_pass_short_sentence_merged", func(t *testing.T) {
		setup(220)
		script = func(_ context.Context, onText llm.TextHandler) (*llm.Result, error) {
			return streamPieces(onText, "Да. ", "Впереди стол и два стула. ", "Всё.")
		}

		tp.describe(t)

		require.Equal(t, []string{"Да. Впереди стол и два стула.", "Всё."}, tp.sink.texts())
		require.ElementsMatch(t, []string{"Да. Впереди стол и два стула.", "Всё."}, tp.tts.Texts())
		end := tp.sink.all()[len(tp.sink.all())-1].message.(*protocol.AnswerEnd)
		require.Equal(t, "Да. Впереди стол и два стула. Всё.", end.Text)
	})

	t.Run("must_pass_limit_closes_llm_stream", func(t *testing.T) {
		setup(220)
		var stopErr error
		pushed := 0
		script = func(_ context.Context, onText llm.TextHandler) (*llm.Result, error) {
			for _, piece := range []string{"Впереди стол и два стула. ", "Слева дверь, далеко. ", "Справа шкаф, близко. ", "Сзади окно, далеко. ", "Ещё одна фраза здесь."} {
				pushed++
				if stopErr = onText(piece); stopErr != nil {
					return &llm.Result{Model: testModel, Stopped: true}, nil
				}
			}
			return &llm.Result{Model: testModel}, nil
		}

		tp.describe(t)

		require.ErrorIs(t, stopErr, llm.ErrStop)
		require.Equal(t, MaxSentences, pushed)
		require.Equal(t, []string{"Впереди стол и два стула.", "Слева дверь, далеко.", "Справа шкаф, близко."}, tp.sink.texts())
		require.Len(t, tp.tts.Texts(), MaxSentences)
	})

	t.Run("must_pass_char_limit_closes_llm_stream", func(t *testing.T) {
		setup(40)
		var stopErr error
		script = func(_ context.Context, onText llm.TextHandler) (*llm.Result, error) {
			for _, piece := range []string{"Впереди стол и два стула. ", "Слева дверь, далеко. ", "Справа шкаф."} {
				if stopErr = onText(piece); stopErr != nil {
					return &llm.Result{Model: testModel, Stopped: true}, nil
				}
			}
			return &llm.Result{Model: testModel}, nil
		}

		tp.describe(t)

		require.ErrorIs(t, stopErr, llm.ErrStop)
		require.Equal(t, []string{"Впереди стол и два стула."}, tp.sink.texts())
	})

	t.Run("must_pass_cleanup", func(t *testing.T) {
		setup(220)
		script = func(_ context.Context, onText llm.TextHandler) (*llm.Result, error) {
			return streamPieces(onText, "<think>Сначала ", "опасность</think>", "**Осторожно**, впереди ", "близко ступени.\n", "- Слева 🪑 стул, ", "_далеко_. ", "## Конец")
		}

		tp.describe(t)

		require.Equal(t, []string{"Осторожно, впереди близко ступени.", "Слева стул, далеко.", "Конец"}, tp.sink.texts())
		require.ElementsMatch(t, tp.sink.texts(), tp.tts.Texts())
	})
}

func TestSentences(t *testing.T) {
	var (
		sentences *Sentences
	)

	setup := func(maxChars int) {
		sentences = NewSentences(maxChars)
	}

	push := func(pieces ...string) ([]string, bool) {
		all := []string{}
		done := false
		for _, piece := range pieces {
			got, stop := sentences.Push(piece)
			all = append(all, got...)
			if stop {
				done = true
				break
			}
		}
		return all, done
	}

	t.Run("must_pass_split_on_space_or_end", func(t *testing.T) {
		setup(220)

		got, done := push("Впереди стол", " и два стула! Слева дверь", ", далеко?", " Справа шкаф… ", "Сзади")

		require.True(t, done)
		require.Equal(t, []string{"Впереди стол и два стула!", "Слева дверь, далеко?", "Справа шкаф…"}, got)
	})

	t.Run("must_pass_no_split_without_space", func(t *testing.T) {
		setup(220)

		got, done := push("Температура 2.5 градуса на улице. Ещё")

		require.False(t, done)
		require.Equal(t, []string{"Температура 2.5 градуса на улице."}, got)
		require.Equal(t, []string{"Ещё"}, sentences.Flush())
	})

	t.Run("must_pass_last_short_sentence_kept", func(t *testing.T) {
		setup(220)

		got, _ := push("Впереди стол и два стула. Всё.")

		require.Equal(t, []string{"Впереди стол и два стула."}, got)
		require.Equal(t, []string{"Всё."}, sentences.Flush())
		require.Nil(t, sentences.Flush())
	})

	t.Run("must_pass_short_sentences_joined_until_long", func(t *testing.T) {
		setup(220)

		got, _ := push("Да. Нет. Впереди стол. ")

		require.Equal(t, []string{"Да. Нет. Впереди стол."}, got)
	})

	t.Run("must_pass_first_sentence_cut_at_limit", func(t *testing.T) {
		setup(20)

		got, done := push("Впереди большой деревянный стол и много стульев")

		require.True(t, done)
		require.Equal(t, []string{"Впереди большой"}, got)
	})

	t.Run("must_pass_pending_over_limit_stops", func(t *testing.T) {
		setup(30)

		got, done := push("Впереди стол и два стула. Слева дверь и ещё")

		require.True(t, done)
		require.Equal(t, []string{"Впереди стол и два стула."}, got)
	})
}

func TestCleanup(t *testing.T) {
	cases := []struct {
		name string
		in   string
		out  string
	}{
		{"must_pass_plain", "Впереди стол.", "Впереди стол."},
		{"must_pass_think_block", "<think>рассуждение</think>Впереди стол.", "Впереди стол."},
		{"must_pass_unclosed_think_block", "Впереди стол. <think>рассуж", "Впереди стол. "},
		{"must_pass_markup", "**Впереди** _стол_ `код` # заголовок", "Впереди стол код заголовок"},
		{"must_pass_list_markers", "- Стол.\n* Стул.\n1. Дверь.\n2) Окно.\n• Шкаф.", "Стол. Стул. Дверь. Окно. Шкаф."},
		{"must_pass_emoji", "Впереди стол 🙂 и стул👍🏽.", "Впереди стол и стул."},
		{"must_pass_spaces", "  Впереди   стол.\n\nСлева  стул. ", "Впереди стол. Слева стул. "},
		{"must_pass_dash_inside_line", "Стол - впереди", "Стол - впереди"},
		{"must_pass_number_not_marker", "2 стула впереди", "2 стула впереди"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.out, Cleanup(c.in))
		})
	}
}

func TestTimings(t *testing.T) {
	var (
		timings *Timings
		m       *metrics.Metrics
	)

	setup := func() {
		timings = NewTimings()
		m = metrics.NewMetrics()
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()

		timings.Set(StageDetect, 41300*time.Microsecond)
		timings.SetOnce(StageServerTTFA, 1001800*time.Microsecond)
		timings.SetOnce(StageServerTTFA, 2*time.Second)
		timings.Observe(m, KindDescribe)

		require.Equal(t, protocol.Timings{StageDetect: 41.3, StageServerTTFA: 1001.8}, timings.Protocol())
		require.Equal(t, map[string]float64{StageDetect: 41.3, StageServerTTFA: 1001.8}, timings.Milliseconds())
		require.True(t, timings.Has(StageDetect))
		require.False(t, timings.Has(StageASR))
	})
}
