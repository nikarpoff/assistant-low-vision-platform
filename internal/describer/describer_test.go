package describer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/fakellm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/hazard"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/history"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/llm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/snapshot"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

// vocabularyPath is the vocabulary example of the detector contract.
const vocabularyPath = "../../testdata/contracts/objects-detector/vocabulary.response.json"

// Settings and identifiers of the tests.
const (
	testLLMModel  = "qwen/qwen3-8b"
	testVLMModel  = "qwen/qwen2.5-vl-7b-instruct"
	testAPIKey    = "sk-test-key"
	testRequestID = "3f6c1f0e-8a2b-4c7d-9e10-5b2a7c4d9e01"
	testDeviceID  = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	testLabel     = "P07"
	testExtraBody = `{"reasoning":{"enabled":false}}`
)

// testImage is a small fake JPEG snapshot: SOI, a marker and EOI.
var testImage = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x02, 0x03, 0xFF, 0xD9}

// readVocabulary reads the vocabulary example of the detector contract.
func readVocabulary(t *testing.T) *vocabulary.Vocabulary {
	t.Helper()
	body, err := os.ReadFile(vocabularyPath)
	require.NoError(t, err)
	document := &vocabulary.Document{}
	require.NoError(t, json.Unmarshal(body, document))
	v, err := vocabulary.NewVocabulary(document)
	require.NoError(t, err)
	return v
}

// readPolicy reads the default hazard rules checked against the vocabulary.
func readPolicy(t *testing.T, vocab *vocabulary.Vocabulary) *hazard.Policy {
	t.Helper()
	rules, err := hazard.LoadRules("")
	require.NoError(t, err)
	policy, err := hazard.NewPolicy(rules, vocab)
	require.NoError(t, err)
	return policy
}

// object builds a detected object.
func object(label string, direction detector.Direction, distance detector.Distance, area float64) detector.Object {
	return detector.Object{Label: label, Direction: direction, Distance: distance, Confidence: 0.8, Area: area}
}

// roomObjects is the room of DSC-8: a bed ahead, a chair and a wardrobe on the left, a door on the right.
func roomObjects() []detector.Object {
	return []detector.Object{
		object("bed", detector.DirectionAhead, detector.DistanceNear, 0.3),
		object("wardrobe", detector.DirectionLeft, detector.DistanceFar, 0.12),
		object("chair", detector.DirectionLeft, detector.DistanceNear, 0.08),
		object("door", detector.DirectionRight, detector.DistanceFar, 0.1),
	}
}

// deviceHistory keeps a history by the device id and returns it, as the pipeline does.
func deviceHistory(t *testing.T) history.History {
	t.Helper()
	store, err := history.NewStore(5, 6, 10*time.Minute, time.Now)
	require.NoError(t, err)
	store.AddScene(testDeviceID, history.Scene{Objects: roomObjects(), Description: "Впереди кровать, слева шкаф и стул."})
	store.AddTurn(testDeviceID,
		history.Turn{Role: history.RoleQuestion, Text: "Где-то есть дверь?"},
		history.Turn{Role: history.RoleAnswer, Text: "Да, дверь справа, далеко."},
	)
	return store.Get(testDeviceID)
}

// provider is the stub provider in the process and the client of the gateway to it.
type provider struct {
	stub   *fakellm.Server
	client *llm.Client
}

// newProvider starts the stub provider and creates the language model client of vars.
func newProvider(t *testing.T, vars *env.Vars) *provider {
	t.Helper()
	stub := fakellm.NewServer(fakellm.Config{APIKey: testAPIKey})
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	vars.AppLLMURL = server.URL + fakellm.BasePath
	client, err := llm.NewClient(llm.NewConfig(vars), metrics.NewMetrics())
	require.NoError(t, err)
	return &provider{stub: stub, client: client}
}

// loadVars loads the configuration of the gateway in the mode.
func loadVars(t *testing.T, mode env.DescriberMode) *env.Vars {
	t.Helper()
	t.Setenv("APP_AUTH_DISABLED", "true")
	t.Setenv("APP_DESCRIBER_MODE", string(mode))
	t.Setenv("APP_LLM_MODEL", testLLMModel)
	t.Setenv("APP_LLM_API_KEY", testAPIKey)
	t.Setenv("APP_LLM_EXTRA_BODY", testExtraBody)
	vars, err := env.Load()
	require.NoError(t, err)
	return vars
}

