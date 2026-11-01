// Package snapshot collects the configuration of the stand and its config_id (PLT-12).
//
// A snapshot holds /v1/info of the detector, ASR and TTS, the language model
// provider and model (not in the template mode) and the own settings of the
// gateway, with the variant of placement in the top-level field placement.
// config_id is the first 12 hex characters of SHA-256 of the canonical JSON
// of the snapshot (sorted keys, no spaces). Secrets never get into it.
package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

// configIDLength is the number of hex characters of SHA-256 in config_id.
const configIDLength = 12

// Snapshot is a collected configuration: the body of GET /v1/config (PLT-13).
type Snapshot struct {
	ConfigID    string          `json:"config_id"`
	CollectedAt time.Time       `json:"collected_at"` // UTC
	Config      json.RawMessage `json:"config"`       // canonical JSON of Config
}

// Config is the configuration of the stand (PLT-12).
type Config struct {
	// Placement is APP_PLACEMENT: the variant of placement (A, B) the experiment compares by.
	Placement     string   `json:"placement"`
	DescriberMode string   `json:"describer_mode"`
	Services      Services `json:"services"`
	// LLM is the language model provider and model; nil in the template mode.
	LLM             *LLM       `json:"llm,omitempty"`
	Prompt          Prompt     `json:"prompt"`
	Generation      Generation `json:"generation"`
	TimeoutsMS      Timeouts   `json:"timeouts_ms"`
	HazardRulesHash string     `json:"hazard_rules_hash"`
	IntentsHash     string     `json:"intents_hash"`
	TTSVoice        string     `json:"tts_voice"` // empty - the default voice of the service
	Image           Image      `json:"image"`
}

// Services are /v1/info of the model services; nil if the service has not answered yet.
type Services struct {
	Detector *upstream.Info `json:"detector"`
	ASR      *upstream.Info `json:"asr"`
	TTS      *upstream.Info `json:"tts"`
}

// LLM is the language model provider and the model of the active describer mode.
type LLM struct {
	// URL is the scheme, host and path of APP_LLM_URL, without credentials and query.
	URL   string `json:"url"`
	Model string `json:"model"`
}

// Prompt is the prompt template (APP_PROMPT) and the hash of its files.
type Prompt struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// Generation are the generation parameters of the language model (DSC-5).
type Generation struct {
	Temperature float64        `json:"temperature"`
	Seed        int            `json:"seed"`
	MaxTokens   int            `json:"max_tokens"`
	ExtraBody   map[string]any `json:"extra_body"` // APP_LLM_EXTRA_BODY
}

// Timeouts are the timeouts of PLT-8 in milliseconds.
type Timeouts struct {
	Detect          int `json:"detect"`
	ASR             int `json:"asr"`
	LLMTTFT         int `json:"llm_ttft"`
	LLMTotal        int `json:"llm_total"`
	TTSFirstChunk   int `json:"tts_first_chunk"`
	RequestDeadline int `json:"request_deadline"`
	PassiveDeadline int `json:"passive_deadline"`
}

// Image are the parameters of the snapshot the application sends.
type Image struct {
	MaxSide     int `json:"max_side"`
	JPEGQuality int `json:"jpeg_quality"`
}

// canonicalJSON encodes the value as JSON with sorted keys, without spaces and without HTML escaping.
// Numbers keep their text, so the same configuration always gives the same bytes.
func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// configID returns the first 12 hex characters of SHA-256 of the canonical JSON.
func configID(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return strings.ToLower(hex.EncodeToString(sum[:]))[:configIDLength]
}
