// Package prompts embeds the prompt templates of the llm and vlm modes (DSC-5):
// prompts/<name>/system.txt is the constant system part, prompts/<name>/user.tmpl
// is the text/template of the user message. APP_PROMPT selects the name.
package prompts

import (
	"embed"
)

// FS holds the embedded prompt templates, one directory per name.
//
//go:embed */system.txt */user.tmpl
var FS embed.FS