// requestBody decodes the body of a recorded request.
func requestBody(t *testing.T, recorded fakellm.RecordedRequest) map[string]any {
	t.Helper()
	body := map[string]any{}
	require.NoError(t, json.Unmarshal(recorded.Body, &body))
	return body
}

// collectText returns a TextHandler that collects the text.
func collectText(texts *[]string) llm.TextHandler {
	return func(text string) error {
		*texts = append(*texts, text)
		return nil
	}
}

func TestDescriber_VLM(t *testing.T) {
	var (
		vars     *env.Vars
		vocab    *vocabulary.Vocabulary
		policy   *hazard.Policy
		prompt   *Prompt
		stub     *provider
		input    *Input
		describe func(t *testing.T) *Result
	)

	setup := func(t *testing.T, configure func(vars *env.Vars)) {
		logger.InitTestLogger()
		vars = loadVars(t, env.DescriberModeVLM)
		t.Setenv("APP_VLM_MODEL", testVLMModel)
		vars.AppVLMModel = testVLMModel
		if configure != nil {
			configure(vars)
		}
		vocab = readVocabulary(t)
		policy = readPolicy(t, vocab)
		var err error
		prompt, err = LoadPrompt(vars.AppPrompt)
		require.NoError(t, err)
		stub = newProvider(t, vars)
		input = &Input{
			RequestID: testRequestID,
			Objects:   roomObjects(),
			Focus:     protocol.FocusAll,
			Image:     testImage,
		}
		describe = func(t *testing.T) *Result {
			describer, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, stub.client)
			require.NoError(t, err)
			result, err := describer.Describe(context.Background(), input, nil)
			require.NoError(t, err)
			return result
		}
	}

	// userContent returns the content parts of the user message of the single request.
	userContent := func(t *testing.T) (string, []any) {
		t.Helper()
		recorded := stub.stub.Requests()
		require.Len(t, recorded, 1)
		body := requestBody(t, recorded[0])
		messages := body["messages"].([]any)
		require.Len(t, messages, 2)
		user := messages[1].(map[string]any)
		require.Equal(t, llm.RoleUser, user["role"])
		return body["model"].(string), user["content"].([]any)
	}

	t.Run("must_pass_image_data_url", func(t *testing.T) {
		setup(t, nil)

		result := describe(t)

		require.Equal(t, env.DescriberModeVLM, result.Mode)
		require.Equal(t, fakellm.DefaultText, result.Text)
		_, content := userContent(t)
		require.Len(t, content, 2)
		text := content[0].(map[string]any)
		require.Equal(t, llm.ContentPartText, text["type"])
		require.Contains(t, text["text"], "К сообщению приложен снимок камеры.")
		image := content[1].(map[string]any)
		require.Equal(t, llm.ContentPartImageURL, image["type"])
		url := image["image_url"].(map[string]any)["url"].(string)
		require.True(t, strings.HasPrefix(url, "data:image/jpeg;base64,"))
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/jpeg;base64,"))
		require.NoError(t, err)
		require.Equal(t, testImage, decoded)
	})

	t.Run("must_pass_detector_hints", func(t *testing.T) {
		setup(t, nil)

		describe(t)

		_, content := userContent(t)
		text := content[0].(map[string]any)["text"].(string)
		require.Contains(t, text, "Подсказки детектора: кровать - впереди, близко; стул - слева, близко; шкаф - слева, далеко; дверь - справа, далеко.")
		require.NotContains(t, text, "Объекты текущей сцены")
	})

	t.Run("must_pass_without_detections", func(t *testing.T) {
		setup(t, func(vars *env.Vars) { vars.AppVLMWithDetections = false })

		describe(t)

		_, content := userContent(t)
		text := content[0].(map[string]any)["text"].(string)
		require.NotContains(t, text, "Подсказки детектора")
		require.NotContains(t, text, "кровать")
		require.Equal(t, "К сообщению приложен снимок камеры.\nВопрос: Опиши обстановку", text)
	})

	t.Run("must_pass_without_detector", func(t *testing.T) {
		setup(t, nil)
		input.Objects = nil

		result := describe(t)

		require.False(t, result.Hazard)
		_, content := userContent(t)
		require.Len(t, content, 2)
		require.Equal(t, "К сообщению приложен снимок камеры.\nВопрос: Опиши обстановку", content[0].(map[string]any)["text"])
	})

	t.Run("must_pass_vlm_model", func(t *testing.T) {
		setup(t, nil)

		describe(t)

		model, _ := userContent(t)
		require.Equal(t, testVLMModel, model)
	})

	t.Run("must_pass_llm_model_when_vlm_model_empty", func(t *testing.T) {
		setup(t, func(vars *env.Vars) { vars.AppVLMModel = "" })

		describe(t)

		model, _ := userContent(t)
		require.Equal(t, testLLMModel, model)
	})

	t.Run("must_pass_question_with_image", func(t *testing.T) {
		setup(t, nil)
		input.Question = "Где-то есть дверь?"

		describe(t)

		_, content := userContent(t)
		require.True(t, strings.HasSuffix(content[0].(map[string]any)["text"].(string), "Вопрос: Где-то есть дверь?"))
	})
}

