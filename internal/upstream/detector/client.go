// Package detector is the client of the objects-detector service (DET-1, DET-2).
package detector

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/metrics"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/vocabulary"
)

const (
	pathDetect     = "/v1/detect"
	pathVocabulary = "/v1/vocabulary"

	// ContentTypeJPEG is the type of the snapshot sent to the detector: the JPEG from the application as is.
	ContentTypeJPEG = "image/jpeg"
)

// Client calls the detector.
type Client struct {
	client *upstream.Client
}

// NewClient creates the detector client from APP_DETECTOR_URL, APP_DETECTOR_TOKEN and APP_DETECT_TIMEOUT_MS.
func NewClient(vars *env.Vars, m *metrics.Metrics) (*Client, error) {
	client, err := upstream.NewClient(upstream.Config{
		Name:    upstream.NameDetector,
		BaseURL: vars.AppDetectorURL,
		Token:   vars.AppDetectorToken,
		Timeout: time.Duration(vars.AppDetectTimeoutMS) * time.Millisecond,
	}, m)
	if err != nil {
		return nil, fmt.Errorf("failed to create detector client: %w", err)
	}
	return &Client{client: client}, nil
}

// Detect finds the objects on the JPEG snapshot (POST /v1/detect) within APP_DETECT_TIMEOUT_MS.
func (c *Client) Detect(ctx context.Context, requestID string, image []byte) (*DetectResponse, error) {
	response := &DetectResponse{}
	request := &upstream.Request{
		Method:      http.MethodPost,
		Path:        pathDetect,
		RequestID:   requestID,
		ContentType: ContentTypeJPEG,
		Body:        image,
	}
	if err := c.client.Call(ctx, request, response); err != nil {
		return nil, fmt.Errorf("failed to detect objects: %w", err)
	}
	return response, nil
}

// Vocabulary returns the vocabulary of the detector and its hash (GET /v1/vocabulary, DET-2).
func (c *Client) Vocabulary(ctx context.Context) (*vocabulary.Document, error) {
	document := &vocabulary.Document{}
	request := &upstream.Request{
		Method:  http.MethodGet,
		Path:    pathVocabulary,
		Timeout: upstream.MetadataTimeout,
	}
	if err := c.client.Call(ctx, request, document); err != nil {
		return nil, fmt.Errorf("failed to get vocabulary: %w", err)
	}
	return document, nil
}

// Info returns the configuration of the detector (GET /v1/info) for config_id.
func (c *Client) Info(ctx context.Context) (*upstream.Info, error) {
	return c.client.Info(ctx)
}
