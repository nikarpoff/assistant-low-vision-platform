// Package describer builds the description of a scene (DSC): by a deterministic
// template without a language model (template), by a language model from the
// objects of the detector (llm) or from the snapshot itself (vlm).
//
// A describer gives a stream of text; the pipeline cuts it into sentences
// (PLT-7), cleans it (DSC-7) and limits the answer. The template description is
// also the fallback of the llm and vlm modes (PLT-9).
package describer

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Texts of the errors, shared by the code and the tests.
const (
	errUnknownModeFormat = "unknown describer mode %q: allowed values are template, llm, vlm"
	errNoLLMClient       = "language model client is required in the llm and vlm modes"
	errNoPrompt          = "prompt is required in the llm and vlm modes"
	errNoModelFormat     = "language model identifier is required in the %s mode"
	errHandleTextFormat  = "failed to handle template text: %w"
)

//go:generate moq -out completer_moq_test.go . Completer

// Completer sends a chat completion request to the language model provider (*llm.Client).
type Completer interface {
	Complete(ctx context.Context, req *llm.Request, onText llm.TextHandler) (*llm.Result, error)
}

// Input is what a scene is described from (DSC-2). It has no identifiers of the
// device or the participant: nothing of it but the text and, in the vlm mode,
// the snapshot reaches the provider (NFR-8).
type Input struct {
	// RequestID identifies the request in the logs; it is never sent to the provider.
	RequestID string
	// Objects are the objects of the detector in any order; nil if the detector did not answer (vlm).
	Objects []detector.Object
	// Focus ahead leaves only the objects with direction ahead.
	Focus protocol.Focus
	// Question is the recognized question; empty - describe the scene.
	Question string
	// History is the history of the device without the current request.
	History history.History
	// Image is the JPEG snapshot; it is sent only in the vlm mode.
	Image []byte
}

// Result is the description given to the pipeline.
type Result struct {
	// Mode is the mode that built the description.
	Mode env.DescriberMode
	// Text is the whole text passed to the TextHandler.
	Text string
	// Hazard is true when the scene in focus has an object of a danger rule.
	Hazard bool
	// LLM is the result of the provider in the llm and vlm modes, nil in the template mode.
	LLM *llm.Result
}

// Config holds the settings of the describer.
type Config struct {
	// Mode is APP_DESCRIBER_MODE.
	Mode env.DescriberMode
	// Model is the model of the active mode: APP_LLM_MODEL, in the vlm mode APP_VLM_MODEL or, if it is empty, APP_LLM_MODEL.
	Model string
	// WithDetections adds the objects of the detector to the vlm prompt (APP_VLM_WITH_DETECTIONS).
	WithDetections bool
}

// NewConfig takes the describer settings from the gateway configuration.
func NewConfig(vars *env.Vars) Config {
	model := vars.AppLLMModel
	if vars.AppDescriberMode == env.DescriberModeVLM && vars.AppVLMModel != "" {
		model = vars.AppVLMModel
	}
	return Config{
		Mode:           vars.AppDescriberMode,
		Model:          model,
		WithDetections: vars.AppVLMWithDetections,
	}
}

// Describer describes scenes in the configured mode. It has no state and is shared by all sessions.
type Describer struct {
	mode           env.DescriberMode
	model          string
	withDetections bool
	template       *Template
	prompt         *Prompt
	client         Completer
}

