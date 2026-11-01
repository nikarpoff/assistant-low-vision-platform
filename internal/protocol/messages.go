package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/utils"
)

// ProtocolVersion is the protocol version the gateway supports in hello (PROTO-10).
const ProtocolVersion = 1

// AudioFormatPCMS16LE is the audio format of AUDIO_IN and AUDIO_OUT: PCM signed 16-bit little-endian.
const AudioFormatPCMS16LE = "pcm_s16le"

// MessageType is the value of the type field of a JSON message.
type MessageType string

// Message types of the application (PROTO-4).
const (
	TypeHello         MessageType = "hello"
	TypeDescribe      MessageType = "describe"
	TypeAskStart      MessageType = "ask.start"
	TypeAskEnd        MessageType = "ask.end"
	TypeCancel        MessageType = "cancel"
	TypeClientMetrics MessageType = "client.metrics"
)

// Message types of the gateway (PROTO-5).
const (
	TypeWelcome      MessageType = "welcome"
	TypeAccepted     MessageType = "accepted"
	TypeTranscript   MessageType = "transcript"
	TypeAnswerStart  MessageType = "answer.start"
	TypeAnswerText   MessageType = "answer.text"
	TypeAnswerEnd    MessageType = "answer.end"
	TypePassiveClear MessageType = "passive.clear"
	TypeError        MessageType = "error"
)

// Network is the network of the phone.
type Network string

// Networks.
const (
	NetworkWiFi     Network = "wifi"
	NetworkCellular Network = "cellular"
	NetworkOther    Network = "other"
)

// DescribeMode is what triggered describe.
type DescribeMode string

// Describe modes.
const (
	ModeOnDemand DescribeMode = "on_demand"
	ModePassive  DescribeMode = "passive"
)

// Focus is the part of the scene to describe.
type Focus string

// Focuses.
const (
	FocusAll   Focus = "all"
	FocusAhead Focus = "ahead"
)

// AskTrigger is what started the question.
type AskTrigger string

// Question triggers.
const (
	TriggerHold         AskTrigger = "hold"
	TriggerListenWindow AskTrigger = "listen_window"
)

// Intent is the intent of the question (PLT-4).
type Intent string

// Intents (PLT-4).
const (
	IntentDescribe      Intent = "describe"
	IntentDescribeAhead Intent = "describe_ahead"
	IntentRepeat        Intent = "repeat"
	IntentStop          Intent = "stop"
	IntentPassiveOn     Intent = "passive_on"
	IntentPassiveOff    Intent = "passive_off"
	IntentQuestion      Intent = "question"
)

// AnswerKind is the kind of the answer in answer.start.
type AnswerKind string

// Answer kinds.
const (
	AnswerKindDescription AnswerKind = "description"
	AnswerKindAnswer      AnswerKind = "answer"
	AnswerKindWarning     AnswerKind = "warning"
)

// Priority is the priority of the answer in answer.start.
type Priority string

