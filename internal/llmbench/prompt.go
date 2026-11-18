package llmbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"unicode/utf8"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// DefaultPromptPath is the reference prompt relative to the working directory:
// the repository root or "/" of the gateway image with testdata/llmbench mounted.
const DefaultPromptPath = "testdata/llmbench/prompt.json"

// MaxPromptTokens is the limit of the reference prompt (PLT-19, LLM-4).
const MaxPromptTokens = 600

// charsPerToken is a conservative size of a token of Russian text for Qwen and
// GigaChat: EstimateTokens overestimates rather than underestimates. The real
// count comes from the provider as prompt_tokens of the report.
const charsPerToken = 2.5

// Settings of the builder of the reference prompt.
const (
	builderModel     = "llmbench"
	builderRequestID = "llmbench-prompt"
	jpegDataURL      = "data:image/jpeg;base64,"
)

// builderImage is a placeholder JPEG: the builder takes only the text of the vlm message.
var builderImage = []byte{0xFF, 0xD8, 0xFF, 0xD9}

// Texts of the errors, shared by the code and the tests.
const (
	errPromptReadFormat     = "failed to read reference prompt %q: %w"
	errPromptParseFormat    = "failed to parse reference prompt %q: %w"
	errPromptMessagesFormat = "reference prompt %q: messages and image_messages must end with a user message"
	errBuildPromptFormat    = "failed to build reference prompt: %w"
	errEncodePromptFormat   = "failed to encode reference prompt: %w"
	errNoRequestFormat      = "describer sent no request in the %s mode"
	errContentFormat        = "describer sent an unexpected message content in the %s mode"
	errNoUserMessage        = "describer prompt does not end with a user message"
)

// PromptMessage is a text message of the reference prompt.
type PromptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// PromptFile is testdata/llmbench/prompt.json: the prompt of the describer for a
// typical scene (DSC-5). Messages is the llm mode; ImageMessages is the vlm mode,
// to whose last (user) message the snapshot of --image is attached.
type PromptFile struct {
	// Prompt is APP_PROMPT the file was built from, PromptHash - its hash (PLT-12).
	Prompt        string          `json:"prompt"`
	PromptHash    string          `json:"prompt_hash"`
	Messages      []PromptMessage `json:"messages"`
	ImageMessages []PromptMessage `json:"image_messages"`
}

// LoadPromptFile reads and checks the reference prompt.
func LoadPromptFile(path string) (*PromptFile, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(errPromptReadFormat, path, err)
	}
	file := &PromptFile{}
	if err := json.Unmarshal(body, file); err != nil {
		return nil, fmt.Errorf(errPromptParseFormat, path, err)
	}
	if !endsWithUser(file.Messages) || !endsWithUser(file.ImageMessages) {
		return nil, fmt.Errorf(errPromptMessagesFormat, path)
	}
	return file, nil
}

// Request returns the messages of a request: the llm prompt as text or, with an
// image, the vlm prompt whose user message carries the snapshot as image_url
// with a data URL, as the describer sends it in the vlm mode (DSC-6).
func (f *PromptFile) Request(image []byte) []llm.Message {
	source := f.Messages
	if len(image) > 0 {
		source = f.ImageMessages
	}

	messages := make([]llm.Message, len(source))
	for i, message := range source {
		messages[i] = llm.Message{Role: message.Role, Content: message.Content}
	}
	if len(image) > 0 {
		last := len(messages) - 1
		messages[last].Content = []llm.ContentPart{
			{Type: llm.ContentPartText, Text: source[last].Content},
			{Type: llm.ContentPartImageURL, ImageURL: &llm.ImageURL{URL: jpegDataURL + base64.StdEncoding.EncodeToString(image)}},
		}
	}
	return messages
}

// EstimateTokens estimates the size of the llm prompt in tokens from above.
func (f *PromptFile) EstimateTokens() int {
	runes := 0
	for _, message := range f.Messages {
		runes += utf8.RuneCountInString(message.Content)
	}
	return int(float64(runes)/charsPerToken + 0.999)
}