// NewDescriber creates the describer of the mode. The template describer is always
// created: it is the fallback of the llm and vlm modes. The client and the prompt are
// required in the llm and vlm modes and not used in the template mode.
func NewDescriber(cfg Config, vocab *vocabulary.Vocabulary, policy *hazard.Policy, prompt *Prompt, client Completer) (*Describer, error) {
	d := &Describer{
		mode:           cfg.Mode,
		model:          cfg.Model,
		withDetections: cfg.WithDetections,
		template:       NewTemplate(vocab, policy),
		prompt:         prompt,
		client:         client,
	}
	switch cfg.Mode {
	case env.DescriberModeTemplate:
		return d, nil
	case env.DescriberModeLLM, env.DescriberModeVLM:
		if client == nil {
			return nil, errors.New(errNoLLMClient)
		}
		if prompt == nil {
			return nil, errors.New(errNoPrompt)
		}
		if cfg.Model == "" {
			return nil, fmt.Errorf(errNoModelFormat, cfg.Mode)
		}
		return d, nil
	default:
		return nil, fmt.Errorf(errUnknownModeFormat, cfg.Mode)
	}
}

// Mode returns the describer mode.
func (d *Describer) Mode() env.DescriberMode {
	return d.mode
}

// Describe describes the scene and passes the text to onText as it comes. In the
// llm and vlm modes a failure of the provider is the error of llm.Client
// (ErrTimeout, ErrUnavailable, ErrCanceled) with what came before it in Result.LLM;
// the pipeline decides on the fallback (PLT-9) and calls Template.
func (d *Describer) Describe(ctx context.Context, in *Input, onText llm.TextHandler) (*Result, error) {
	switch d.mode {
	case env.DescriberModeLLM:
		return d.describeLLM(ctx, in, onText)
	case env.DescriberModeVLM:
		return d.describeVLM(ctx, in, onText)
	default:
		return d.Template(in, onText)
	}
}

// Template describes the scene by the template (DSC-4) in any mode: the template
// mode and the fallback of the llm and vlm modes (PLT-9). The whole text goes to
// onText at once; llm.ErrStop from it is not an error.
func (d *Describer) Template(in *Input, onText llm.TextHandler) (*Result, error) {
	text, hazard := d.template.Describe(in)
	result := &Result{Mode: env.DescriberModeTemplate, Text: text, Hazard: hazard}
	if onText != nil {
		if err := onText(text); err != nil && !errors.Is(err, llm.ErrStop) {
			return result, fmt.Errorf(errHandleTextFormat, err)
		}
	}
	return result, nil
}

// scene is the sorted objects in focus and their hazard matches.
type scene struct {
	objects []detector.Object
	matches map[int]hazard.Match // index in objects -> match of a danger or notice rule
}

// prepareScene leaves the objects in focus and sorts them as DSC-2 orders them:
// hazards (level danger) first, then near before far, then by decreasing area.
func prepareScene(objects []detector.Object, focus protocol.Focus, policy *hazard.Policy) scene {
	kept := make([]detector.Object, 0, len(objects))
	for _, object := range objects {
		if focus == protocol.FocusAhead && object.Direction != detector.DirectionAhead {
			continue
		}
		kept = append(kept, object)
	}

	type ranked struct {
		object  detector.Object
		match   hazard.Match
		matched bool
	}
	items := make([]ranked, len(kept))
	for i, object := range kept {
		match, ok := policy.Match(object)
		items[i] = ranked{object: object, match: match, matched: ok}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		aDanger := a.matched && a.match.Level == hazard.LevelDanger
		bDanger := b.matched && b.match.Level == hazard.LevelDanger
		if aDanger != bDanger {
			return aDanger
		}
		if a.object.Distance != b.object.Distance {
			return a.object.Distance == detector.DistanceNear
		}
		return a.object.Area > b.object.Area
	})

	result := scene{objects: make([]detector.Object, len(items)), matches: map[int]hazard.Match{}}
	for i, item := range items {
		result.objects[i] = item.object
		if item.matched {
			result.matches[i] = item.match
		}
	}
	return result
}

// danger reports whether the object at index i falls under a danger rule.
func (s *scene) danger(i int) bool {
	match, ok := s.matches[i]
	return ok && match.Level == hazard.LevelDanger
}

// hasHazard reports whether the scene has an object of a danger rule.
func (s *scene) hasHazard() bool {
	for i := range s.objects {
		if s.danger(i) {
			return true
		}
	}
	return false
}
