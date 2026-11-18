// Package tts is the client of the tts-service (TTS-1): the audio of a
// sentence comes as a stream of PCM chunks.
package tts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
)

const (
	pathSynthesize = "/v1/synthesize"

	// FormatPCM is the requested audio format: PCM s16le mono, streamed.
	FormatPCM = "pcm_s16le"
	// ContentTypeAudio is the media type of the streamed audio (audio/L16; rate=<rate>; channels=1).
	ContentTypeAudio = "audio/L16"
	// Speed is the speech rate sent to the service: the default of TTS-1.
	Speed = 1.0

	headerSampleRate = "X-Sample-Rate"
	headerCache      = "X-Cache"
	bytesPerSample   = 2 // s16le mono
)

// SynthesizeRequest is the body of POST /v1/synthesize (TTS-1).
type SynthesizeRequest struct {
	Text       string  `json:"text"`
	Voice      string  `json:"voice,omitempty"` // empty - the default voice of the service
	SampleRate int     `json:"sample_rate"`
	Format     string  `json:"format"`
	Speed      float64 `json:"speed"`
}

// Client calls the TTS service.
type Client struct {
	client     *upstream.Client
	voice      string
	sampleRate int
}

// NewClient creates the TTS client from APP_TTS_URL, APP_TTS_TOKEN, APP_TTS_FIRST_CHUNK_TIMEOUT_MS,
// APP_TTS_VOICE and APP_AUDIO_OUT_SAMPLE_RATE.
func NewClient(vars *env.Vars, m *metrics.Metrics) (*Client, error) {
	client, err := upstream.NewClient(upstream.Config{
		Name:    upstream.NameTTS,
		BaseURL: vars.AppTTSURL,
		Token:   vars.AppTTSToken,
		Timeout: time.Duration(vars.AppTTSFirstChunkTimeoutMS) * time.Millisecond,
	}, m)
	if err != nil {
		return nil, fmt.Errorf("failed to create TTS client: %w", err)
	}
	return &Client{
		client:     client,
		voice:      vars.AppTTSVoice,
		sampleRate: vars.AppAudioOutSampleRate,
	}, nil
}

// Synthesize starts the synthesis of the text (POST /v1/synthesize) and returns once the first audio chunk
// arrived within APP_TTS_FIRST_CHUNK_TIMEOUT_MS. The rest of the stream is limited only by ctx.
// The stream must be closed.
func (c *Client) Synthesize(ctx context.Context, requestID, text string) (*Stream, error) {
	body, err := json.Marshal(&SynthesizeRequest{
		Text:       text,
		Voice:      c.voice,
		SampleRate: c.sampleRate,
		Format:     FormatPCM,
		Speed:      Speed,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode synthesis request: %w", err)
	}

	request := &upstream.Request{
		Method:      http.MethodPost,
		Path:        pathSynthesize,
		RequestID:   requestID,
		ContentType: upstream.ContentTypeJSON,
		Body:        body,
	}
	stream, err := c.client.Stream(ctx, request, ContentTypeAudio)
	if err != nil {
		return nil, fmt.Errorf("failed to synthesize speech: %w", err)
	}

	sampleRate, _ := strconv.Atoi(stream.Header().Get(headerSampleRate))
	return &Stream{
		stream:     stream,
		SampleRate: sampleRate,
		Cache:      stream.Header().Get(headerCache),
	}, nil
}

// Info returns the configuration of the TTS service (GET /v1/info) for config_id.
func (c *Client) Info(ctx context.Context) (*upstream.Info, error) {
	return c.client.Info(ctx)
}

// Stream is the PCM s16le mono audio of one synthesis.
//
// Every chunk holds whole samples: an odd byte is kept for the next chunk, so
// the chunks can go to the application as AUDIO_OUT frames one by one.
type Stream struct {
	// SampleRate is X-Sample-Rate of the answer; zero if the header is missing.
	SampleRate int
	// Cache is X-Cache of the answer: hit or miss.
	Cache string

	stream  *upstream.Stream
	pending []byte
}

// Next returns the next audio chunk; io.EOF after the last one.
func (s *Stream) Next() ([]byte, error) {
	for {
		chunk, err := s.stream.Next()
		if err != nil {
			return nil, err
		}
		if len(s.pending) > 0 {
			chunk = append(s.pending, chunk...)
			s.pending = nil
		}
		if odd := len(chunk) % bytesPerSample; odd != 0 {
			s.pending = append([]byte(nil), chunk[len(chunk)-odd:]...)
			chunk = chunk[:len(chunk)-odd]
		}
		if len(chunk) > 0 {
			return chunk, nil
		}
	}
}

// Close stops the synthesis and releases the connection. It is safe to call it more than once.
func (s *Stream) Close() {
	s.stream.Close()
}