// Encode writes the file as it is kept in git: indented, with a newline at the end.
func (f *PromptFile) Encode() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(f); err != nil {
		return nil, fmt.Errorf(errEncodePromptFormat, err)
	}
	return buffer.Bytes(), nil
}

// TypicalScene is the typical scene of the reference prompt: a street crossing
// with a pedestrian, a car, a traffic light and a pole.
func TypicalScene() []detector.Object {
	return []detector.Object{
		{Label: "crosswalk", Direction: detector.DirectionAhead, Distance: detector.DistanceNear, Confidence: 0.81, Area: 0.22},
		{Label: "person", Direction: detector.DirectionLeft, Distance: detector.DistanceNear, Confidence: 0.77, Area: 0.09},
		{Label: "car", Direction: detector.DirectionRight, Distance: detector.DistanceFar, Confidence: 0.72, Area: 0.05},
		{Label: "traffic_light", Direction: detector.DirectionAhead, Distance: detector.DistanceFar, Confidence: 0.64, Area: 0.01},
		{Label: "pole", Direction: detector.DirectionLeft, Distance: detector.DistanceNear, Confidence: 0.58, Area: 0.03},
	}
}

// BuildPromptFile builds the reference prompt by the prompt builder of the
// describer (DSC-5): the describe request of the typical scene in the llm mode
// and in the vlm mode with the objects as hints. Nothing is sent anywhere.
func BuildPromptFile(prompt *describer.Prompt, vocab *vocabulary.Vocabulary, policy *hazard.Policy) (*PromptFile, error) {
	file := &PromptFile{Prompt: prompt.Name(), PromptHash: prompt.Hash()}

	var err error
	if file.Messages, err = buildMessages(env.DescriberModeLLM, prompt, vocab, policy, nil); err != nil {
		return nil, fmt.Errorf(errBuildPromptFormat, err)
	}
	if file.ImageMessages, err = buildMessages(env.DescriberModeVLM, prompt, vocab, policy, builderImage); err != nil {
		return nil, fmt.Errorf(errBuildPromptFormat, err)
	}
	return file, nil
}

// buildMessages runs the describer of the mode with a recorder instead of the provider.
func buildMessages(mode env.DescriberMode, prompt *describer.Prompt, vocab *vocabulary.Vocabulary, policy *hazard.Policy, image []byte) ([]PromptMessage, error) {
	recorder := &requestRecorder{}
	d, err := describer.NewDescriber(describer.Config{Mode: mode, Model: builderModel, WithDetections: true}, vocab, policy, prompt, recorder)
	if err != nil {
		return nil, err
	}
	in := &describer.Input{RequestID: builderRequestID, Objects: TypicalScene(), Focus: protocol.FocusAll, Image: image}
	if _, err := d.Describe(context.Background(), in, nil); err != nil {
		return nil, err
	}
	if recorder.request == nil {
		return nil, fmt.Errorf(errNoRequestFormat, mode)
	}

	messages := make([]PromptMessage, 0, len(recorder.request.Messages))
	for _, message := range recorder.request.Messages {
		text, ok := messageText(message.Content)
		if !ok {
			return nil, fmt.Errorf(errContentFormat, mode)
		}
		messages = append(messages, PromptMessage{Role: message.Role, Content: text})
	}
	if !endsWithUser(messages) {
		return nil, errors.New(errNoUserMessage)
	}
	return messages, nil
}

// messageText is the text of a message: the string or the text part of a multimodal content.
func messageText(content any) (string, bool) {
	switch value := content.(type) {
	case string:
		return value, true
	case []llm.ContentPart:
		for _, part := range value {
			if part.Type == llm.ContentPartText {
				return part.Text, true
			}
		}
	}
	return "", false
}

// endsWithUser reports whether the messages end with a user message.
func endsWithUser(messages []PromptMessage) bool {
	return len(messages) > 0 && messages[len(messages)-1].Role == llm.RoleUser
}

// requestRecorder takes the place of the provider for the builder and keeps the request.
type requestRecorder struct {
	request *llm.Request
}

// Complete keeps the request and answers with an empty result.
func (r *requestRecorder) Complete(_ context.Context, req *llm.Request, _ llm.TextHandler) (*llm.Result, error) {
	r.request = req
	return &llm.Result{}, nil
}
