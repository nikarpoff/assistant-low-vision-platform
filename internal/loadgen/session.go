package loadgen

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/session"
)

// Fields of hello: loadgen introduces itself as an application of its own platform.
const (
	helloAppVersion = "0.1.0"
	helloLocale     = "ru-RU"
	helloPlatform   = "loadgen"
)

// audioChunkMS is the duration of an AUDIO_IN frame, as the application sends it (PROTO-7).
const audioChunkMS = 100

// closeWait limits the wait for the close handshake.
const closeWait = time.Second

// Texts of the errors, shared by the code and the tests.
const (
	errDialFormat        = "failed to connect to %s: %w"
	errDialStatusFormat  = "failed to connect to %s: HTTP %d: %w"
	errHelloFormat       = "failed to say hello: %w"
	errWelcomeFormat     = "failed to read welcome: %w"
	errNotWelcomeFormat  = "gateway answered hello with %s"
	errWriteFormat       = "failed to send %s: %w"
	errUnknownKindFormat = "unknown scenario %q"
)

// item is one request of the manifest.
type item struct {
	kind     string
	image    *Image    // describe, passive and ask with_image
	question *Question // ask
}

// conn is a session of a simulated application.
type conn struct {
	ws       *websocket.Conn
	configID string
	timeout  time.Duration
}

// dial opens a session: the subprotocol alv.v1, the device token, hello and welcome (PROTO-1, PROTO-2).
func dial(ctx context.Context, url, token string, timeout time.Duration) (*conn, error) {
	dialer := websocket.Dialer{
		Subprotocols:     []string{session.Subprotocol},
		HandshakeTimeout: timeout,
		Proxy:            http.ProxyFromEnvironment,
	}
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}

	ws, response, err := dialer.DialContext(ctx, url, header)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf(errDialStatusFormat, url, response.StatusCode, err)
		}
		return nil, fmt.Errorf(errDialFormat, url, err)
	}

	c := &conn{ws: ws, timeout: timeout}
	hello, err := protocol.EncodeMessage(&protocol.Hello{
		Protocol:   protocol.ProtocolVersion,
		DeviceID:   uuid.NewString(),
		AppVersion: helloAppVersion,
		Locale:     helloLocale,
		Platform:   helloPlatform,
		Network:    protocol.NetworkOther,
	})
	if err == nil {
		err = ws.WriteMessage(websocket.TextMessage, hello)
	}
	if err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf(errHelloFormat, err)
	}

	_ = ws.SetReadDeadline(time.Now().Add(timeout))
	message, _, err := c.read()
	if err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf(errWelcomeFormat, err)
	}
	welcome, ok := message.(*protocol.Welcome)
	if !ok {
		_ = ws.Close()
		return nil, fmt.Errorf(errWelcomeFormat, fmt.Errorf(errNotWelcomeFormat, describeMessage(message)))
	}
	c.configID = welcome.ConfigID
	return c, nil
}

// close ends the session with the close handshake.
func (c *conn) close() {
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(closeWait))
	_ = c.ws.Close()
}

// read returns the next message or frame of the gateway.
func (c *conn) read() (protocol.Message, *protocol.Frame, error) {
	messageType, data, err := c.ws.ReadMessage()
	if err != nil {
		return nil, nil, err
	}
	if messageType == websocket.BinaryMessage {
		frame, err := protocol.DecodeFrame(data, protocol.NewFrameLimits(0))
		return nil, frame, err
	}
	message, err := protocol.DecodeServerMessage(data)
	return message, nil, err
}

