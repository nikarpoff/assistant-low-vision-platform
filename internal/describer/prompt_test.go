package describer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// Paths of the default prompt files.
const (
	systemPath = "../../prompts/default/system.txt"
	userPath   = "../../prompts/default/user.tmpl"
)

func TestPrompt(t *testing.T) {
	var (
		vars      *env.Vars
		vocab     *vocabulary.Vocabulary
		policy    *hazard.Policy
		prompt    *Prompt
		completer *CompleterMock
		describer *Describer
		input     *Input
	)

	setup := func(t *testing.T) {
		logger.InitTestLogger()
		vars = loadVars(t, env.DescriberModeLLM)
		vocab = readVocabulary(t)
		policy = readPolicy(t, vocab)
		var err error
		prompt, err = LoadPrompt("default")
		require.NoError(t, err)
		completer = &CompleterMock{
			CompleteFunc: func(ctx context.Context, req *llm.Request, onText llm.TextHandler) (*llm.Result, error) {
				return &llm.Result{}, nil
			},
		}
		describer, err = NewDescriber(NewConfig(vars), vocab, policy, prompt, completer)
		require.NoError(t, err)
		input = &Input{
			RequestID: testRequestID,
			Objects: []detector.Object{
				object("chair", detector.DirectionLeft, detector.DistanceFar, 0.06),
				object("stairs", detector.DirectionAhead, detector.DistanceNear, 0.2),
			},
			Focus: protocol.FocusAll,
		}
	}

	// messages describes the input and returns the messages of the request.
	messages := func(t *testing.T) []llm.Message {
		t.Helper()
		_, err := describer.Describe(context.Background(), input, nil)
		require.NoError(t, err)
		calls := completer.CompleteCalls()
		require.NotEmpty(t, calls)
		return calls[len(calls)-1].Req.Messages
	}

	// userText returns the user message of the request.
	userText := func(t *testing.T) string {
		t.Helper()
		result := messages(t)
		require.Len(t, result, 2)
		require.Equal(t, llm.RoleUser, result[1].Role)
		return result[1].Content.(string)
	}

	t.Run("must_pass_system_first_and_constant", func(t *testing.T) {
		setup(t)
		system, err := os.ReadFile(systemPath)
		require.NoError(t, err)

		first := messages(t)
		input.Objects = roomObjects()
		input.Focus = protocol.FocusAhead
		input.Question = "Где-то есть дверь?"
		input.History = deviceHistory(t)
		second := messages(t)

		for _, result := range [][]llm.Message{first, second} {
			require.Len(t, result, 2)
			require.Equal(t, llm.RoleSystem, result[0].Role)
			require.Equal(t, strings.TrimSpace(string(system)), result[0].Content)
		}
		require.Equal(t, first[0], second[0])
		require.NotEqual(t, first[1], second[1])
	})

	t.Run("must_pass_system_rules", func(t *testing.T) {
		setup(t)

		system := prompt.System()

		// DSC-3 rules and the DSC-8 dialogue
		for _, rule := range []string{
			"по-русски, от одного до трёх коротких предложений",
			"Опасности называй первыми, со словом «Осторожно»",
			"«слева», «впереди», «справа»",
			"«близко» и «далеко»",
			"Не используй разметку, списки, эмодзи и цифры: числа пиши словами.",
			"«Двери не вижу.»",
			"«Ничего примечательного не вижу. Попробуйте повернуть телефон.»",
			"Ассистент: Вы в небольшой комнате. Слева стул и шкаф, впереди кровать.",
			"Пользователь: В комнате есть люди?\nАссистент: Людей не вижу.",
			"Пользователь: Где-то есть дверь?\nАссистент: Да, дверь справа, далеко.",
		} {
			require.Contains(t, system, rule)
		}
	})

	t.Run("must_pass_objects", func(t *testing.T) {
		setup(t)

		text := userText(t)

		require.Equal(t, "Объекты текущей сцены: ступени - впереди, близко, опасность; стул - слева, далеко.\nВопрос: Опиши обстановку", text)
	})

	t.Run("must_pass_no_objects", func(t *testing.T) {
		setup(t)
		input.Objects = nil

		text := userText(t)

		require.Equal(t, "Объекты текущей сцены: ничего не найдено.\nВопрос: Опиши обстановку", text)
	})

	t.Run("must_pass_focus", func(t *testing.T) {
		setup(t)
		input.Focus = protocol.FocusAhead

		text := userText(t)

		require.Equal(t, "Объекты текущей сцены: ступени - впереди, близко, опасность.\nФокус: только то, что впереди.\nВопрос: Опиши обстановку", text)
	})

	t.Run("must_pass_scenes", func(t *testing.T) {
		setup(t)
		at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		input.History = history.History{Scenes: []history.Scene{
			{Description: "Первая сцена.", At: at},
			{Description: "Впереди кровать, слева шкаф и стул.", At: at.Add(time.Minute)},
			{Objects: []detector.Object{object("door", detector.DirectionRight, detector.DistanceFar, 0.1)}, At: at.Add(2 * time.Minute)},
		}}

		text := userText(t)

		require.NotContains(t, text, "Первая сцена")
		require.True(t, strings.HasPrefix(text, "Предыдущие сцены, от старой к новой:\nВпереди кровать, слева шкаф и стул.\nдверь - справа, далеко\n\nОбъекты текущей сцены:"), text)
	})

	t.Run("must_pass_turns", func(t *testing.T) {
		setup(t)
		input.History = history.History{Turns: []history.Turn{
			{Role: history.RoleQuestion, Text: "В комнате есть люди?"},
			{Role: history.RoleAnswer, Text: "Людей не вижу."},
			{Role: history.RoleQuestion, Text: "  "},
		}}
		input.Question = "Где-то есть дверь?"

		text := userText(t)

		require.Equal(t, "Реплики диалога:\nПользователь: В комнате есть люди?\nАссистент: Людей не вижу.\n\n"+
			"Объекты текущей сцены: ступени - впереди, близко, опасность; стул - слева, далеко.\nВопрос: Где-то есть дверь?", text)
	})

	t.Run("must_pass_default_question", func(t *testing.T) {
		setup(t)
		input.Question = "   "

		text := userText(t)

		require.True(t, strings.HasSuffix(text, "\nВопрос: Опиши обстановку"))
	})

	t.Run("must_pass_no_identifiers", func(t *testing.T) {
		setup(t)
		input.Image = testImage
		input.History = deviceHistory(t)

		result := messages(t)

		for _, message := range result {
			text := message.Content.(string)
			require.NotContains(t, text, testRequestID)
			require.NotContains(t, text, testDeviceID)
			require.NotContains(t, text, "снимок камеры")
		}
	})

	t.Run("must_pass_parameters", func(t *testing.T) {
		setup(t)
		stub := newProvider(t, vars)
		var err error
		describer, err = NewDescriber(NewConfig(vars), vocab, policy, prompt, stub.client)
		require.NoError(t, err)

		_, err = describer.Describe(context.Background(), input, nil)

		require.NoError(t, err)
		recorded := stub.stub.Requests()
		require.Len(t, recorded, 1)
		body := requestBody(t, recorded[0])
		require.Equal(t, testLLMModel, body["model"])
		require.Equal(t, true, body["stream"])
		require.Equal(t, 0.0, body["temperature"])
		require.Equal(t, 42.0, body["seed"])
		require.Equal(t, 96.0, body["max_tokens"])
		require.Equal(t, map[string]any{"enabled": false}, body["reasoning"])
		messages := body["messages"].([]any)
		require.Equal(t, llm.RoleSystem, messages[0].(map[string]any)["role"])
		require.Equal(t, prompt.System(), messages[0].(map[string]any)["content"])
	})

	t.Run("must_pass_hash", func(t *testing.T) {
		setup(t)
		system, err := os.ReadFile(systemPath)
		require.NoError(t, err)
		user, err := os.ReadFile(userPath)
		require.NoError(t, err)
		hash := sha256.New()
		hash.Write([]byte("system.txt\x00"))
		hash.Write(system)
		hash.Write([]byte("\x00user.tmpl\x00"))
		hash.Write(user)

		changed, err := NewPrompt("default", system, append(user, ' '))
		require.NoError(t, err)

		require.Equal(t, "default", prompt.Name())
		require.Equal(t, hex.EncodeToString(hash.Sum(nil))[:12], prompt.Hash())
		require.NotEqual(t, prompt.Hash(), changed.Hash())
	})

	t.Run("must_pass_directory", func(t *testing.T) {
		setup(t)
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "system.txt"), []byte("Отвечай коротко."), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "user.tmpl"), []byte("Вопрос: {{.Question}}\n"), 0o600))

		fromDir, err := LoadPrompt(dir)
		require.NoError(t, err)
		text, err := fromDir.User(&promptData{Question: "Что впереди?"})
		require.NoError(t, err)

		require.Equal(t, dir, fromDir.Name())
		require.Equal(t, "Отвечай коротко.", fromDir.System())
		require.Equal(t, "Вопрос: Что впереди?", text)
	})

	t.Run("must_fail_unknown_prompt", func(t *testing.T) {
		setup(t)

		_, err := LoadPrompt("short")

		require.EqualError(t, err, `failed to read prompt "short": open system.txt: file does not exist`)
	})

	t.Run("must_fail_invalid_name", func(t *testing.T) {
		setup(t)

		_, err := LoadPrompt("..")

		require.EqualError(t, err, `invalid APP_PROMPT "..": a name of prompts/ or a path to a directory is required`)
	})

	t.Run("must_fail_empty_system", func(t *testing.T) {
		setup(t)

		_, err := NewPrompt("empty", []byte(" \n"), []byte("Вопрос: {{.Question}}"))

		require.EqualError(t, err, `prompt "empty": system.txt is empty`)
	})

	t.Run("must_fail_empty_user", func(t *testing.T) {
		setup(t)

		_, err := NewPrompt("empty", []byte("Правила"), nil)

		require.EqualError(t, err, `prompt "empty": user.tmpl is empty`)
	})

	t.Run("must_fail_parse", func(t *testing.T) {
		setup(t)

		_, err := NewPrompt("broken", []byte("Правила"), []byte("Вопрос: {{.Question"))

		require.EqualError(t, err, `failed to parse prompt "broken": template: user.tmpl:1: unclosed action`)
	})

	t.Run("must_fail_unknown_field", func(t *testing.T) {
		setup(t)

		_, err := NewPrompt("broken", []byte("Правила"), []byte("Устройство: {{.DeviceID}}"))

		require.EqualError(t, err, `failed to build prompt "broken": template: user.tmpl:1:24: executing "user.tmpl" at <.DeviceID>: can't evaluate field DeviceID in type *describer.promptData`)
	})
}
