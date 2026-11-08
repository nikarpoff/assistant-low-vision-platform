// Package intent recognizes the commands in the recognized question (PLT-4).
//
// The text is normalized by textnorm and compared with the phrases of the
// intents file (configs/intents.yaml or APP_INTENTS_PATH), also with
// «пожалуйста» at the start or at the end. Any other non-empty text is a
// question. New phrases are added to the file without changing the code.
package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nikarpoff/assistant-low-vision-platform/configs"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/textnorm"
)

// Texts of the errors, shared by the code and the tests.
const (
	errEmptyText             = "empty text"
	errReadFormat            = "failed to read intents file: %w"
	errParseFormat           = "failed to parse intents: %w"
	errNoPhrases             = "failed to parse intents: no phrases"
	errUnknownIntentFormat   = "failed to parse intents: unknown intent %q: allowed values are describe, describe_ahead, repeat, stop, passive_on, passive_off"
	errEmptyPhraseFormat     = "failed to parse intents: intent %q: phrase %q is empty after normalization"
	errDuplicatePhraseFormat = "failed to parse intents: phrase %q belongs to intents %q and %q"
)

// ErrEmptyText is returned for a text that is empty after normalization: the
// pipeline answers it with asr_empty.
var ErrEmptyText = errors.New(errEmptyText)

// please is the polite word allowed at the start or at the end of a command.
const please = "пожалуйста"

// hashLength is the number of hex characters of SHA-256 in the hash of the file.
const hashLength = 12

// commands are the intents the file may give phrases to; question is the fallback.
var commands = map[protocol.Intent]bool{
	protocol.IntentDescribe:      true,
	protocol.IntentDescribeAhead: true,
	protocol.IntentRepeat:        true,
	protocol.IntentStop:          true,
	protocol.IntentPassiveOn:     true,
	protocol.IntentPassiveOff:    true,
}

// Matcher recognizes the intent of a text by the phrases of the commands.
type Matcher struct {
	phrases map[string]protocol.Intent // normalized phrase -> intent
	hash    string
}

// LoadMatcher reads the intents file at path; an empty path gives the embedded configs/intents.yaml.
func LoadMatcher(path string) (*Matcher, error) {
	if path == "" {
		return NewMatcher(configs.Intents)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(errReadFormat, err)
	}
	return NewMatcher(data)
}

// NewMatcher parses the YAML of the intents file: a mapping of the intent to the list of its phrases.
// The phrases are normalized; a phrase empty after normalization, a phrase of two intents,
// an unknown intent and a file without phrases are errors.
func NewMatcher(data []byte) (*Matcher, error) {
	document := map[string][]string{}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf(errParseFormat, err)
	}

	names := make([]string, 0, len(document))
	for name := range document {
		names = append(names, name)
	}
	sort.Strings(names)

	m := &Matcher{
		phrases: map[string]protocol.Intent{},
		hash:    fileHash(data),
	}
	for _, name := range names {
		intent := protocol.Intent(name)
		if !commands[intent] {
			return nil, fmt.Errorf(errUnknownIntentFormat, name)
		}
		for _, phrase := range document[name] {
			normalized := textnorm.Normalize(phrase)
			if normalized == "" {
				return nil, fmt.Errorf(errEmptyPhraseFormat, name, phrase)
			}
			if other, ok := m.phrases[normalized]; ok && other != intent {
				return nil, fmt.Errorf(errDuplicatePhraseFormat, normalized, other, intent)
			}
			m.phrases[normalized] = intent
		}
	}
	if len(m.phrases) == 0 {
		return nil, errors.New(errNoPhrases)
	}

	return m, nil
}

// Hash returns the first 12 hex characters of SHA-256 of the intents file: it goes into config_id (PLT-12).
func (m *Matcher) Hash() string {
	return m.hash
}

// Recognize returns the intent of the text: the command whose phrase equals the normalized text,
// also with «пожалуйста» at the start or at the end, or question for any other text.
// A text empty after normalization gives ErrEmptyText.
func (m *Matcher) Recognize(text string) (protocol.Intent, error) {
	normalized := textnorm.Normalize(text)
	if normalized == "" {
		return "", ErrEmptyText
	}
	if intent, ok := m.phrases[normalized]; ok {
		return intent, nil
	}

	command := strings.TrimPrefix(normalized, please+" ")
	command = strings.TrimSuffix(command, " "+please)
	if intent, ok := m.phrases[command]; ok {
		return intent, nil
	}

	return protocol.IntentQuestion, nil
}

// fileHash returns the first 12 hex characters of SHA-256 of the file.
func fileHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:hashLength]
}
