package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/pipeline"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// Texts of the protocol errors of the session, shared by the code and the tests.
const (
	errHelloExpected        = "hello is expected before any other message"
	errFrameBeforeHello     = "hello is expected before any frame"
	errHelloRepeated        = "hello is already received"
	errAudioOutFromClient   = "AUDIO_OUT frames are not accepted from the application"
	errUnexpectedImage      = "unexpected IMAGE_JPEG frame: no request waits for a snapshot with this request_id"
	errUnexpectedAudio      = "unexpected AUDIO_IN frame: no question with this request_id is in progress"
	errUnexpectedAskEnd     = "unexpected ask.end: no question with this request_id is in progress"
	errQuestionTooLongFmt   = "question exceeds %d ms"
	errShuttingDownRequests = "server is shutting down"
	msgUnsupportedProtocol  = "unsupported protocol version"
)

// Causes of the end of a request context other than pipeline.ErrCancelled.
var (
	errRequestDone   = errors.New("request done")
	errSessionClosed = errors.New("session closed")
)

// outboundSize is the room of the write queue of a session.
const outboundSize = 256

// readLimitSlack is what a frame may exceed the snapshot limit by and still get
// payload_too_large instead of closing the connection.
const readLimitSlack = 1 << 20

// outbound is a message or a frame on its way to the application.
type outbound struct {
	request     *request // nil - not of a request, never dropped
	messageType int      // websocket.TextMessage, BinaryMessage or CloseMessage
	data        []byte
}

// request is a request of the session from its first message to the end of its pipeline.
type request struct {
	id        uuid.UUID
	kind      string
	ctx       context.Context
	cancel    context.CancelCauseFunc
	startedAt time.Time

	// The fields below belong to the reader goroutine.
	describe     *protocol.Describe
	askStart     *protocol.AskStart
	waitingImage bool
	running      bool
	image        chan []byte
	end          chan pipeline.AskEnd
	audio        []byte
	ended        bool
}

// dropped reports whether the application cancelled the request or the session rejected
// it: its messages and frames are dropped (PROTO-8).
func (r *request) dropped() bool {
	cause := context.Cause(r.ctx)
	return errors.Is(cause, pipeline.ErrCancelled) || errors.Is(cause, pipeline.ErrRejected)
}

// Session is one WebSocket connection of the application.
type Session struct {
	id     uuid.UUID
	server *Server
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	out       chan outbound
	closed    chan struct{}
	closeOnce sync.Once
	active    atomic.Int32 // requests whose pipeline is running

	// hello and ignored belong to the reader goroutine.
	hello   *protocol.Hello
	ignored map[uuid.UUID]bool // skipped passive requests whose snapshot is still to come

	mu      sync.Mutex
	user    *request // describe on_demand or ask
	passive *request // describe passive
}

func newSession(server *Server, conn *websocket.Conn) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		id:      uuid.New(),
		server:  server,
		conn:    conn,
		ctx:     ctx,
		cancel:  cancel,
		out:     make(chan outbound, outboundSize),
		closed:  make(chan struct{}),
		ignored: map[uuid.UUID]bool{},
	}
}

// run serves the session until the connection is closed.
func (s *Session) run() {
	logger.Info("Session opened: session_id=%s", s.id)
	s.conn.SetReadLimit(int64(protocol.FrameHeaderSize + max(s.server.cfg.Limits.MaxImageBytes, s.server.cfg.Limits.MaxAudioChunkBytes) + readLimitSlack))
	go s.write()
	s.startKeepalive()

	for {
		messageType, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				logger.Info("session connection lost: session_id=%s error=%v", s.id, err)
			}
			break
		}
		switch messageType {
		case websocket.TextMessage:
			s.handleMessage(data)
		case websocket.BinaryMessage:
			s.handleFrame(data)
		}
	}

	s.close()
	s.mu.Lock()
	for _, r := range []*request{s.user, s.passive} {
		if r != nil {
			s.drop(r, errSessionClosed)
		}
	}
	s.user, s.passive = nil, nil
	s.mu.Unlock()
	logger.Info("Session closed: session_id=%s", s.id)
}

// close ends the session: stops the writer and the pings, cancels the requests and closes the connection.
func (s *Session) close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.cancel()
		_ = s.conn.Close()
	})
}

// closeWith sends a close frame with the code and closes the connection right away.
func (s *Session) closeWith(code int, text string) {
	_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(writeTimeout))
	s.close()
}

// closeAfterQueue queues a close frame with the code after the messages already queued
// and waits until the writer sends it; if the writer is stuck, the connection is closed anyway.
func (s *Session) closeAfterQueue(code int, text string) {
	s.enqueue(outbound{messageType: websocket.CloseMessage, data: websocket.FormatCloseMessage(code, text)})
	select {
	case <-s.closed:
	case <-time.After(writeTimeout):
		s.closeWith(code, text)
	}
}