// Priorities.
const (
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

// DegradedReason is the reason of a degraded answer in answer.end (PLT-9).
type DegradedReason string

// Degraded reasons; DegradedNone is an answer that is not degraded.
const (
	DegradedNone           DegradedReason = ""
	DegradedLLMTimeout     DegradedReason = "llm_timeout"
	DegradedLLMUnavailable DegradedReason = "llm_unavailable"
	DegradedTTSUnavailable DegradedReason = "tts_unavailable"
)

// ClearReason is the reason of passive.clear.
type ClearReason string

// Passive clear reasons.
const (
	ClearNoHazard ClearReason = "no_hazard"
	ClearCooldown ClearReason = "cooldown"
	ClearSkipped  ClearReason = "skipped"
	ClearStale    ClearReason = "stale"
)

// Direction is the direction to an object (DET-4).
type Direction string

// Directions.
const (
	DirectionLeft  Direction = "left"
	DirectionAhead Direction = "ahead"
	DirectionRight Direction = "right"
)

// Distance is the distance to an object (DET-4).
type Distance string

// Distances.
const (
	DistanceNear Distance = "near"
	DistanceFar  Distance = "far"
)

// Message is a JSON message of the protocol. The decoders return pointers to the message structs.
type Message interface {
	MessageType() MessageType
}

// Hello is the first message of the application after the connection is open.
type Hello struct {
	Protocol   int     `json:"protocol" validate:"required"`
	DeviceID   string  `json:"device_id" validate:"required"`
	AppVersion string  `json:"app_version" validate:"required"`
	Locale     string  `json:"locale" validate:"required"`
	Platform   string  `json:"platform" validate:"required"` // android-<API>
	Network    Network `json:"network" validate:"required,oneof=wifi cellular other"`
	Label      *string `json:"label,omitempty"` // participant code of the trials (EXP-6)
}

// Describe asks to describe the scene; the IMAGE_JPEG frame with the same request_id follows it.
type Describe struct {
	RequestID uuid.UUID    `json:"request_id" validate:"required"`
	Mode      DescribeMode `json:"mode" validate:"required,oneof=on_demand passive"`
	Focus     Focus        `json:"focus" validate:"required,oneof=all ahead"`
	Image     ImageInfo    `json:"image"`
}

// ImageInfo describes the snapshot of describe.
type ImageInfo struct {
	Width  int `json:"width" validate:"gt=0"`
	Height int `json:"height" validate:"gt=0"`
	Bytes  int `json:"bytes" validate:"gt=0"`
}

// AskStart starts a question; an optional IMAGE_JPEG frame and AUDIO_IN frames follow it.
type AskStart struct {
	RequestID uuid.UUID  `json:"request_id" validate:"required"`
	Trigger   AskTrigger `json:"trigger" validate:"required,oneof=hold listen_window"`
	WithImage bool       `json:"with_image"`
}

// AskEnd ends a question.
type AskEnd struct {
	RequestID uuid.UUID `json:"request_id" validate:"required"`
	AudioMS   float64   `json:"audio_ms" validate:"gt=0"` // milliseconds
}

// Cancel cancels a request: the user interrupted the answer or started a new request.
type Cancel struct {
	RequestID uuid.UUID `json:"request_id" validate:"required"`
}

// ClientMetrics carries the metrics measured on the phone after an answer or an error (PLT-14).
type ClientMetrics struct {
	RequestID uuid.UUID          `json:"request_id" validate:"required"`
	Metrics   ClientMetricValues `json:"metrics"`
	Network   Network            `json:"network" validate:"required,oneof=wifi cellular other"`
}

// ClientMetricValues are the phone metrics (PLT-14), milliseconds. A metric the request does not
// have, such as the first audio after an error, is absent.
type ClientMetricValues struct {
	CaptureMS             *float64 `json:"capture_ms,omitempty" validate:"omitempty,gte=0"`
	EncodeMS              *float64 `json:"encode_ms,omitempty" validate:"omitempty,gte=0"`
	TriggerToSendMS       *float64 `json:"trigger_to_send_ms,omitempty" validate:"omitempty,gte=0"`
	TriggerToAcceptedMS   *float64 `json:"trigger_to_accepted_ms,omitempty" validate:"omitempty,gte=0"`
	TriggerToFirstAudioMS *float64 `json:"trigger_to_first_audio_ms,omitempty" validate:"omitempty,gte=0"`
	PlaybackUnderruns     *int     `json:"playback_underruns,omitempty" validate:"omitempty,gte=0"`
}

// Welcome answers hello with the session and the settings of the application (PROTO-13).
type Welcome struct {
	SessionID     uuid.UUID    `json:"session_id" validate:"required"`
	ServerVersion string       `json:"server_version" validate:"required"`
	ConfigID      string       `json:"config_id" validate:"required"`
	Config        ClientConfig `json:"config"`
}

// ClientConfig are the settings the application applies on every connection (PROTO-13).
type ClientConfig struct {
	ImageMaxSide      int         `json:"image_max_side" validate:"gt=0"`
	JPEGQuality       int         `json:"jpeg_quality" validate:"gt=0,lte=100"`
	PassiveIntervalMS int         `json:"passive_interval_ms" validate:"gt=0"`
	ListenWindowMS    int         `json:"listen_window_ms" validate:"gt=0"`
	MaxQuestionMS     int         `json:"max_question_ms" validate:"gt=0"`
	AudioIn           AudioFormat `json:"audio_in"`
	AudioOut          AudioFormat `json:"audio_out"`
}

// AudioFormat is the format of AUDIO_IN or AUDIO_OUT payloads.
type AudioFormat struct {
	Format     string `json:"format" validate:"required,oneof=pcm_s16le"`
	SampleRate int    `json:"sample_rate" validate:"gt=0"` // Hz
	Channels   int    `json:"channels" validate:"gt=0"`
}

// Accepted tells that the input of the request is received completely: the snapshot of describe or ask.end.
type Accepted struct {
	RequestID uuid.UUID `json:"request_id" validate:"required"`
}

// Transcript carries the recognized question and its intent.
type Transcript struct {
	RequestID uuid.UUID `json:"request_id" validate:"required"`
	Text      string    `json:"text" validate:"required"`
	Intent    Intent    `json:"intent" validate:"required,oneof=describe describe_ahead repeat stop passive_on passive_off question"`
}

// AnswerStart starts an answer. Audio is nil when the answer has no sound.
type AnswerStart struct {
	RequestID uuid.UUID    `json:"request_id" validate:"required"`
	Kind      AnswerKind   `json:"kind" validate:"required,oneof=description answer warning"`
	Priority  Priority     `json:"priority" validate:"required,oneof=normal high"`
	Audio     *AudioFormat `json:"audio"`
}

// AnswerText carries the text of a sentence before its audio.
type AnswerText struct {
	RequestID uuid.UUID `json:"request_id" validate:"required"`
	Seq       int       `json:"seq" validate:"gte=0"` // number of the sentence, from 0
	Text      string    `json:"text" validate:"required"`
}

// AnswerEnd ends an answer. Text is empty for the commands the application executes (PROTO-7).
type AnswerEnd struct {
	RequestID      uuid.UUID      `json:"request_id" validate:"required"`
	Text           string         `json:"text"`
	Intent         Intent         `json:"intent" validate:"omitempty,oneof=describe describe_ahead repeat stop passive_on passive_off question"`
	Hazard         bool           `json:"hazard"`
	Degraded       bool           `json:"degraded"`
	DegradedReason DegradedReason `json:"degraded_reason" validate:"omitempty,oneof=llm_timeout llm_unavailable tts_unavailable"`
	Objects        []SceneObject  `json:"objects" validate:"required,dive"`
	Timings        Timings        `json:"timings" validate:"required,dive,gte=0"`
}

// SceneObject is an object of the scene in answer.end.
type SceneObject struct {
	Label     string    `json:"label" validate:"required"`
	LabelRU   string    `json:"label_ru" validate:"required"`
	Direction Direction `json:"direction" validate:"required,oneof=left ahead right"`
	Distance  Distance  `json:"distance" validate:"required,oneof=near far"`
	Hazard    bool      `json:"hazard"`
}

// Timings are the stages of the request measured by the gateway: stage name (PLT-14) to milliseconds.
type Timings map[string]Millis

// PassiveClear ends a passive request without a warning.
type PassiveClear struct {
	RequestID uuid.UUID   `json:"request_id" validate:"required"`
	Reason    ClearReason `json:"reason" validate:"required,oneof=no_hazard cooldown skipped stale"`
}

// ErrorMessage tells that a request failed or a frame or message was rejected (PROTO-11).
// RequestID is nil (null) when the error does not belong to a request.
type ErrorMessage struct {
	RequestID *uuid.UUID `json:"request_id"`
	Code      ErrorCode  `json:"code" validate:"required,oneof=bad_request unsupported_protocol payload_too_large busy asr_empty asr_unavailable detector_unavailable upstream_timeout internal"`
	Message   string     `json:"message" validate:"required"`
	Retryable bool       `json:"retryable"`
}

// Millis is a duration in milliseconds that the gateway writes with one decimal place (PROTO-12).
type Millis float64

// DurationMillis converts d to milliseconds rounded to one decimal place.
func DurationMillis(d time.Duration) Millis {
	return Millis(math.Round(float64(d)/float64(time.Millisecond)*10) / 10)
}

// MarshalJSON writes the duration with exactly one decimal place: 583.0, 1.2.
func (m Millis) MarshalJSON() ([]byte, error) {
	value := float64(m)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf(errMillisNotFiniteFormat, value)
	}
	return strconv.AppendFloat(nil, value, 'f', 1, 64), nil
}

