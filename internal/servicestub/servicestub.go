// Package servicestub runs the model services in-process for the tests of the
// pipelines, the sessions and the whole gateway: a detector, an ASR and a TTS on
// httptest with the answers of the reference contracts (testdata/contracts),
// without delays unless a test asks for them.
package servicestub

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/textnorm"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

// Paths of the model service APIs.
const (
	pathDetect     = "/v1/detect"
	pathVocabulary = "/v1/vocabulary"
	pathInfo       = "/v1/info"
	pathSynthesize = "/v1/synthesize"
	pathRecognize  = "/v1/recognize"
)

// Audio of the TTS stub.
const (
	// SampleRate is X-Sample-Rate of the TTS stub.
	SampleRate = 24000
	// ChunkBytes is the size of an audio chunk of the TTS stub.
	ChunkBytes = 960
	// ChunksPerSentence is the number of audio chunks of a sentence.
	ChunksPerSentence = 3
)

// holdLimit keeps a held request from hanging a test forever.
const holdLimit = 10 * time.Second

// ContractPath returns the path of a file of the reference contracts: service is
// objects-detector, asr-service or tts-service.
func ContractPath(service, name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "contracts", service, name)
}

// mustRead reads a contract file; the contracts are part of the repository.
func mustRead(service, name string) []byte {
	data, err := os.ReadFile(ContractPath(service, name))
	if err != nil {
		panic(err)
	}
	return data
}

// Detector is an in-process objects-detector.
type Detector struct {
	// URL is the base URL of the stub.
	URL string

	server     *httptest.Server
	vocabulary []byte
	info       []byte

	mu        sync.Mutex
	response  detector.DetectResponse
	hold      bool
	requests  int
	cancelled int
	arrived   chan struct{}
}

// NewDetector starts the detector stub: /v1/detect answers detect.response.json of the contract.
func NewDetector() *Detector {
	d := &Detector{
		vocabulary: mustRead("objects-detector", "vocabulary.response.json"),
		info:       mustRead("objects-detector", "info.response.json"),
		arrived:    make(chan struct{}, 64),
	}
	if err := json.Unmarshal(mustRead("objects-detector", "detect.response.json"), &d.response); err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(pathDetect, d.detect)
	mux.HandleFunc(pathVocabulary, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, d.vocabulary) })
	mux.HandleFunc(pathInfo, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, d.info) })
	d.server = httptest.NewServer(mux)
	d.URL = d.server.URL
	return d
}

// SetObjects replaces the objects of the answer.
func (d *Detector) SetObjects(objects []detector.Object) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.response.Objects = objects
}

// Objects returns the objects of the answer.
func (d *Detector) Objects() []detector.Object {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]detector.Object(nil), d.response.Objects...)
}

// Hold makes /v1/detect wait until the client gives the request up.
func (d *Detector) Hold(hold bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.hold = hold
}

// Arrived is signalled when a detect request comes; signals beyond its buffer are dropped.
func (d *Detector) Arrived() <-chan struct{} {
	return d.arrived
}

// Requests returns the number of detect requests.
func (d *Detector) Requests() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.requests
}

// Cancelled returns the number of detect requests the client gave up.
func (d *Detector) Cancelled() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.cancelled
}

// Close stops the stub.
func (d *Detector) Close() {
	d.server.CloseClientConnections()
	d.server.Close()
}

func (d *Detector) detect(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)

	d.mu.Lock()
	d.requests++
	hold := d.hold
	body, _ := json.Marshal(&d.response)
	d.mu.Unlock()
	select {
	case d.arrived <- struct{}{}:
	default:
	}

	if hold {
		select {
		case <-r.Context().Done():
			d.mu.Lock()
			d.cancelled++
			d.mu.Unlock()
			return
		case <-time.After(holdLimit):
		}
	}
	writeJSON(w, body)
}

// ASR is an in-process asr-service: /v1/recognize answers recognize.response.json of the
// contract with the text the test sets; an empty text is the answer without speech
// (recognize.silence.response.json).
type ASR struct {
	// URL is the base URL of the stub.
	URL string

	server  *httptest.Server
	info    []byte
	speech  map[string]any
	silence map[string]any

	mu     sync.Mutex
	text   string
	fail   bool
	bodies [][]byte
}

// NewASR starts the ASR stub; it recognizes the text of the contract until SetText.
func NewASR() *ASR {
	a := &ASR{info: mustRead("asr-service", "info.response.json")}
	if err := json.Unmarshal(mustRead("asr-service", "recognize.response.json"), &a.speech); err != nil {
		panic(err)
	}
	if err := json.Unmarshal(mustRead("asr-service", "recognize.silence.response.json"), &a.silence); err != nil {
		panic(err)
	}
	a.text, _ = a.speech["text"].(string)

	mux := http.NewServeMux()
	mux.HandleFunc(pathRecognize, a.recognize)
	mux.HandleFunc(pathInfo, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.info) })
	a.server = httptest.NewServer(mux)
	a.URL = a.server.URL
	return a
}

// SetText sets the recognized text; empty - no speech.
func (a *ASR) SetText(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.text = text
}

// Fail makes every recognition answer 500.
func (a *ASR) Fail(fail bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.fail = fail
}