// write sends the queued messages and frames in order. A message or a frame of a
// cancelled request is dropped (PROTO-8): the check and the write are in one goroutine,
// so nothing of the request is written after the cancellation is seen.
func (s *Session) write() {
	for {
		select {
		case <-s.closed:
			return
		case item := <-s.out:
			if item.request != nil && item.request.dropped() {
				continue
			}
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := s.conn.WriteMessage(item.messageType, item.data); err != nil {
				logger.Warning("failed to write to session: session_id=%s error=%v", s.id, err)
				s.close()
				return
			}
			if item.messageType == websocket.CloseMessage {
				s.close()
				return
			}
		}
	}
}

// enqueue puts the item in the write queue; it is dropped if the session is closed.
func (s *Session) enqueue(item outbound) {
	select {
	case s.out <- item:
	case <-s.closed:
	}
}

// send queues a message; r is the request it belongs to or nil.
func (s *Session) send(r *request, message protocol.Message) {
	data, err := protocol.EncodeMessage(message)
	if err != nil {
		logger.Error(err)
		return
	}
	s.enqueue(outbound{request: r, messageType: websocket.TextMessage, data: data})
}

// sendError queues the error message; requestID is nil if the error belongs to no request.
func (s *Session) sendError(requestID *uuid.UUID, err error) {
	logger.Warning("session error: session_id=%s code=%s error=%v", s.id, protocol.CodeOf(err), err)
	s.send(nil, protocol.NewErrorMessage(requestID, err))
}

// handleMessage handles a text frame (PROTO-4, PROTO-10).
func (s *Session) handleMessage(data []byte) {
	message, err := protocol.DecodeClientMessage(data)
	if err != nil {
		s.sendError(nil, err)
		if protocol.CodeOf(err) == protocol.CodeUnsupportedProtocol {
			s.enqueue(outbound{
				messageType: websocket.CloseMessage,
				data:        websocket.FormatCloseMessage(CloseUnsupportedProtocol, msgUnsupportedProtocol),
			})
		}
		return
	}

	if s.hello == nil {
		hello, ok := message.(*protocol.Hello)
		if !ok {
			s.sendError(nil, protocol.NewError(protocol.CodeBadRequest, errHelloExpected))
			return
		}
		s.onHello(hello)
		return
	}

	switch m := message.(type) {
	case *protocol.Hello:
		s.sendError(nil, protocol.NewError(protocol.CodeBadRequest, errHelloRepeated))
	case *protocol.Describe:
		s.onDescribe(m)
	case *protocol.AskStart:
		s.onAskStart(m)
	case *protocol.AskEnd:
		s.onAskEnd(m)
	case *protocol.Cancel:
		s.onCancel(m)
	case *protocol.ClientMetrics:
		logger.Debug("client metrics: session_id=%s request_id=%s", s.id, m.RequestID)
	}
}

// onHello answers welcome with the config and config_id (PROTO-5, PROTO-13).
func (s *Session) onHello(hello *protocol.Hello) {
	s.hello = hello
	configID := ""
	if current := s.server.config.Current(); current != nil {
		configID = current.ConfigID
	}
	logger.Info("Session hello: session_id=%s device_id=%s app_version=%s platform=%s network=%s config_id=%s",
		s.id, hello.DeviceID, hello.AppVersion, hello.Platform, hello.Network, configID)

	s.send(nil, &protocol.Welcome{
		SessionID:     s.id,
		ServerVersion: s.server.cfg.ServerVersion,
		ConfigID:      configID,
		Config:        s.server.cfg.Client,
	})
}

// onDescribe starts a describe request; it runs when its snapshot comes (PROTO-6).
func (s *Session) onDescribe(m *protocol.Describe) {
	if m.Mode == protocol.ModePassive {
		s.onPassive(m)
		return
	}

	r := s.newRequest(m.RequestID, pipeline.KindDescribe)
	r.describe = m
	r.waitingImage = true

	s.mu.Lock()
	s.cancelUser()
	s.user = r
	s.mu.Unlock()
}

// onPassive starts a passive request unless a user or a passive request is in progress:
// then the answer is passive.clear with the reason skipped (PROTO-8).
func (s *Session) onPassive(m *protocol.Describe) {
	s.mu.Lock()
	busy := s.user != nil || s.passive != nil
	if !busy {
		r := s.newRequest(m.RequestID, pipeline.KindPassive)
		r.describe = m
		r.waitingImage = true
		s.passive = r
	}
	s.mu.Unlock()

	if busy {
		s.ignored[m.RequestID] = true
		s.server.metrics.Requests.WithLabelValues(pipeline.KindPassive, pipeline.StatusSkipped).Inc()
		s.send(nil, &protocol.PassiveClear{RequestID: m.RequestID, Reason: protocol.ClearSkipped})
	}
}