// MarshalJSON writes nil objects and timings as an empty array and object, not null.
func (m *AnswerEnd) MarshalJSON() ([]byte, error) {
	type answerEnd AnswerEnd

	message := answerEnd(*m)
	if message.Objects == nil {
		message.Objects = []SceneObject{}
	}
	if message.Timings == nil {
		message.Timings = Timings{}
	}
	return json.Marshal(message)
}

// MessageType returns hello.
func (m *Hello) MessageType() MessageType { return TypeHello }

// MessageType returns describe.
func (m *Describe) MessageType() MessageType { return TypeDescribe }

// MessageType returns ask.start.
func (m *AskStart) MessageType() MessageType { return TypeAskStart }

// MessageType returns ask.end.
func (m *AskEnd) MessageType() MessageType { return TypeAskEnd }

// MessageType returns cancel.
func (m *Cancel) MessageType() MessageType { return TypeCancel }

// MessageType returns client.metrics.
func (m *ClientMetrics) MessageType() MessageType { return TypeClientMetrics }

// MessageType returns welcome.
func (m *Welcome) MessageType() MessageType { return TypeWelcome }

// MessageType returns accepted.
func (m *Accepted) MessageType() MessageType { return TypeAccepted }

// MessageType returns transcript.
func (m *Transcript) MessageType() MessageType { return TypeTranscript }

// MessageType returns answer.start.
func (m *AnswerStart) MessageType() MessageType { return TypeAnswerStart }

// MessageType returns answer.text.
func (m *AnswerText) MessageType() MessageType { return TypeAnswerText }

// MessageType returns answer.end.
func (m *AnswerEnd) MessageType() MessageType { return TypeAnswerEnd }

