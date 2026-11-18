package protocol

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

const (
	directionClient = "client"
	directionServer = "server"
)

func pointer[T any](value T) *T {
	return &value
}

func TestMessage(t *testing.T) {
	var (
		testVectors   *vectors
		describeID    uuid.UUID
		askID         uuid.UUID
		passiveID     uuid.UUID
		clientWant    map[string]Message
		serverWant    map[string]Message
		decodeClient  = DecodeClientMessage
		decodeServer  = DecodeServerMessage
		vectorMessage func(name string) []byte
	)

	setup := func() {
		logger.InitTestLogger()
		testVectors = loadVectors(t)
		describeID = uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
		askID = uuid.MustParse("0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b")
		passiveID = uuid.MustParse("9c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e5f")
		pcm24k := AudioFormat{Format: AudioFormatPCMS16LE, SampleRate: 24000, Channels: 1}

		clientWant = map[string]Message{
			"hello": &Hello{
				Protocol:   ProtocolVersion,
				DeviceID:   "5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10",
				AppVersion: "0.1.0",
				Locale:     "ru-RU",
				Platform:   "android-34",
				Network:    NetworkWiFi,
			},
			"describe": &Describe{
				RequestID: describeID,
				Mode:      ModeOnDemand,
				Focus:     FocusAll,
				Image:     ImageInfo{Width: 640, Height: 480, Bytes: 48213},
			},
			"ask.start": &AskStart{RequestID: askID, Trigger: TriggerListenWindow, WithImage: true},
			"ask.end":   &AskEnd{RequestID: askID, AudioMS: 1830},
			"client.metrics": &ClientMetrics{
				RequestID: describeID,
				Metrics: ClientMetricValues{
					CaptureMS:             pointer(34.0),
					EncodeMS:              pointer(21.0),
					TriggerToSendMS:       pointer(71.0),
					TriggerToAcceptedMS:   pointer(158.0),
					TriggerToFirstAudioMS: pointer(1240.0),
					PlaybackUnderruns:     pointer(0),
				},
				Network: NetworkWiFi,
			},
			"cancel": &Cancel{RequestID: describeID},
		}

		serverWant = map[string]Message{
			"welcome": &Welcome{
				SessionID:     uuid.MustParse("8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b"),
				ServerVersion: "0.1.0",
				ConfigID:      "c9f2a1d0e3b4",
				Config: ClientConfig{
					ImageMaxSide:      640,
					JPEGQuality:       80,
					PassiveIntervalMS: 3000,
					ListenWindowMS:    5000,
					MaxQuestionMS:     15000,
					AudioIn:           AudioFormat{Format: AudioFormatPCMS16LE, SampleRate: 16000, Channels: 1},
					AudioOut:          pcm24k,
				},
			},
			"answer.start": &AnswerStart{RequestID: describeID, Kind: AnswerKindDescription, Priority: PriorityNormal, Audio: &pcm24k},
			"answer.text":  &AnswerText{RequestID: describeID, Seq: 0, Text: "Вы в небольшой комнате."},
			"answer.end": &AnswerEnd{
				RequestID:      describeID,
				Text:           "Вы в небольшой комнате. Слева стул и шкаф, впереди кровать.",
				Intent:         IntentDescribe,
				DegradedReason: DegradedNone,
				Objects: []SceneObject{
					{Label: "chair", LabelRU: "стул", Direction: DirectionLeft, Distance: DistanceNear},
				},
				Timings: Timings{
					"receive_ms":            1.2,
					"detect_ms":             41.3,
					"llm_ttft_ms":           583.0,
					"llm_first_sentence_ms": 790.4,
					"tts_first_chunk_ms":    158.6,
					"server_ttfa_ms":        1001.8,
					"llm_total_ms":          1152.7,
					"server_total_ms":       1630.4,
				},
			},
			"passive.clear":              &PassiveClear{RequestID: passiveID, Reason: ClearNoHazard},
			"accepted":                   &Accepted{RequestID: describeID},
			"transcript":                 &Transcript{RequestID: askID, Text: "Где дверь?", Intent: IntentQuestion},
			"error.bad_request":          NewErrorMessage(nil, NewError(CodeBadRequest, "hello expected before other messages")),
			"error.unsupported_protocol": NewErrorMessage(nil, NewError(CodeUnsupportedProtocol, errUnsupportedProtocolFormat, 2)),
			"error.payload_too_large":    NewErrorMessage(&describeID, NewError(CodePayloadTooLarge, errImageTooLargeFormat, defaultMaxImageBytes)),
			"error.busy":                 NewErrorMessage(&describeID, NewError(CodeBusy, "detector queue is full")),
			"error.asr_empty":            NewErrorMessage(&askID, NewError(CodeASREmpty, "no speech recognized")),
			"error.asr_unavailable":      NewErrorMessage(&askID, NewError(CodeASRUnavailable, "asr service unavailable")),
			"error.detector_unavailable": NewErrorMessage(&describeID, NewError(CodeDetectorUnavailable, "detector service unavailable")),
			"error.upstream_timeout":     NewErrorMessage(&describeID, NewError(CodeUpstreamTimeout, "request deadline exceeded")),
			"error.internal":             NewErrorMessage(&describeID, fmt.Errorf("failed to describe scene: %w", errors.New("connection refused"))),
		}

		vectorMessage = func(name string) []byte {
			for _, vector := range testVectors.Messages {
				if vector.Name == name {
					return vector.Message
				}
			}
			for _, vector := range testVectors.InvalidMessages {
				if vector.Name == name {
					require.Equal(t, string(CodeBadRequest), vector.Error, name)
					return vector.Message
				}
			}
			require.FailNow(t, "no message vector "+name)
			return nil
		}
	}

	t.Run("must_pass_client_messages", func(t *testing.T) {
		setup()

		count := 0
		for _, vector := range testVectors.Messages {
			if vector.Direction != directionClient {
				continue
			}
			count++
			want, ok := clientWant[vector.Name]
			require.True(t, ok, "no expected value for the vector "+vector.Name)

			decoded, decodeErr := DecodeClientMessage(vector.Message)
			encoded, encodeErr := EncodeMessage(want)

			require.NoError(t, decodeErr, vector.Name)
			require.Equal(t, want, decoded, vector.Name)
			require.NoError(t, encodeErr, vector.Name)
			require.JSONEq(t, string(vector.Message), string(encoded), vector.Name)
			require.True(t, strings.HasPrefix(string(encoded), `{"type":"`+string(want.MessageType())+`",`), vector.Name)
		}
		require.Equal(t, len(clientWant), count)
	})

	t.Run("must_pass_server_messages", func(t *testing.T) {
		setup()

		count := 0
		for _, vector := range testVectors.Messages {
			if vector.Direction != directionServer {
				continue
			}
			count++
			want, ok := serverWant[vector.Name]
			require.True(t, ok, "no expected value for the vector "+vector.Name)

			decoded, decodeErr := DecodeServerMessage(vector.Message)
			encoded, encodeErr := EncodeMessage(want)

			require.NoError(t, decodeErr, vector.Name)
			require.Equal(t, want, decoded, vector.Name)
			require.NoError(t, encodeErr, vector.Name)
			require.JSONEq(t, string(vector.Message), string(encoded), vector.Name)
			require.True(t, strings.HasPrefix(string(encoded), `{"type":"`+string(want.MessageType())+`",`), vector.Name)
		}
		require.Equal(t, len(serverWant), count)
	})

	t.Run("must_pass_timings_one_decimal", func(t *testing.T) {
		setup()
		answerEnd := &AnswerEnd{
			RequestID: askID,
			Intent:    IntentRepeat,
			Timings: Timings{
				"asr_ms":          DurationMillis(1250 * time.Microsecond),
				"queue_ms":        DurationMillis(0),
				"server_total_ms": DurationMillis(583 * time.Millisecond),
				"llm_total_ms":    DurationMillis(1152*time.Millisecond + 749*time.Microsecond),
			},
		}

		encoded, err := EncodeMessage(answerEnd)
		reference, referenceErr := EncodeMessage(serverWant["answer.end"])

		require.NoError(t, err)
		require.Equal(t, `{"type":"answer.end","request_id":"0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b","text":"","intent":"repeat",`+
			`"hazard":false,"degraded":false,"degraded_reason":"","objects":[],`+
			`"timings":{"asr_ms":1.3,"llm_total_ms":1152.7,"queue_ms":0.0,"server_total_ms":583.0}}`, string(encoded))
		require.NoError(t, referenceErr)
		require.Contains(t, string(reference), `"llm_ttft_ms":583.0`)
		require.Contains(t, string(reference), `"receive_ms":1.2`)
	})

	t.Run("must_pass_error_codes", func(t *testing.T) {
		setup()
		retryable := map[ErrorCode]bool{
			CodeBadRequest:          false,
			CodeUnsupportedProtocol: false,
			CodePayloadTooLarge:     false,
			CodeBusy:                true,
			CodeASREmpty:            true,
			CodeASRUnavailable:      true,
			CodeDetectorUnavailable: true,
			CodeUpstreamTimeout:     true,
			CodeInternal:            true,
		}
		wrapped := fmt.Errorf("failed to detect objects: %w", NewError(CodeBusy, "detector queue is full"))
		plain := errors.New("connection refused")

		wrappedMessage := NewErrorMessage(&describeID, wrapped)
		plainMessage := NewErrorMessage(nil, plain)

		require.Len(t, retryableCodes, len(retryable))
		for code, want := range retryable {
			require.Equal(t, want, code.Retryable(), string(code))

			decoded, err := DecodeServerMessage(vectorMessage("error." + string(code)))
			require.NoError(t, err, string(code))
			require.Equal(t, code, decoded.(*ErrorMessage).Code)
			require.Equal(t, want, decoded.(*ErrorMessage).Retryable, string(code))
		}
		require.False(t, ErrorCode("teapot").Retryable())
		require.Equal(t, CodeBusy, CodeOf(wrapped))
		require.Equal(t, CodeInternal, CodeOf(plain))
		require.EqualError(t, NewError(CodeASREmpty, "no speech recognized"), "asr_empty: no speech recognized")
		require.Equal(t, &ErrorMessage{RequestID: &describeID, Code: CodeBusy, Message: "detector queue is full", Retryable: true}, wrappedMessage)
		require.Equal(t, &ErrorMessage{Code: CodeInternal, Message: msgInternalError, Retryable: true}, plainMessage)
		require.Equal(t, "internal server error", plainMessage.Message)
	})

	t.Run("must_fail_missing_type", func(t *testing.T) {
		setup()
		inputs := [][]byte{
			vectorMessage("missing_type"),
			[]byte(`{"type":"","request_id":"123e4567-e89b-12d3-a456-426614174000"}`),
			[]byte(`{}`),
			[]byte(`null`),
		}

		for _, input := range inputs {
			for _, decode := range []func([]byte) (Message, error){decodeClient, decodeServer} {
				message, err := decode(input)

				require.Nil(t, message, string(input))
				require.EqualError(t, err, string(CodeBadRequest)+": "+errMissingType, string(input))
				require.Equal(t, "bad_request: message type is missing", err.Error())
				require.Equal(t, CodeBadRequest, CodeOf(err))
			}
		}
	})

	t.Run("must_fail_unknown_type", func(t *testing.T) {
		setup()
		unknown := vectorMessage("unknown_type")

		clientMessage, clientErr := DecodeClientMessage(unknown)
		serverMessage, serverErr := DecodeServerMessage(unknown)
		welcomeMessage, welcomeErr := DecodeClientMessage(vectorMessage("welcome"))
		helloMessage, helloErr := DecodeServerMessage(vectorMessage("hello"))

		require.Nil(t, clientMessage)
		require.EqualError(t, clientErr, string(CodeBadRequest)+": "+fmt.Sprintf(errUnknownTypeFormat, "describe.start"))
		require.Equal(t, `bad_request: unknown message type "describe.start"`, clientErr.Error())
		require.Equal(t, CodeBadRequest, CodeOf(clientErr))
		require.Nil(t, serverMessage)
		require.EqualError(t, serverErr, `bad_request: unknown message type "describe.start"`)
		require.Nil(t, welcomeMessage)
		require.EqualError(t, welcomeErr, `bad_request: unknown message type "welcome"`)
		require.Nil(t, helloMessage)
		require.EqualError(t, helloErr, `bad_request: unknown message type "hello"`)
	})

	t.Run("must_fail_invalid_fields", func(t *testing.T) {
		setup()
		tests := []struct {
			name        string
			decode      func([]byte) (Message, error)
			input       string
			expectedErr string
		}{
			{
				name:        "not_json",
				decode:      decodeClient,
				input:       `{"type":"describe"`,
				expectedErr: "bad_request: message is not a JSON object: unexpected end of JSON input",
			},
			{
				name:        "not_object",
				decode:      decodeClient,
				input:       `["describe"]`,
				expectedErr: "bad_request: message is not a JSON object: json: cannot unmarshal array into Go value of type protocol.messageEnvelope",
			},
			{
				name:        "describe_bad_mode",
				decode:      decodeClient,
				input:       `{"type":"describe","request_id":"123e4567-e89b-12d3-a456-426614174000","mode":"manual","focus":"all","image":{"width":640,"height":480,"bytes":48213}}`,
				expectedErr: "bad_request: invalid describe message: validation failed for fields: mode",
			},
			{
				name:        "describe_bad_focus_and_no_image",
				decode:      decodeClient,
				input:       `{"type":"describe","request_id":"123e4567-e89b-12d3-a456-426614174000","mode":"passive","focus":"left"}`,
				expectedErr: "bad_request: invalid describe message: validation failed for fields: focus, width, height, bytes",
			},
			{
				name:        "describe_no_request_id",
				decode:      decodeClient,
				input:       `{"type":"describe","mode":"on_demand","focus":"all","image":{"width":640,"height":480,"bytes":48213}}`,
				expectedErr: "bad_request: invalid describe message: validation failed for fields: request_id",
			},
			{
				name:        "describe_request_id_not_uuid",
				decode:      decodeClient,
				input:       `{"type":"describe","request_id":"abc","mode":"on_demand","focus":"all","image":{"width":640,"height":480,"bytes":48213}}`,
				expectedErr: "bad_request: invalid describe message: invalid UUID length: 3",
			},
			{
				name:        "hello_bad_network_no_device_id",
				decode:      decodeClient,
				input:       `{"type":"hello","protocol":1,"app_version":"0.1.0","locale":"ru-RU","platform":"android-34","network":"lte"}`,
				expectedErr: "bad_request: invalid hello message: validation failed for fields: device_id, network",
			},
			{
				name:        "hello_no_protocol",
				decode:      decodeClient,
				input:       `{"type":"hello","device_id":"d1","app_version":"0.1.0","locale":"ru-RU","platform":"android-34","network":"wifi"}`,
				expectedErr: "bad_request: invalid hello message: validation failed for fields: protocol",
			},
			{
				name:        "ask_start_bad_trigger",
				decode:      decodeClient,
				input:       `{"type":"ask.start","request_id":"0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b","trigger":"tap","with_image":false}`,
				expectedErr: "bad_request: invalid ask.start message: validation failed for fields: trigger",
			},
			{
				name:        "ask_end_no_audio_ms",
				decode:      decodeClient,
				input:       `{"type":"ask.end","request_id":"0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b"}`,
				expectedErr: "bad_request: invalid ask.end message: validation failed for fields: audio_ms",
			},
			{
				name:        "client_metrics_negative_and_bad_network",
				decode:      decodeClient,
				input:       `{"type":"client.metrics","request_id":"123e4567-e89b-12d3-a456-426614174000","metrics":{"capture_ms":-1,"playback_underruns":0},"network":"5g"}`,
				expectedErr: "bad_request: invalid client.metrics message: validation failed for fields: capture_ms, network",
			},
			{
				name:        "client_metrics_wrong_type",
				decode:      decodeClient,
				input:       `{"type":"client.metrics","request_id":"123e4567-e89b-12d3-a456-426614174000","metrics":{"capture_ms":"34"},"network":"wifi"}`,
				expectedErr: "bad_request: invalid client.metrics message: json: cannot unmarshal string into Go struct field ClientMetricValues.metrics.capture_ms of type float64",
			},
			{
				name:        "welcome_bad_audio_format",
				decode:      decodeServer,
				input:       `{"type":"welcome","session_id":"8a1e2f30-5c4d-4e6f-9a8b-7c6d5e4f3a2b","server_version":"0.1.0","config_id":"c9f2a1d0e3b4","config":{"image_max_side":640,"jpeg_quality":101,"passive_interval_ms":3000,"listen_window_ms":5000,"max_question_ms":15000,"audio_in":{"format":"opus","sample_rate":16000,"channels":1},"audio_out":{"format":"pcm_s16le","sample_rate":24000,"channels":1}}}`,
				expectedErr: "bad_request: invalid welcome message: validation failed for fields: jpeg_quality, format",
			},
			{
				name:        "answer_start_bad_kind_and_priority",
				decode:      decodeServer,
				input:       `{"type":"answer.start","request_id":"123e4567-e89b-12d3-a456-426614174000","kind":"summary","priority":"low","audio":null}`,
				expectedErr: "bad_request: invalid answer.start message: validation failed for fields: kind, priority",
			},
			{
				name:        "answer_end_bad_object_and_reason",
				decode:      decodeServer,
				input:       `{"type":"answer.end","request_id":"123e4567-e89b-12d3-a456-426614174000","text":"","intent":"describe","hazard":false,"degraded":true,"degraded_reason":"llm_error","objects":[{"label":"chair","label_ru":"стул","direction":"behind","distance":"near","hazard":false}],"timings":{"detect_ms":-1.0}}`,
				expectedErr: "bad_request: invalid answer.end message: validation failed for fields: degraded_reason, direction, timings[detect_ms]",
			},
			{
				name:        "answer_end_null_objects_and_timings",
				decode:      decodeServer,
				input:       `{"type":"answer.end","request_id":"123e4567-e89b-12d3-a456-426614174000","text":"","intent":"stop","hazard":false,"degraded":false,"degraded_reason":"","objects":null,"timings":null}`,
				expectedErr: "bad_request: invalid answer.end message: validation failed for fields: objects, timings",
			},
			{
				name:        "transcript_empty_text_bad_intent",
				decode:      decodeServer,
				input:       `{"type":"transcript","request_id":"0b6a9a3e-4f7e-4b0e-8f0a-1c2d3e4f5a6b","text":"","intent":"chat"}`,
				expectedErr: "bad_request: invalid transcript message: validation failed for fields: text, intent",
			},
			{
				name:        "passive_clear_bad_reason",
				decode:      decodeServer,
				input:       `{"type":"passive.clear","request_id":"9c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e5f","reason":"none"}`,
				expectedErr: "bad_request: invalid passive.clear message: validation failed for fields: reason",
			},
			{
				name:        "error_unknown_code",
				decode:      decodeServer,
				input:       `{"type":"error","request_id":null,"code":"teapot","message":"","retryable":false}`,
				expectedErr: "bad_request: invalid error message: validation failed for fields: code, message",
			},
		}

		for _, tt := range tests {
			message, err := tt.decode([]byte(tt.input))

			require.Nil(t, message, tt.name)
			require.EqualError(t, err, tt.expectedErr, tt.name)
			require.Equal(t, CodeBadRequest, CodeOf(err), tt.name)
		}
	})

	t.Run("must_fail_unsupported_protocol", func(t *testing.T) {
		setup()
		hello := `{"type":"hello","protocol":2,"device_id":"5f0c3a52-8a0b-4d4e-9c61-3f1d2b7a9e10","app_version":"0.2.0","locale":"ru-RU","platform":"android-34","network":"wifi"}`
		helloWithOtherFields := `{"type":"hello","protocol":2,"device":{"id":"d1"}}`

		message, err := DecodeClientMessage([]byte(hello))
		otherMessage, otherErr := DecodeClientMessage([]byte(helloWithOtherFields))
		encoded, encodeErr := EncodeMessage(NewErrorMessage(nil, err))

		require.Nil(t, message)
		require.EqualError(t, err, string(CodeUnsupportedProtocol)+": "+fmt.Sprintf(errUnsupportedProtocolFormat, 2))
		require.Equal(t, CodeUnsupportedProtocol, CodeOf(err))
		require.Nil(t, otherMessage)
		require.EqualError(t, otherErr, "unsupported_protocol: unsupported protocol version 2")
		require.NoError(t, encodeErr)
		require.JSONEq(t, string(vectorMessage("error.unsupported_protocol")), string(encoded))
	})

	t.Run("must_fail_encode", func(t *testing.T) {
		setup()
		answerEnd := &AnswerEnd{RequestID: describeID, Timings: Timings{"detect_ms": Millis(math.NaN())}}

		_, nanErr := EncodeMessage(answerEnd)
		_, nilErr := EncodeMessage((*Accepted)(nil))
		_, infErr := Millis(math.Inf(1)).MarshalJSON()

		require.EqualError(t, nanErr, "failed to encode answer.end message: json: error calling MarshalJSON for type *protocol.AnswerEnd: "+
			"json: error calling MarshalJSON for type protocol.Millis: "+fmt.Sprintf(errMillisNotFiniteFormat, math.NaN()))
		require.EqualError(t, nilErr, fmt.Sprintf("failed to encode accepted message: %s", errNotJSONObject))
		require.EqualError(t, infErr, "duration is not a finite number: +Inf")
	})
}
