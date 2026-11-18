package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

// vectorsPath is the reference vectors shared with the application (AC-2).
const vectorsPath = "../../testdata/protocol/vectors.json"

// defaultMaxImageBytes is the default of APP_MAX_IMAGE_BYTES: 2 MiB.
const defaultMaxImageBytes = 2 * 1024 * 1024

type frameVector struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	Kind       int    `json:"kind"`
	KindName   string `json:"kind_name"`
	RequestID  string `json:"request_id"`
	Seq        uint32 `json:"seq"`
	PayloadHex string `json:"payload_hex"`
	FrameHex   string `json:"frame_hex"`
}

type invalidFrameVector struct {
	Name     string `json:"name"`
	FrameHex string `json:"frame_hex"`
	Error    string `json:"error"`
}

type messageVector struct {
	Name      string          `json:"name"`
	Direction string          `json:"direction"`
	Message   json.RawMessage `json:"message"`
}

type invalidMessageVector struct {
	Name    string          `json:"name"`
	Message json.RawMessage `json:"message"`
	Error   string          `json:"error"`
}

type vectors struct {
	Frames          []frameVector          `json:"frames"`
	InvalidFrames   []invalidFrameVector   `json:"invalid_frames"`
	Messages        []messageVector        `json:"messages"`
	InvalidMessages []invalidMessageVector `json:"invalid_messages"`
}

// loadVectors reads testdata/protocol/vectors.json and fails on a field the tests do not know.
func loadVectors(t *testing.T) *vectors {
	t.Helper()

	data, err := os.ReadFile(vectorsPath)
	require.NoError(t, err)

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	result := &vectors{}
	require.NoError(t, decoder.Decode(result))
	require.NotEmpty(t, result.Frames)
	require.NotEmpty(t, result.InvalidFrames)
	require.NotEmpty(t, result.Messages)
	require.NotEmpty(t, result.InvalidMessages)

	return result
}

// invalidFrame returns the invalid frame vector with the name.
func (v *vectors) invalidFrame(t *testing.T, name string) []byte {
	t.Helper()

	for _, vector := range v.InvalidFrames {
		if vector.Name == name {
			require.Equal(t, string(CodeBadRequest), vector.Error, name)

			data, err := hex.DecodeString(vector.FrameHex)
			require.NoError(t, err, name)
			return data
		}
	}

	require.FailNow(t, "no invalid frame vector "+name)
	return nil
}

func decodeHex(t *testing.T, value string) []byte {
	t.Helper()

	data, err := hex.DecodeString(value)
	require.NoError(t, err)
	return data
}