// do runs one request and reads everything up to its answer.end, passive.clear or error.
// broken reports that the connection is no longer usable: the next request needs a new session.
func (c *conn) do(it item) (record Record, broken bool) {
	id := uuid.New()
	record = Record{
		Kind:      it.kind,
		RequestID: id.String(),
		ConfigID:  c.configID,
		Timings:   map[string]protocol.Millis{},
	}
	if it.image != nil {
		imageID := it.image.ID
		record.ImageID = &imageID
	}
	if it.question != nil {
		questionID := it.question.ID
		record.QuestionID = &questionID
	}

	sent, err := c.send(id, it)
	record.SentAt = formatTime(sent)
	if err != nil {
		record.Status, record.ErrorCode = StatusError, CodeConnection
		return record, true
	}

	_ = c.ws.SetReadDeadline(sent.Add(c.timeout))
	for {
		message, frame, err := c.read()
		var netErr net.Error
		switch {
		case errors.As(err, &netErr) && netErr.Timeout():
			record.Status, record.ErrorCode = StatusError, CodeTimeout
			return record, true
		case err != nil && isTransport(err):
			record.Status, record.ErrorCode = StatusError, CodeConnection
			return record, true
		case err != nil:
			// A frame or a message loadgen does not understand is not about this request.
			continue
		}
		elapsed := protocol.DurationMillis(time.Since(sent))

		if frame != nil {
			if frame.RequestID != id || frame.Kind != protocol.KindAudioOut {
				continue
			}
			if record.FirstAudioMS == nil {
				record.FirstAudioMS = &elapsed
			}
			record.AudioBytes += len(frame.Payload)
			continue
		}

		switch m := message.(type) {
		case *protocol.Accepted:
			if m.RequestID == id {
				record.AcceptedMS = &elapsed
			}
		case *protocol.Transcript:
			if m.RequestID == id {
				record.Intent = string(m.Intent)
			}
		case *protocol.AnswerEnd:
			if m.RequestID != id {
				continue
			}
			record.EndMS = &elapsed
			record.Text = m.Text
			if m.Intent != "" {
				record.Intent = string(m.Intent)
			}
			record.Degraded = m.Degraded
			record.DegradedReason = string(m.DegradedReason)
			for stage, value := range m.Timings {
				record.Timings[stage] = value
			}
			record.Status = StatusOK
			if m.Degraded {
				record.Status = StatusDegraded
			}
			return record, false
		case *protocol.PassiveClear:
			if m.RequestID != id {
				continue
			}
			record.EndMS = &elapsed
			record.Status = clearStatus(m.Reason)
			return record, false
		case *protocol.ErrorMessage:
			// An error without request_id rejected a frame or a message of this request.
			if m.RequestID != nil && *m.RequestID != id {
				continue
			}
			record.EndMS = &elapsed
			record.Status, record.ErrorCode = StatusError, string(m.Code)
			return record, false
		}
	}
}

// send sends the input of the request and returns the moment the input is complete:
// the describe message, or ask.end - the end of speech is the trigger of a question (PLT-14).
func (c *conn) send(id uuid.UUID, it item) (time.Time, error) {
	switch it.kind {
	case ScenarioDescribe, ScenarioPassive:
		mode, focus := protocol.ModeOnDemand, protocol.FocusAll
		if it.kind == ScenarioPassive {
			mode, focus = protocol.ModePassive, protocol.FocusAhead
		}
		sent := time.Now()
		if err := c.writeMessage(&protocol.Describe{
			RequestID: id,
			Mode:      mode,
			Focus:     focus,
			Image:     protocol.ImageInfo{Width: it.image.Width, Height: it.image.Height, Bytes: len(it.image.Data)},
		}); err != nil {
			return sent, err
		}
		return sent, c.writeFrame(protocol.KindImageJPEG, id, 0, it.image.Data)
	case ScenarioAsk:
		if err := c.writeMessage(&protocol.AskStart{RequestID: id, Trigger: protocol.TriggerHold, WithImage: it.image != nil}); err != nil {
			return time.Now(), err
		}
		if it.image != nil {
			if err := c.writeFrame(protocol.KindImageJPEG, id, 0, it.image.Data); err != nil {
				return time.Now(), err
			}
		}
		chunk := audioChunkMS * audioBytesPerMS
		var seq uint32
		for offset := 0; offset < len(it.question.PCM); offset += chunk {
			end := min(offset+chunk, len(it.question.PCM))
			if err := c.writeFrame(protocol.KindAudioIn, id, seq, it.question.PCM[offset:end]); err != nil {
				return time.Now(), err
			}
			seq++
		}
		sent := time.Now()
		return sent, c.writeMessage(&protocol.AskEnd{RequestID: id, AudioMS: it.question.AudioMS()})
	default:
		return time.Now(), fmt.Errorf(errUnknownKindFormat, it.kind)
	}
}

func (c *conn) writeMessage(message protocol.Message) error {
	data, err := protocol.EncodeMessage(message)
	if err == nil {
		err = c.ws.WriteMessage(websocket.TextMessage, data)
	}
	if err != nil {
		return fmt.Errorf(errWriteFormat, message.MessageType(), err)
	}
	return nil
}

func (c *conn) writeFrame(kind protocol.FrameKind, id uuid.UUID, seq uint32, payload []byte) error {
	data := protocol.EncodeFrame(&protocol.Frame{Kind: kind, RequestID: id, Seq: seq, Payload: payload})
	if err := c.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return fmt.Errorf(errWriteFormat, kind, err)
	}
	return nil
}

// clearStatus is the journal status of passive.clear (PLT-11).
func clearStatus(reason protocol.ClearReason) string {
	switch reason {
	case protocol.ClearSkipped:
		return StatusSkipped
	case protocol.ClearStale:
		return StatusStale
	default:
		return StatusClear
	}
}

// isTransport reports whether err broke the connection rather than rejected one frame or message.
func isTransport(err error) bool {
	var closeErr *websocket.CloseError
	var protocolErr *protocol.Error
	return errors.As(err, &closeErr) || !errors.As(err, &protocolErr)
}

// describeMessage names a message for the diagnostics.
func describeMessage(message protocol.Message) string {
	if message == nil {
		return "a binary frame"
	}
	return string(message.MessageType())
}