// onAskStart starts a question right away: the pipeline gets the snapshot as soon as it
// comes and the audio at ask.end (PROTO-7, PLT-3).
func (s *Session) onAskStart(m *protocol.AskStart) {
	r := s.newRequest(m.RequestID, pipeline.KindAsk)
	r.askStart = m
	r.waitingImage = m.WithImage
	r.image = make(chan []byte, 1)
	r.end = make(chan pipeline.AskEnd, 1)
	if !m.WithImage {
		close(r.image)
	}

	s.mu.Lock()
	s.cancelUser()
	s.user = r
	s.mu.Unlock()

	request := &pipeline.AskRequest{
		RequestID: m.RequestID,
		SessionID: s.id.String(),
		DeviceID:  s.hello.DeviceID,
		Trigger:   m.Trigger,
		WithImage: m.WithImage,
		StartedAt: r.startedAt,
		Image:     r.image,
		End:       r.end,
	}
	s.start(r, func(ctx context.Context, sink pipeline.Sink) {
		s.server.pipelines.Ask(ctx, request, sink)
	})
}

// onAskEnd ends the question: accepted, then the audio goes to the pipeline (PROTO-7).
func (s *Session) onAskEnd(m *protocol.AskEnd) {
	r := s.currentUser(m.RequestID, pipeline.KindAsk)
	if r == nil || r.ended {
		s.sendError(&m.RequestID, protocol.NewError(protocol.CodeBadRequest, errUnexpectedAskEnd))
		return
	}
	if m.AudioMS > float64(s.server.cfg.MaxQuestionMS) {
		s.reject(r, protocol.NewError(protocol.CodePayloadTooLarge, errQuestionTooLongFmt, s.server.cfg.MaxQuestionMS))
		return
	}

	r.ended = true
	if r.waitingImage {
		r.waitingImage = false
		close(r.image)
	}
	received := time.Now()
	s.send(r, &protocol.Accepted{RequestID: r.id})
	r.end <- pipeline.AskEnd{Audio: r.audio, AudioMS: m.AudioMS, ReceivedAt: received}
	r.audio = nil
}

// onCancel cancels the request if it is in progress; a finished or unknown one is ignored (PROTO-8).
func (s *Session) onCancel(m *protocol.Cancel) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.user != nil && s.user.id == m.RequestID:
		s.cancelUser()
	case s.passive != nil && s.passive.id == m.RequestID:
		s.drop(s.passive, pipeline.ErrCancelled)
		s.passive = nil
	default:
		logger.Debug("cancel of a request not in progress: session_id=%s request_id=%s", s.id, m.RequestID)
	}
}

// handleFrame handles a binary frame (PROTO-3).
func (s *Session) handleFrame(data []byte) {
	if s.hello == nil {
		s.sendError(nil, protocol.NewError(protocol.CodeBadRequest, errFrameBeforeHello))
		return
	}

	frame, err := protocol.DecodeFrame(data, s.server.cfg.Limits)
	if err != nil {
		if frame == nil {
			s.sendError(nil, err)
			return
		}
		// Over the limit: the request of the frame cannot go on.
		if r := s.find(frame.RequestID); r != nil {
			s.reject(r, err)
			return
		}
		s.sendError(&frame.RequestID, err)
		return
	}

	switch frame.Kind {
	case protocol.KindImageJPEG:
		s.onImage(frame)
	case protocol.KindAudioIn:
		s.onAudio(frame)
	default:
		s.sendError(&frame.RequestID, protocol.NewError(protocol.CodeBadRequest, errAudioOutFromClient))
	}
}

// onImage takes the snapshot of a request: a describe request runs now, a question gets it at once.
func (s *Session) onImage(frame *protocol.Frame) {
	if s.ignored[frame.RequestID] {
		delete(s.ignored, frame.RequestID)
		return
	}
	r := s.find(frame.RequestID)
	if r == nil || !r.waitingImage {
		s.sendError(&frame.RequestID, protocol.NewError(protocol.CodeBadRequest, errUnexpectedImage))
		return
	}
	r.waitingImage = false
	received := time.Now()
	image := frame.Payload

	switch r.kind {
	case pipeline.KindAsk:
		r.image <- image
		close(r.image)
	case pipeline.KindPassive:
		s.send(r, &protocol.Accepted{RequestID: r.id})
		request := &pipeline.PassiveRequest{
			RequestID:  r.id,
			SessionID:  s.id.String(),
			DeviceID:   s.hello.DeviceID,
			Image:      image,
			StartedAt:  r.startedAt,
			ReceivedAt: received,
		}
		s.start(r, func(ctx context.Context, sink pipeline.Sink) {
			s.server.pipelines.Passive(ctx, request, sink)
		})
	default:
		s.send(r, &protocol.Accepted{RequestID: r.id})
		request := &pipeline.DescribeRequest{
			RequestID:  r.id,
			SessionID:  s.id.String(),
			DeviceID:   s.hello.DeviceID,
			Focus:      r.describe.Focus,
			Image:      image,
			StartedAt:  r.startedAt,
			ReceivedAt: received,
		}
		s.start(r, func(ctx context.Context, sink pipeline.Sink) {
			s.server.pipelines.Describe(ctx, request, sink)
		})
	}
}