func TestDescriber_Privacy(t *testing.T) {
	var (
		vars   *env.Vars
		vocab  *vocabulary.Vocabulary
		policy *hazard.Policy
		prompt *Prompt
		stub   *provider
		input  *Input
	)

	setup := func(t *testing.T, mode env.DescriberMode) {
		logger.InitTestLogger()
		vars = loadVars(t, mode)
		vocab = readVocabulary(t)
		policy = readPolicy(t, vocab)
		var err error
		prompt, err = LoadPrompt(vars.AppPrompt)
		require.NoError(t, err)
		stub = newProvider(t, vars)
		input = &Input{
			RequestID: testRequestID,
			Objects:   roomObjects(),
			Focus:     protocol.FocusAll,
			Question:  "Что слева?",
			History:   deviceHistory(t),
			Image:     testImage,
		}
		describer, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, stub.client)
		require.NoError(t, err)
		_, err = describer.Describe(context.Background(), input, nil)
		require.NoError(t, err)
	}

	// requireNoIdentifiers checks the recorded request has no identifiers of the device,
	// the participant and the request, no field user and no header X-Request-ID.
	requireNoIdentifiers := func(t *testing.T, recorded fakellm.RecordedRequest) {
		t.Helper()
		body := string(recorded.Body)
		for _, identifier := range []string{testDeviceID, testLabel, testRequestID, "device_id", "request_id", `"label"`} {
			require.NotContains(t, body, identifier)
		}
		require.NotContains(t, requestBody(t, recorded), "user")
		require.Empty(t, recorded.Header.Values("X-Request-ID"))
		for name, values := range recorded.Header {
			for _, value := range values {
				require.NotContains(t, value, testRequestID, name)
				require.NotContains(t, value, testDeviceID, name)
			}
		}
		require.Equal(t, "Bearer "+testAPIKey, recorded.Header.Get("Authorization"))
		require.Equal(t, "application/json", recorded.Header.Get("Content-Type"))
		require.Equal(t, "text/event-stream", recorded.Header.Get("Accept"))
	}

	t.Run("must_pass_llm_text_only", func(t *testing.T) {
		setup(t, env.DescriberModeLLM)

		recorded := stub.stub.Requests()
		require.Len(t, recorded, 1)
		requireNoIdentifiers(t, recorded[0])
		body := string(recorded[0].Body)
		require.NotContains(t, body, "image_url")
		require.NotContains(t, body, "data:image")
		require.NotContains(t, body, base64.StdEncoding.EncodeToString(testImage))
		require.NotContains(t, body, "/9j/") // the base64 start of any JPEG
		for _, message := range requestBody(t, recorded[0])["messages"].([]any) {
			require.IsType(t, "", message.(map[string]any)["content"])
		}
		// The text that is needed for the answer is there: objects, history, question
		require.Contains(t, body, "кровать - впереди, близко")
		require.Contains(t, body, "Ассистент: Да, дверь справа, далеко.")
		require.Contains(t, body, "Вопрос: Что слева?")
	})

	t.Run("must_pass_vlm_image_only", func(t *testing.T) {
		setup(t, env.DescriberModeVLM)

		recorded := stub.stub.Requests()
		require.Len(t, recorded, 1)
		requireNoIdentifiers(t, recorded[0])
		body := string(recorded[0].Body)
		require.Equal(t, 1, strings.Count(body, `"image_url":`))
		require.Equal(t, 1, strings.Count(body, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(testImage)))
	})
}

