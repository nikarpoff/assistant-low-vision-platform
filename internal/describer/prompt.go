package describer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
	"github.com/nikarpoff/assistant-low-vision-platform/prompts"
)

// Files of a prompt template.
const (
	systemFile = "system.txt"
	userFile   = "user.tmpl"
)

// hashLength is the number of hex characters of SHA-256 in the hash of the prompt.
const hashLength = 12

// Texts of the errors, shared by the code and the tests.
const (
	errPromptNameFormat  = "invalid APP_PROMPT %q: a name of prompts/ or a path to a directory is required"
	errPromptReadFormat  = "failed to read prompt %q: %w"
	errPromptEmptyFormat = "prompt %q: %s is empty"
	errPromptParseFormat = "failed to parse prompt %q: %w"
	errPromptBuildFormat = "failed to build prompt %q: %w"
)

// Parts of the prompt (DSC-5).
const (
	defaultQuestion  = "Опиши обстановку"
	objectSeparator  = "; "
	objectPartFormat = "%s - %s"
	hazardWord       = "опасность"
	speakerUser      = "Пользователь: "
	speakerAssistant = "Ассистент: "
	maxPromptScenes  = 2
)

// promptNamePattern is a name of an embedded prompt: prompts/<name>/.
var promptNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Prompt is a checked prompt template: the constant system part and the template of the user message.
type Prompt struct {
	name   string
	system string
	user   *template.Template
	hash   string
}

// promptData are the values of the user template.
type promptData struct {
	// Objects are the objects of the current scene in the compact form; empty - none.
	Objects string
	// FocusAhead is true for focus=ahead.
	FocusAhead bool
	// Scenes are the summaries of up to two previous scenes, the oldest first.
	Scenes []string
	// Turns are the turns of the dialogue: «Пользователь: ...», «Ассистент: ...».
	Turns []string
	// Question is the question or «Опиши обстановку».
	Question string
	// Image is true when the message carries the snapshot (vlm).
	Image bool
}

// LoadPrompt loads the prompt APP_PROMPT: a name is a directory of the embedded prompts/,
// a value with a path separator is a directory on disk. Both hold system.txt and user.tmpl.
func LoadPrompt(name string) (*Prompt, error) {
	var files fs.FS
	switch {
	case strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator):
		files = os.DirFS(name)
	case promptNamePattern.MatchString(name):
		sub, err := fs.Sub(prompts.FS, name)
		if err != nil {
			return nil, fmt.Errorf(errPromptReadFormat, name, err)
		}
		files = sub
	default:
		return nil, fmt.Errorf(errPromptNameFormat, name)
	}

	system, err := fs.ReadFile(files, systemFile)
	if err != nil {
		return nil, fmt.Errorf(errPromptReadFormat, name, err)
	}
	user, err := fs.ReadFile(files, userFile)
	if err != nil {
		return nil, fmt.Errorf(errPromptReadFormat, name, err)
	}
	return NewPrompt(name, system, user)
}

// NewPrompt checks and parses the files of a prompt. The hash is the first 12 hex characters
// of SHA-256 of both files; it goes into config_id (PLT-12).
func NewPrompt(name string, system, user []byte) (*Prompt, error) {
	if strings.TrimSpace(string(system)) == "" {
		return nil, fmt.Errorf(errPromptEmptyFormat, name, systemFile)
	}
	if strings.TrimSpace(string(user)) == "" {
		return nil, fmt.Errorf(errPromptEmptyFormat, name, userFile)
	}
	parsed, err := template.New(userFile).Option("missingkey=error").Parse(string(user))
	if err != nil {
		return nil, fmt.Errorf(errPromptParseFormat, name, err)
	}

	p := &Prompt{
		name:   name,
		system: strings.TrimSpace(string(system)),
		user:   parsed,
		hash:   promptHash(system, user),
	}
	// A template with an unknown field fails at start, not on the first request.
	if _, err := p.User(&promptData{Question: defaultQuestion}); err != nil {
		return nil, err
	}
	return p, nil
}

// Name returns APP_PROMPT the prompt was loaded by.
func (p *Prompt) Name() string {
	return p.name
}

// Hash returns the hash of the prompt files for snapshot.Hashes.Prompt.
func (p *Prompt) Hash() string {
	return p.hash
}

// System returns the system part: it is constant and goes first, so the provider can reuse the prefix cache.
func (p *Prompt) System() string {
	return p.system
}

// User renders the user message of the prompt.
func (p *Prompt) User(data *promptData) (string, error) {
	var buffer bytes.Buffer
	if err := p.user.Execute(&buffer, data); err != nil {
		return "", fmt.Errorf(errPromptBuildFormat, p.name, err)
	}
	return strings.TrimSpace(buffer.String()), nil
}

// promptHash hashes the files with their names, so moving text between them changes the hash.
func promptHash(system, user []byte) string {
	hash := sha256.New()
	for _, part := range [][]byte{[]byte(systemFile), {0}, system, {0}, []byte(userFile), {0}, user} {
		hash.Write(part)
	}
	return hex.EncodeToString(hash.Sum(nil))[:hashLength]
}

// newPromptData collects the values of the user template from the input (DSC-5).
// Only text reaches the template: no identifiers of the device or the request (NFR-8).
func newPromptData(in *Input, sc *scene, vocab *vocabulary.Vocabulary, policy *hazard.Policy, image bool) *promptData {
	data := &promptData{
		Objects:    compactScene(sc, vocab),
		FocusAhead: in.Focus == protocol.FocusAhead,
		Scenes:     []string{},
		Turns:      []string{},
		Question:   strings.TrimSpace(in.Question),
		Image:      image,
	}
	if data.Question == "" {
		data.Question = defaultQuestion
	}

	scenes := in.History.Scenes
	if len(scenes) > maxPromptScenes {
		scenes = scenes[len(scenes)-maxPromptScenes:]
	}
	for _, previous := range scenes {
		summary := strings.TrimSpace(previous.Description)
		if summary == "" {
			previousScene := prepareScene(previous.Objects, protocol.FocusAll, policy)
			summary = compactScene(&previousScene, vocab)
		}
		if summary != "" {
			data.Scenes = append(data.Scenes, summary)
		}
	}

	for _, turn := range in.History.Turns {
		text := strings.TrimSpace(turn.Text)
		if text == "" {
			continue
		}
		speaker := speakerUser
		if turn.Role == history.RoleAnswer {
			speaker = speakerAssistant
		}
		data.Turns = append(data.Turns, speaker+text)
	}
	return data
}

// compactScene writes the objects in the compact Russian form of DSC-5:
// «ступени - впереди, близко, опасность; стул - слева, далеко».
func compactScene(sc *scene, vocab *vocabulary.Vocabulary) string {
	parts := make([]string, 0, len(sc.objects))
	for i, object := range sc.objects {
		place := []string{directionWords[object.Direction], distanceWords[object.Distance]}
		if sc.danger(i) {
			place = append(place, hazardWord)
		}
		parts = append(parts, fmt.Sprintf(objectPartFormat, objectName(object, vocab), strings.Join(place, listSeparator)))
	}
	return strings.Join(parts, objectSeparator)
}

// objectName is the Russian name of the class from the vocabulary, label_ru of the detector for an unknown class.
func objectName(object detector.Object, vocab *vocabulary.Vocabulary) string {
	if class, ok := vocab.Class(object.Label); ok {
		return class.RU.One
	}
	if object.LabelRU != "" {
		return object.LabelRU
	}
	return object.Label
}
