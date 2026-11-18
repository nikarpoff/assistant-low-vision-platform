package llmbench

import (
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/describer"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// updatePrompt rewrites testdata/llmbench/prompt.json from the prompt builder:
// go test ./internal/llmbench -run '^TestPromptFile$' -update
var updatePrompt = flag.Bool("update", false, "rewrite testdata/llmbench/prompt.json")

// Inputs of the prompt builder, as the gateway takes them by default.
const (
	testVocabularyPath = "../../testdata/contracts/objects-detector/vocabulary.response.json"
	testPromptName     = "default"
)

func TestPromptFile(t *testing.T) {
	var (
		vocab  *vocabulary.Vocabulary
		policy *hazard.Policy
		prompt *describer.Prompt
	)

	setup := func() {
		body, err := os.ReadFile(testVocabularyPath)
		require.NoError(t, err)
		document := &vocabulary.Document{}
		require.NoError(t, json.Unmarshal(body, document))
		vocab, err = vocabulary.NewVocabulary(document)
		require.NoError(t, err)

		rules, err := hazard.LoadRules("")
		require.NoError(t, err)
		policy, err = hazard.NewPolicy(rules, vocab)
		require.NoError(t, err)

		prompt, err = describer.LoadPrompt(testPromptName)
		require.NoError(t, err)
	}

	t.Run("must_pass_built_by_describer", func(t *testing.T) {
		setup()

		built, err := BuildPromptFile(prompt, vocab, policy)
		require.NoError(t, err)
		encoded, err := built.Encode()
		require.NoError(t, err)
		if *updatePrompt {
			require.NoError(t, os.WriteFile(testPromptPath, encoded, 0o644))
		}

		// The file in git is exactly what the builder gives for the current prompt
		kept, err := os.ReadFile(testPromptPath)
		require.NoError(t, err)
		require.Equal(t, string(encoded), string(kept), "run: go test ./internal/llmbench -run '^TestPromptFile$' -update")

		loaded, err := LoadPromptFile(testPromptPath)
		require.NoError(t, err)
		require.Equal(t, testPromptName, loaded.Prompt)
		require.Equal(t, prompt.Hash(), loaded.PromptHash)
		require.Equal(t, []PromptMessage{
			{Role: llm.RoleSystem, Content: prompt.System()},
			loaded.Messages[1],
		}, loaded.Messages)
		require.Contains(t, loaded.Messages[1].Content, "Объекты текущей сцены: ")
		require.Contains(t, loaded.ImageMessages[1].Content, "К сообщению приложен снимок камеры.")
		require.Contains(t, loaded.ImageMessages[1].Content, "Подсказки детектора: ")
	})

	t.Run("must_pass_token_limit", func(t *testing.T) {
		setup()

		loaded, err := LoadPromptFile(testPromptPath)
		require.NoError(t, err)
		require.LessOrEqual(t, loaded.EstimateTokens(), MaxPromptTokens)
	})

	t.Run("must_fail_invalid_file", func(t *testing.T) {
		setup()

		_, err := LoadPromptFile("missing.json")
		require.EqualError(t, err, `failed to read reference prompt "missing.json": open missing.json: no such file or directory`)

		path := t.TempDir() + "/prompt.json"
		require.NoError(t, os.WriteFile(path, []byte(`{"messages":[{"role":"system","content":"x"}],"image_messages":[]}`), 0o600))
		_, err = LoadPromptFile(path)
		require.EqualError(t, err, "reference prompt \""+path+"\": messages and image_messages must end with a user message")

		require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
		_, err = LoadPromptFile(path)
		require.EqualError(t, err, "failed to parse reference prompt \""+path+"\": unexpected end of JSON input")
	})
}