// onAudio collects a chunk of the question in memory; a question longer than
// max_question_ms is payload_too_large (PROTO-3).
func (s *Session) onAudio(frame *protocol.Frame) {
	r := s.currentUser(frame.RequestID, pipeline.KindAsk)
	if r == nil || r.ended {
		s.sendError(&frame.RequestID, protocol.NewError(protocol.CodeBadRequest, errUnexpectedAudio))
		return
	}
	limit := s.server.cfg.MaxQuestionMS * audioInBytesPerSecond / 1000
	if len(r.audio)+len(frame.Payload) > limit {
		s.reject(r, protocol.NewError(protocol.CodePayloadTooLarge, errQuestionTooLongFmt, s.server.cfg.MaxQuestionMS))
		return
	}
	r.audio = append(r.audio, frame.Payload...)
}

// newRequest creates a request of the session.
func (s *Session) newRequest(id uuid.UUID, kind string) *request {
	ctx, cancel := context.WithCancelCause(s.ctx)
	return &request{
		id:        id,
		kind:      kind,
		ctx:       ctx,
		cancel:    cancel,
		startedAt: time.Now(),
	}
}

// start runs the pipeline of the request. While the server shuts down, no request
// starts: the answer is busy.
func (s *Session) start(r *request, run func(ctx context.Context, sink pipeline.Sink)) {
	if s.server.registry.Closing() {
		s.reject(r, protocol.NewError(protocol.CodeBusy, errShuttingDownRequests))
		return
	}

	r.running = true
	s.active.Add(1)
	go func() {
		defer s.active.Add(-1)

		run(r.ctx, &sink{session: s, request: r})

		s.mu.Lock()
		if s.user == r {
			s.user = nil
		}
		if s.passive == r {
			s.passive = nil
		}
		s.mu.Unlock()
		r.cancel(errRequestDone)
	}()
}

// reject ends the request with an error before or instead of its pipeline.
func (s *Session) reject(r *request, err error) {
	s.mu.Lock()
	if s.user == r {
		s.user = nil
	}
	if s.passive == r {
		s.passive = nil
	}
	s.mu.Unlock()

	running := r.running
	r.cancel(pipeline.ErrRejected)
	if !running {
		s.server.metrics.Requests.WithLabelValues(r.kind, pipeline.StatusError).Inc()
	}
	s.sendError(&r.id, err)
}

// cancelUser cancels the user request in progress (PROTO-8). s.mu must be held.
func (s *Session) cancelUser() {
	if s.user != nil {
		s.drop(s.user, pipeline.ErrCancelled)
		s.user = nil
	}
}

// drop ends the request with the cause. A request whose pipeline is not running yet is
// counted here; a running pipeline counts itself.
func (s *Session) drop(r *request, cause error) {
	if r.ctx.Err() != nil {
		return
	}
	r.cancel(cause)
	if !r.running {
		s.server.metrics.Requests.WithLabelValues(r.kind, pipeline.StatusCancelled).Inc()
	}
	if errors.Is(cause, pipeline.ErrCancelled) {
		logger.Info("request cancelled by the application: session_id=%s request_id=%s kind=%s", s.id, r.id, r.kind)
	}
}

// find returns the request in progress with the id.
func (s *Session) find(id uuid.UUID) *request {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.user != nil && s.user.id == id {
		return s.user
	}
	if s.passive != nil && s.passive.id == id {
		return s.passive
	}
	return nil
}

// currentUser returns the user request in progress if it has the id and the kind.
func (s *Session) currentUser(id uuid.UUID, kind string) *request {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.user != nil && s.user.id == id && s.user.kind == kind {
		return s.user
	}
	return nil
}

// sink sends the messages and the frames of one request through the write queue.
type sink struct {
	session *Session
	request *request
}

// Send queues a message of the request.
func (k *sink) Send(message protocol.Message) {
	k.session.send(k.request, message)
}

// SendAudio queues an AUDIO_OUT frame of the request.
func (k *sink) SendAudio(seq uint32, payload []byte) {
	data := protocol.EncodeFrame(&protocol.Frame{
		Kind:      protocol.KindAudioOut,
		RequestID: k.request.id,
		Seq:       seq,
		Payload:   payload,
	})
	k.session.enqueue(outbound{request: k.request, messageType: websocket.BinaryMessage, data: data})
}