// MessageType returns passive.clear.
func (m *PassiveClear) MessageType() MessageType { return TypePassiveClear }

// MessageType returns error.
func (m *ErrorMessage) MessageType() MessageType { return TypeError }

// clientMessages create the messages the application sends (PROTO-4).
var clientMessages = map[MessageType]func() Message{
	TypeHello:         func() Message { return &Hello{} },
	TypeDescribe:      func() Message { return &Describe{} },
	TypeAskStart:      func() Message { return &AskStart{} },
	TypeAskEnd:        func() Message { return &AskEnd{} },
	TypeCancel:        func() Message { return &Cancel{} },
	TypeClientMetrics: func() Message { return &ClientMetrics{} },
}

// serverMessages create the messages the gateway sends (PROTO-5).
var serverMessages = map[MessageType]func() Message{
	TypeWelcome:      func() Message { return &Welcome{} },
	TypeAccepted:     func() Message { return &Accepted{} },
	TypeTranscript:   func() Message { return &Transcript{} },
	TypeAnswerStart:  func() Message { return &AnswerStart{} },
	TypeAnswerText:   func() Message { return &AnswerText{} },
	TypeAnswerEnd:    func() Message { return &AnswerEnd{} },
	TypePassiveClear: func() Message { return &PassiveClear{} },
	TypeError:        func() Message { return &ErrorMessage{} },
}

var messageValidator = utils.NewValidator().WithJsonFieldNames()

// EncodeMessage writes the message as JSON with the type field first: {"type":"accepted","request_id":...}.
func EncodeMessage(message Message) ([]byte, error) {
	body, err := json.Marshal(message)
	if err == nil && (len(body) < len("{}") || body[0] != '{') {
		err = errors.New(errNotJSONObject)
	}
	if err != nil {
		return nil, fmt.Errorf(errEncodeMessageFormat, message.MessageType(), err)
	}

	data := make([]byte, 0, len(`{"type":"",`)+len(message.MessageType())+len(body))
	data = append(data, `{"type":"`...)
	data = append(data, message.MessageType()...)
	data = append(data, '"')
	if len(body) > len("{}") {
		data = append(data, ',')
		data = append(data, body[1:]...)
	} else {
		data = append(data, '}')
	}

	return data, nil
}

// DecodeClientMessage parses a text frame of the application (PROTO-4) and checks its fields.
//
// Every failure is a bad_request *Error: not a JSON object, no type, a type the application
// does not send, a missing field or a value out of its enumeration. hello with a protocol
// version other than ProtocolVersion is an unsupported_protocol *Error (PROTO-10).
func DecodeClientMessage(data []byte) (Message, error) {
	return decodeMessage(data, clientMessages)
}

// DecodeServerMessage parses a text frame of the gateway (PROTO-5) and checks its fields
// the same way as DecodeClientMessage. It serves the load generator and the tests.
func DecodeServerMessage(data []byte) (Message, error) {
	return decodeMessage(data, serverMessages)
}

// messageEnvelope is the part of a message that tells its type.
type messageEnvelope struct {
	Type MessageType `json:"type"`
}

func decodeMessage(data []byte, messages map[MessageType]func() Message) (Message, error) {
	var envelope messageEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, NewError(CodeBadRequest, errMessageNotObjectFormat, err)
	}
	if envelope.Type == "" {
		return nil, NewError(CodeBadRequest, errMissingType)
	}

	newMessage, ok := messages[envelope.Type]
	if !ok {
		return nil, NewError(CodeBadRequest, errUnknownTypeFormat, envelope.Type)
	}

	if envelope.Type == TypeHello {
		if err := checkProtocolVersion(data); err != nil {
			return nil, err
		}
	}

	message := newMessage()
	if err := json.Unmarshal(data, message); err != nil {
		return nil, NewError(CodeBadRequest, errInvalidMessageFormat, envelope.Type, err)
	}
	if result := messageValidator.Validate(message); result.Error != nil {
		return nil, NewError(CodeBadRequest, errInvalidMessageFormat, envelope.Type, result.Message)
	}

	return message, nil
}

// helloVersion is the protocol version of hello; nil when the field is missing.
type helloVersion struct {
	Protocol *int `json:"protocol"`
}

// checkProtocolVersion rejects hello of another protocol version before its fields are checked:
// a hello of a newer version may have other fields. A missing version is left to the field check.
func checkProtocolVersion(data []byte) error {
	var version helloVersion
	if err := json.Unmarshal(data, &version); err != nil {
		return NewError(CodeBadRequest, errInvalidMessageFormat, TypeHello, err)
	}
	if version.Protocol != nil && *version.Protocol != ProtocolVersion {
		return NewError(CodeUnsupportedProtocol, errUnsupportedProtocolFormat, *version.Protocol)
	}
	return nil
}