func TestFrame(t *testing.T) {
	var (
		testVectors *vectors
		limits      FrameLimits
		requestID   uuid.UUID
	)

	setup := func() {
		logger.InitTestLogger()
		testVectors = loadVectors(t)
		limits = NewFrameLimits(defaultMaxImageBytes)
		requestID = uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	}

	t.Run("must_pass_vectors", func(t *testing.T) {
		setup()

		for _, vector := range testVectors.Frames {
			id, err := uuid.Parse(vector.RequestID)
			require.NoError(t, err, vector.Name)
			frame := &Frame{
				Kind:      FrameKind(vector.Kind),
				RequestID: id,
				Seq:       vector.Seq,
				Payload:   decodeHex(t, vector.PayloadHex),
			}

			encoded := EncodeFrame(frame)
			decoded, err := DecodeFrame(decodeHex(t, vector.FrameHex), limits)

			require.Equal(t, int(FrameVersion), vector.Version, vector.Name)
			require.Equal(t, vector.KindName, frame.Kind.String(), vector.Name)
			require.Equal(t, vector.FrameHex, hex.EncodeToString(encoded), vector.Name)
			require.NoError(t, err, vector.Name)
			require.Equal(t, frame.Kind, decoded.Kind, vector.Name)
			require.Equal(t, frame.RequestID, decoded.RequestID, vector.Name)
			require.Equal(t, frame.Seq, decoded.Seq, vector.Name)
			require.Equal(t, frame.Payload, decoded.Payload, vector.Name)
		}
	})

	t.Run("must_pass_payload_at_limit", func(t *testing.T) {
		setup()

		image := EncodeFrame(&Frame{Kind: KindImageJPEG, RequestID: requestID, Payload: make([]byte, defaultMaxImageBytes)})
		audio := EncodeFrame(&Frame{Kind: KindAudioIn, RequestID: requestID, Seq: 7, Payload: make([]byte, MaxAudioChunkBytes)})
		answer := EncodeFrame(&Frame{Kind: KindAudioOut, RequestID: requestID, Payload: make([]byte, defaultMaxImageBytes+1)})

		imageFrame, imageErr := DecodeFrame(image, limits)
		audioFrame, audioErr := DecodeFrame(audio, limits)
		answerFrame, answerErr := DecodeFrame(answer, limits)

		require.NoError(t, imageErr)
		require.Len(t, imageFrame.Payload, defaultMaxImageBytes)
		require.NoError(t, audioErr)
		require.Len(t, audioFrame.Payload, MaxAudioChunkBytes)
		require.Equal(t, uint32(7), audioFrame.Seq)
		require.NoError(t, answerErr)
		require.Len(t, answerFrame.Payload, defaultMaxImageBytes+1)
	})

	t.Run("must_fail_short_header", func(t *testing.T) {
		setup()
		data := testVectors.invalidFrame(t, "short_header")

		frame, err := DecodeFrame(data, limits)
		emptyFrame, emptyErr := DecodeFrame(nil, limits)

		require.Nil(t, frame)
		require.EqualError(t, err, string(CodeBadRequest)+": "+fmt.Sprintf(errFrameShortFormat, FrameHeaderSize, len(data)))
		require.Equal(t, CodeBadRequest, CodeOf(err))
		require.Nil(t, emptyFrame)
		require.EqualError(t, emptyErr, "bad_request: frame is shorter than the 22-byte header: 0 bytes")
	})

	t.Run("must_fail_unknown_version", func(t *testing.T) {
		setup()
		data := testVectors.invalidFrame(t, "unknown_version")

		frame, err := DecodeFrame(data, limits)

		require.Nil(t, frame)
		require.EqualError(t, err, string(CodeBadRequest)+": "+fmt.Sprintf(errFrameVersionFormat, 0x02))
		require.Equal(t, CodeBadRequest, CodeOf(err))
	})

	t.Run("must_fail_unknown_kind", func(t *testing.T) {
		setup()
		data := testVectors.invalidFrame(t, "unknown_kind")

		frame, err := DecodeFrame(data, limits)

		require.Nil(t, frame)
		require.EqualError(t, err, string(CodeBadRequest)+": "+fmt.Sprintf(errFrameKindFormat, 0x03))
		require.Equal(t, CodeBadRequest, CodeOf(err))
		require.Equal(t, "FrameKind(0x03)", FrameKind(0x03).String())
	})

	t.Run("must_fail_payload_too_large", func(t *testing.T) {
		setup()
		image := EncodeFrame(&Frame{Kind: KindImageJPEG, RequestID: requestID, Payload: make([]byte, defaultMaxImageBytes+1)})
		audio := EncodeFrame(&Frame{Kind: KindAudioIn, RequestID: requestID, Seq: 3, Payload: make([]byte, MaxAudioChunkBytes+1)})
		smallLimits := NewFrameLimits(1024)

		imageFrame, imageErr := DecodeFrame(image, limits)
		audioFrame, audioErr := DecodeFrame(audio, limits)
		_, smallErr := DecodeFrame(EncodeFrame(&Frame{Kind: KindImageJPEG, Payload: make([]byte, 1025)}), smallLimits)

		require.EqualError(t, imageErr, string(CodePayloadTooLarge)+": "+fmt.Sprintf(errImageTooLargeFormat, defaultMaxImageBytes))
		require.Equal(t, "payload_too_large: image exceeds 2097152 bytes", imageErr.Error())
		require.Equal(t, CodePayloadTooLarge, CodeOf(imageErr))
		require.Equal(t, requestID, imageFrame.RequestID)
		require.EqualError(t, audioErr, string(CodePayloadTooLarge)+": "+fmt.Sprintf(errAudioChunkTooLargeFormat, MaxAudioChunkBytes))
		require.Equal(t, CodePayloadTooLarge, CodeOf(audioErr))
		require.Equal(t, requestID, audioFrame.RequestID)
		require.Equal(t, uint32(3), audioFrame.Seq)
		require.EqualError(t, smallErr, "payload_too_large: image exceeds 1024 bytes")
	})

	t.Run("must_fail_invalid_vectors", func(t *testing.T) {
		setup()

		for _, vector := range testVectors.InvalidFrames {
			frame, err := DecodeFrame(decodeHex(t, vector.FrameHex), limits)

			require.Nil(t, frame, vector.Name)
			require.Error(t, err, vector.Name)
			require.Equal(t, vector.Error, string(CodeOf(err)), vector.Name)
		}
	})
}
