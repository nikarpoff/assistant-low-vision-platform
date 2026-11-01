// Package asr is the client of the asr-service (ASR-1).
package asr

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

const (
	pathRecognize = "/v1/recognize"

	// ContentTypePCM is the type of the question audio: the AUDIO_IN frames of the application, PCM s16le mono 16 kHz.
	ContentTypePCM = "audio/L16; rate=16000; channels=1"
)

// RecognizeResponse is the body of POST /v1/recognize (ASR-1).
// No speech is a normal answer with Speech=false and an empty Text (ASR-2).
type RecognizeResponse struct {
	Text            string             `json:"text"`
	Normalized      string             `json:"normalized"`
	Language        string             `json:"language"`
	Speech          bool               `json:"speech"`
	AudioDurationMS int                `json:"audio_duration_ms"`
	Model           Model              `json:"model"`
	Timings         map[string]float64 `json:"timings"` // stage -> milliseconds
	RTF             float64            `json:"rtf"`
}

// Model is the model that answered.
type Model struct {
	Backend string `json:"backend"`
	Name    string `json:"name"`
	Device  string `json:"device"`
}

// Client calls the ASR service.
type Client struct {
	client *upstream.Client
}

// NewClient creates the ASR client from APP_ASR_URL, APP_ASR_TOKEN and APP_ASR_TIMEOUT_MS.
func NewClient(vars *env.Vars, m *metrics.Metrics) (*Client, error) {
	client, err := upstream.NewClient(upstream.Config{
		Name:    upstream.NameASR,
		BaseURL: vars.AppASRURL,
		Token:   vars.AppASRToken,
		Timeout: time.Duration(vars.AppASRTimeoutMS) * time.Millisecond,
	}, m)
	if err != nil {
		return nil, fmt.Errorf("failed to create ASR client: %w", err)
	}
	return &Client{client: client}, nil
}

// Recognize transcribes the question (POST /v1/recognize) within APP_ASR_TIMEOUT_MS.
// The audio is PCM s16le mono 16 kHz sent as is, without base64.
func (c *Client) Recognize(ctx context.Context, requestID string, pcm []byte) (*RecognizeResponse, error) {
	response := &RecognizeResponse{}
	request := &upstream.Request{
		Method:      http.MethodPost,
		Path:        pathRecognize,
		RequestID:   requestID,
		ContentType: ContentTypePCM,
		Body:        pcm,
	}
	if err := c.client.Call(ctx, request, response); err != nil {
		return nil, fmt.Errorf("failed to recognize speech: %w", err)
	}
	return response, nil
}

// Info returns the configuration of the ASR service (GET /v1/info) for config_id.
func (c *Client) Info(ctx context.Context) (*upstream.Info, error) {
	return c.client.Info(ctx)
}
