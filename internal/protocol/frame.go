// Package protocol implements the alv.v1 protocol between the application and the gateway:
// binary frames (PROTO-3), JSON messages of the application and the gateway (PROTO-4, PROTO-5),
// the protocol version (PROTO-10) and the error codes (PROTO-11).
//
// The reference vectors shared with the application are in testdata/protocol/vectors.json.
package protocol

import (
	"encoding/binary"
	"fmt"

	"github.com/google/uuid"
)

// Binary frame layout (PROTO-3): version, kind, request_id, seq, payload.
const (
	// FrameVersion is the only supported version of the binary frame header.
	FrameVersion byte = 0x01
	// FrameHeaderSize is the size of the binary frame header in bytes.
	FrameHeaderSize = 22
	// MaxAudioChunkBytes is the limit of an AUDIO_IN payload: 64 KiB.
	MaxAudioChunkBytes = 64 * 1024

	offsetVersion   = 0
	offsetKind      = 1
	offsetRequestID = 2
	offsetSeq       = 18
)

// FrameKind is the kind of a binary frame and its payload.
type FrameKind byte

// Frame kinds (PROTO-3).
const (
	// KindImageJPEG is a whole JPEG snapshot in one frame, application to gateway.
	KindImageJPEG FrameKind = 0x01
	// KindAudioIn is a chunk of the question, PCM s16le mono 16 kHz, application to gateway.
	KindAudioIn FrameKind = 0x02
	// KindAudioOut is a chunk of the answer, PCM s16le mono at the rate of answer.start, gateway to application.
	KindAudioOut FrameKind = 0x81
)

var frameKindNames = map[FrameKind]string{
	KindImageJPEG: "IMAGE_JPEG",
	KindAudioIn:   "AUDIO_IN",
	KindAudioOut:  "AUDIO_OUT",
}

// String returns the name of the kind as in PROTO-3: IMAGE_JPEG, AUDIO_IN, AUDIO_OUT.
func (k FrameKind) String() string {
	if name, ok := frameKindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("FrameKind(0x%02x)", byte(k))
}

// Frame is a binary frame of the protocol.
type Frame struct {
	Kind      FrameKind
	RequestID uuid.UUID // 16 bytes in network order (RFC 4122)
	Seq       uint32    // number of the chunk in the request, from 0
	Payload   []byte
}

// FrameLimits are the payload limits of the frames from the application (PROTO-3).
// AUDIO_OUT frames come from the gateway and are not limited.
type FrameLimits struct {
	MaxImageBytes      int // APP_MAX_IMAGE_BYTES
	MaxAudioChunkBytes int
}

// NewFrameLimits returns the limits with the given snapshot limit and the 64 KiB audio chunk limit.
func NewFrameLimits(maxImageBytes int) FrameLimits {
	return FrameLimits{
		MaxImageBytes:      maxImageBytes,
		MaxAudioChunkBytes: MaxAudioChunkBytes,
	}
}

// EncodeFrame returns the frame as bytes: the 22-byte header and the payload.
func EncodeFrame(frame *Frame) []byte {
	data := make([]byte, FrameHeaderSize+len(frame.Payload))
	data[offsetVersion] = FrameVersion
	data[offsetKind] = byte(frame.Kind)
	copy(data[offsetRequestID:offsetSeq], frame.RequestID[:])
	binary.BigEndian.PutUint32(data[offsetSeq:FrameHeaderSize], frame.Seq)
	copy(data[FrameHeaderSize:], frame.Payload)

	return data
}

// DecodeFrame parses a binary frame and checks its payload against the limits (PROTO-3).
// The payload of the returned frame shares memory with data.
//
// A header shorter than 22 bytes, an unknown version or kind give a bad_request *Error and no frame.
// A payload over the limit gives a payload_too_large *Error together with the decoded frame,
// so that the error message can carry its request_id.
func DecodeFrame(data []byte, limits FrameLimits) (*Frame, error) {
	if len(data) < FrameHeaderSize {
		return nil, NewError(CodeBadRequest, errFrameShortFormat, FrameHeaderSize, len(data))
	}
	if data[offsetVersion] != FrameVersion {
		return nil, NewError(CodeBadRequest, errFrameVersionFormat, data[offsetVersion])
	}

	kind := FrameKind(data[offsetKind])
	if _, ok := frameKindNames[kind]; !ok {
		return nil, NewError(CodeBadRequest, errFrameKindFormat, data[offsetKind])
	}

	frame := &Frame{
		Kind:    kind,
		Seq:     binary.BigEndian.Uint32(data[offsetSeq:FrameHeaderSize]),
		Payload: data[FrameHeaderSize:],
	}
	copy(frame.RequestID[:], data[offsetRequestID:offsetSeq])

	switch {
	case kind == KindImageJPEG && len(frame.Payload) > limits.MaxImageBytes:
		return frame, NewError(CodePayloadTooLarge, errImageTooLargeFormat, limits.MaxImageBytes)
	case kind == KindAudioIn && len(frame.Payload) > limits.MaxAudioChunkBytes:
		return frame, NewError(CodePayloadTooLarge, errAudioChunkTooLargeFormat, limits.MaxAudioChunkBytes)
	}

	return frame, nil
}
