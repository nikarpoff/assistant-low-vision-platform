package describer

import (
	"context"
	"encoding/base64"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
)

// jpegDataURLPrefix is the prefix of the data URL of a JPEG snapshot.
const jpegDataURLPrefix = "data:image/jpeg;base64,"

// describeVLM describes the scene by the multimodal model from the snapshot (DSC-6): the
// snapshot goes in the user message as image_url with a data URL, the objects of the
// detector are hints with APP_VLM_WITH_DETECTIONS. Without the detector (nil objects) the
// description is built from the snapshot alone; without a snapshot - from the text alone.
func (d *Describer) describeVLM(ctx context.Context, in *Input, onText llm.TextHandler) (*Result, error) {
	sc := prepareScene(in.Objects, in.Focus, d.template.policy)
	hints := sc
	if !d.withDetections {
		hints = scene{}
	}
	image := len(in.Image) > 0
	user, err := d.prompt.User(newPromptData(in, &hints, d.template.vocab, d.template.policy, image))
	if err != nil {
		return nil, err
	}

	var content any = user
	if image {
		content = []llm.ContentPart{
			{Type: llm.ContentPartText, Text: user},
			{Type: llm.ContentPartImageURL, ImageURL: &llm.ImageURL{URL: jpegDataURL(in.Image)}},
		}
	}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: d.prompt.System()},
		{Role: llm.RoleUser, Content: content},
	}
	return d.complete(ctx, env.DescriberModeVLM, in.RequestID, messages, sc.hasHazard(), onText)
}

// jpegDataURL encodes the JPEG snapshot as a data URL.
func jpegDataURL(image []byte) string {
	return jpegDataURLPrefix + base64.StdEncoding.EncodeToString(image)
}