// Bodies returns the audio of the recognition requests in the order they came.
func (a *ASR) Bodies() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([][]byte(nil), a.bodies...)
}

// Close stops the stub.
func (a *ASR) Close() {
	a.server.CloseClientConnections()
	a.server.Close()
}

func (a *ASR) recognize(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	a.mu.Lock()
	a.bodies = append(a.bodies, body)
	text := a.text
	fail := a.fail
	a.mu.Unlock()

	if fail {
		writeError(w, http.StatusInternalServerError)
		return
	}
	response := a.silence
	if text != "" {
		response = map[string]any{}
		for key, value := range a.speech {
			response[key] = value
		}
		response["text"] = text
		response["normalized"] = textnorm.Normalize(text)
	}
	encoded, _ := json.Marshal(response)
	writeJSON(w, encoded)
}

// TTS is an in-process tts-service: every sentence is ChunksPerSentence chunks of
// ChunkBytes bytes filled with the marker of its text, so the tests can tell whose audio a frame is.
type TTS struct {
	// URL is the base URL of the stub.
	URL string

	server *httptest.Server
	info   []byte

	mu          sync.Mutex
	texts       []string
	times       []time.Time
	delays      map[string]time.Duration
	holdFirst   bool
	fail        bool
	active      int
	maxActive   int
	cancelled   int
	arrived     chan string
	chunkPauses time.Duration
}

// NewTTS starts the TTS stub.
func NewTTS() *TTS {
	t := &TTS{
		info:    mustRead("tts-service", "info.response.json"),
		delays:  map[string]time.Duration{},
		arrived: make(chan string, 64),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(pathSynthesize, t.synthesize)
	mux.HandleFunc(pathInfo, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, t.info) })
	t.server = httptest.NewServer(mux)
	t.URL = t.server.URL
	return t
}

// Marker returns the byte the audio of the text is filled with.
func Marker(text string) byte {
	sum := sha256.Sum256([]byte(text))
	return sum[0]
}

// Delay makes the synthesis of the text wait before the first chunk.
func (t *TTS) Delay(text string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.delays[text] = d
}

// ChunkPause makes the stub wait between the chunks of every sentence.
func (t *TTS) ChunkPause(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.chunkPauses = d
}

// HoldFirst makes the first synthesis send one chunk and wait until the client gives it up.
func (t *TTS) HoldFirst(hold bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.holdFirst = hold
}

// Fail makes every synthesis answer 503.
func (t *TTS) Fail(fail bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.fail = fail
}

// Arrived gives the text of every synthesis request as it comes; texts beyond its buffer are dropped.
func (t *TTS) Arrived() <-chan string {
	return t.arrived
}

// Texts returns the texts of the synthesis requests in the order they came.
func (t *TTS) Texts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append([]string(nil), t.texts...)
}

// Times returns when the synthesis requests came.
func (t *TTS) Times() []time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append([]time.Time(nil), t.times...)
}

// MaxActive returns the largest number of syntheses at a time.
func (t *TTS) MaxActive() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.maxActive
}

// Cancelled returns the number of syntheses the client gave up.
func (t *TTS) Cancelled() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.cancelled
}

// Close stops the stub.
func (t *TTS) Close() {
	t.server.CloseClientConnections()
	t.server.Close()
}

// synthesizeRequest is the part of the TTS-1 request the stub reads.
type synthesizeRequest struct {
	Text string `json:"text"`
}

func (t *TTS) synthesize(w http.ResponseWriter, r *http.Request) {
	var request synthesizeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	t.mu.Lock()
	first := len(t.texts) == 0
	t.texts = append(t.texts, request.Text)
	t.times = append(t.times, time.Now())
	hold := first && t.holdFirst
	fail := t.fail
	delay := t.delays[request.Text]
	pause := t.chunkPauses
	t.active++
	t.maxActive = max(t.maxActive, t.active)
	t.mu.Unlock()
	select {
	case t.arrived <- request.Text:
	default:
	}
	defer func() {
		t.mu.Lock()
		t.active--
		t.mu.Unlock()
	}()

	if fail {
		writeError(w, http.StatusServiceUnavailable)
		return
	}
	if !t.wait(r, delay) {
		return
	}

	w.Header().Set("Content-Type", "audio/L16; rate="+strconv.Itoa(SampleRate)+"; channels=1")
	w.Header().Set("X-Sample-Rate", strconv.Itoa(SampleRate))
	w.Header().Set("X-Audio-Format", "pcm_s16le")
	w.Header().Set("X-Cache", "miss")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	chunk := make([]byte, ChunkBytes)
	for i := range chunk {
		chunk[i] = Marker(request.Text)
	}
	for i := 0; i < ChunksPerSentence; i++ {
		if i > 0 && !t.wait(r, pause) {
			return
		}
		if _, err := w.Write(chunk); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		if hold {
			t.wait(r, holdLimit)
			return
		}
	}
}

// wait pauses for d; false if the client gave the request up meanwhile.
func (t *TTS) wait(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return r.Context().Err() == nil
	}
	select {
	case <-r.Context().Done():
		t.mu.Lock()
		t.cancelled++
		t.mu.Unlock()
		return false
	case <-time.After(d):
		return true
	}
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"busy","message":"queue is full"}}`))
}