func TestDescriberMode(t *testing.T) {
	var (
		vars      *env.Vars
		vocab     *vocabulary.Vocabulary
		policy    *hazard.Policy
		prompt    *Prompt
		completer *CompleterMock
		input     *Input
		texts     []string
	)

	setup := func(t *testing.T, mode env.DescriberMode) {
		logger.InitTestLogger()
		vars = loadVars(t, mode)
		vocab = readVocabulary(t)
		policy = readPolicy(t, vocab)
		var err error
		prompt, err = LoadPrompt(vars.AppPrompt)
		require.NoError(t, err)
		completer = &CompleterMock{
			CompleteFunc: func(ctx context.Context, req *llm.Request, onText llm.TextHandler) (*llm.Result, error) {
				return &llm.Result{}, nil
			},
		}
		input = &Input{RequestID: testRequestID, Objects: roomObjects(), Focus: protocol.FocusAll, Image: testImage}
		texts = nil
	}

	// configID collects the snapshot of vars with the prompt hash and returns config_id.
	configID := func(t *testing.T, vars *env.Vars, prompt *Prompt) string {
		t.Helper()
		collector, err := snapshot.NewCollector(vars, snapshot.Hashes{Prompt: prompt.Hash()}, snapshot.Sources{})
		require.NoError(t, err)
		collected, err := collector.Refresh(context.Background())
		require.NoError(t, err)
		return collected.ConfigID
	}

	t.Run("must_pass_template_no_llm", func(t *testing.T) {
		setup(t, env.DescriberModeTemplate)
		stub := newProvider(t, vars)

		describer, err := NewDescriber(NewConfig(vars), vocab, policy, nil, nil)
		require.NoError(t, err)
		result, err := describer.Describe(context.Background(), input, collectText(&texts))
		require.NoError(t, err)
		withClient, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, completer)
		require.NoError(t, err)
		_, err = withClient.Describe(context.Background(), input, nil)
		require.NoError(t, err)

		require.Equal(t, env.DescriberModeTemplate, describer.Mode())
		require.Equal(t, env.DescriberModeTemplate, result.Mode)
		require.Nil(t, result.LLM)
		require.Equal(t, "Впереди кровать, слева стул и шкаф, справа дверь.", result.Text)
		require.Equal(t, []string{result.Text}, texts)
		require.Empty(t, completer.CompleteCalls())
		require.Empty(t, stub.stub.Requests())
	})

	t.Run("must_pass_llm", func(t *testing.T) {
		setup(t, env.DescriberModeLLM)
		stub := newProvider(t, vars)

		describer, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, stub.client)
		require.NoError(t, err)
		result, err := describer.Describe(context.Background(), input, collectText(&texts))

		require.NoError(t, err)
		require.Equal(t, env.DescriberModeLLM, describer.Mode())
		require.Equal(t, env.DescriberModeLLM, result.Mode)
		require.Equal(t, fakellm.DefaultText, result.Text)
		require.Equal(t, fakellm.DefaultText, strings.Join(texts, ""))
		require.Equal(t, fakellm.DefaultModel, result.LLM.Model)
		recorded := stub.stub.Requests()
		require.Len(t, recorded, 1)
		require.Equal(t, http.MethodPost, recorded[0].Method)
		require.Equal(t, fakellm.PathChatCompletions, recorded[0].Path)
		require.Equal(t, testLLMModel, requestBody(t, recorded[0])["model"])
		require.NotContains(t, string(recorded[0].Body), "image_url")
	})

	t.Run("must_pass_vlm", func(t *testing.T) {
		setup(t, env.DescriberModeVLM)
		stub := newProvider(t, vars)

		describer, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, stub.client)
		require.NoError(t, err)
		result, err := describer.Describe(context.Background(), input, collectText(&texts))

		require.NoError(t, err)
		require.Equal(t, env.DescriberModeVLM, describer.Mode())
		require.Equal(t, env.DescriberModeVLM, result.Mode)
		require.Equal(t, fakellm.DefaultText, result.Text)
		recorded := stub.stub.Requests()
		require.Len(t, recorded, 1)
		require.Equal(t, testLLMModel, requestBody(t, recorded[0])["model"])
		require.Equal(t, 1, strings.Count(string(recorded[0].Body), `"image_url":`))
	})

	t.Run("must_pass_mode_in_config_id", func(t *testing.T) {
		setup(t, env.DescriberModeLLM)

		ids := map[string]env.DescriberMode{}
		for _, mode := range []env.DescriberMode{env.DescriberModeTemplate, env.DescriberModeLLM, env.DescriberModeVLM} {
			vars.AppDescriberMode = mode
			id := configID(t, vars, prompt)
			require.Len(t, id, 12)
			require.Equal(t, id, configID(t, vars, prompt))
			require.NotContains(t, ids, id, mode)
			ids[id] = mode
		}

		// The name and the hash of the prompt are part of config_id too
		vars.AppDescriberMode = env.DescriberModeLLM
		base := configID(t, vars, prompt)
		changed, err := NewPrompt(prompt.Name(), []byte(prompt.System()+"\nОтвечай вежливо."), []byte("Вопрос: {{.Question}}"))
		require.NoError(t, err)
		require.NotEqual(t, prompt.Hash(), changed.Hash())
		require.NotEqual(t, base, configID(t, vars, changed))
		vars.AppPrompt = "short"
		require.NotEqual(t, base, configID(t, vars, prompt))
	})

	t.Run("must_pass_template_fallback_in_llm_mode", func(t *testing.T) {
		setup(t, env.DescriberModeLLM)

		describer, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, completer)
		require.NoError(t, err)
		result, err := describer.Template(input, func(string) error { return llm.ErrStop })

		require.NoError(t, err)
		require.Equal(t, env.DescriberModeTemplate, result.Mode)
		require.Equal(t, "Впереди кровать, слева стул и шкаф, справа дверь.", result.Text)
		require.Empty(t, completer.CompleteCalls())
	})

	t.Run("must_pass_llm_error_passed_through", func(t *testing.T) {
		setup(t, env.DescriberModeLLM)
		failure := &llm.Error{Reason: llm.ReasonHTTP429, StatusCode: http.StatusTooManyRequests}
		completer.CompleteFunc = func(ctx context.Context, req *llm.Request, onText llm.TextHandler) (*llm.Result, error) {
			return &llm.Result{Text: "Впереди"}, failure
		}

		describer, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, completer)
		require.NoError(t, err)
		result, err := describer.Describe(context.Background(), input, nil)

		require.ErrorIs(t, err, llm.ErrUnavailable)
		require.Equal(t, "Впереди", result.Text)
		require.Len(t, completer.CompleteCalls(), 1)
		require.Equal(t, testRequestID, completer.CompleteCalls()[0].Req.RequestID)
	})

	t.Run("must_fail_handle_template_text", func(t *testing.T) {
		setup(t, env.DescriberModeTemplate)

		describer, err := NewDescriber(NewConfig(vars), vocab, policy, nil, nil)
		require.NoError(t, err)
		_, err = describer.Describe(context.Background(), input, func(string) error { return context.Canceled })

		require.EqualError(t, err, "failed to handle template text: context canceled")
	})

	t.Run("must_fail_unknown_mode", func(t *testing.T) {
		setup(t, env.DescriberModeLLM)

		_, err := NewDescriber(Config{Mode: "chat", Model: testLLMModel}, vocab, policy, prompt, completer)

		require.EqualError(t, err, `unknown describer mode "chat": allowed values are template, llm, vlm`)
	})

	t.Run("must_fail_no_client", func(t *testing.T) {
		setup(t, env.DescriberModeLLM)

		_, err := NewDescriber(NewConfig(vars), vocab, policy, prompt, nil)

		require.EqualError(t, err, errNoLLMClient)
	})

	t.Run("must_fail_no_prompt", func(t *testing.T) {
		setup(t, env.DescriberModeVLM)

		_, err := NewDescriber(NewConfig(vars), vocab, policy, nil, completer)

		require.EqualError(t, err, errNoPrompt)
	})

	t.Run("must_fail_no_model", func(t *testing.T) {
		setup(t, env.DescriberModeVLM)

		_, err := NewDescriber(Config{Mode: env.DescriberModeVLM}, vocab, policy, prompt, completer)

		require.EqualError(t, err, "language model identifier is required in the vlm mode")
	})
}
