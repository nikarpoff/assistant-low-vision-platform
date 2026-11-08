package describer

import (
	"context"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
)

// describeLLM describes the scene by the language model from the objects of the detector (DSC-5).
// The provider gets only text: the snapshot and the identifiers stay in the gateway (NFR-8).
func (d *Describer) describeLLM(ctx context.Context, in *Input, onText llm.TextHandler) (*Result, error) {
	sc := prepareScene(in.Objects, in.Focus, d.template.policy)
	user, err := d.prompt.User(newPromptData(in, &sc, d.template.vocab, d.template.policy, false))
	if err != nil {
		return nil, err
	}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: d.prompt.System()},
		{Role: llm.RoleUser, Content: user},
	}
	return d.complete(ctx, env.DescriberModeLLM, in.RequestID, messages, sc.hasHazard(), onText)
}

// complete sends the prompt to the provider and wraps its result.
func (d *Describer) complete(ctx context.Context, mode env.DescriberMode, requestID string, messages []llm.Message, hazard bool, onText llm.TextHandler) (*Result, error) {
	if onText == nil {
		onText = func(string) error { return nil }
	}
	request := &llm.Request{RequestID: requestID, Model: d.model, Messages: messages}
	completion, err := d.client.Complete(ctx, request, onText)
	result := &Result{Mode: mode, Hazard: hazard, LLM: completion}
	if completion != nil {
		result.Text = completion.Text
	}
	return result, err
}
