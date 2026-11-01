package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errStreamClosed is the cancellation cause of a stream closed by the caller.
var errStreamClosed = errors.New("stream closed")

// Stream is an answer of a model service that comes in chunks: the audio of TTS.
//
// Next returns the chunks as they arrive. A broken stream is ErrUnavailable;
// a stream interrupted by the caller's context is not counted as a failure.
type Stream struct {
	client  *Client
	parent  context.Context
	ctx     context.Context
	cancel  context.CancelCauseFunc
	header  http.Header
	body    io.ReadCloser
	buffer  []byte
	first   []byte
	pending error
	closed  bool
}

// Header returns the headers of the answer.
func (s *Stream) Header() http.Header {
	return s.header
}

// Next returns the next chunk of the answer; io.EOF after the last one.
func (s *Stream) Next() ([]byte, error) {
	if s.first != nil {
		chunk := s.first
		s.first = nil
		return chunk, nil
	}

	chunk, err := s.read()
	switch {
	case err == nil:
		return chunk, nil
	case errors.Is(err, io.EOF):
		return nil, io.EOF
	case s.parent.Err() != nil:
		return nil, fmt.Errorf(errInterruptedFormat, s.client.name, s.parent.Err())
	case errors.Is(context.Cause(s.ctx), errStreamClosed):
		return nil, fmt.Errorf(errInterruptedFormat, s.client.name, errStreamClosed)
	default:
		return nil, s.client.count(ReasonUnavailable, fmt.Errorf(errTransportFormat, ErrUnavailable, s.client.name, err))
	}
}

// Close stops the stream and releases its connection. It is safe to call it more than once.
func (s *Stream) Close() {
	if s.closed {
		return
	}
	s.closed = true
	s.cancel(errStreamClosed)
	_ = s.body.Close()
}

// read returns a copy of the next non-empty piece of the body or the error that ended it.
// An error that came together with data is kept for the next call.
func (s *Stream) read() ([]byte, error) {
	if s.pending != nil {
		return nil, s.pending
	}

	for {
		n, err := s.body.Read(s.buffer)
		if n > 0 {
			s.pending = err
			chunk := make([]byte, n)
			copy(chunk, s.buffer[:n])
			return chunk, nil
		}
		if err != nil {
			s.pending = err
			return nil, err
		}
	}
}
