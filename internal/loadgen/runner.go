package loadgen

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
)

// Texts of the errors, shared by the code and the tests.
const (
	errWarmupFormat = "warmup failed: %w"
	errWarmupBroken = "connection broke during a warmup request"
)

// Config are the parameters of the runs.
type Config struct {
	URL       string        // ws:// or wss:// address of /v1/session
	Token     string        // device token; empty - no Authorization (APP_AUTH_DISABLED=true)
	Scenarios []string      // subset of Scenarios
	Sessions  int           // simultaneous sessions
	Pause     time.Duration // pause between the requests of a session
	Runs      int           // full passes of the manifest
	FirstRun  int           // number of the first run
	Warmup    int           // describe requests before the first run, not recorded
	Timeout   time.Duration // limit of a request from its input to its final message
}

// Runner runs the manifest of a data set against the gateway.
type Runner struct {
	cfg     Config
	dataset *Dataset

	mu        sync.Mutex
	configIDs []string
}

// NewRunner creates a runner of the data set.
func NewRunner(cfg Config, dataset *Dataset) *Runner {
	return &Runner{cfg: cfg, dataset: dataset}
}

// ConfigIDs returns every config_id the gateway told in welcome, in order of appearance.
func (r *Runner) ConfigIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.configIDs)
}

// Warmup sends the warmup requests in one session: describe of the snapshots in turn.
// Their results are not recorded; a failed connection is an error.
func (r *Runner) Warmup(ctx context.Context) error {
	if r.cfg.Warmup == 0 {
		return nil
	}
	c, err := r.dial(ctx)
	if err != nil {
		return fmt.Errorf(errWarmupFormat, err)
	}
	defer c.close()

	for i := 0; i < r.cfg.Warmup && ctx.Err() == nil; i++ {
		image := &r.dataset.Images[i%len(r.dataset.Images)]
		if _, broken := c.do(item{kind: ScenarioDescribe, image: image}); broken {
			return fmt.Errorf(errWarmupFormat, errors.New(errWarmupBroken))
		}
	}
	return ctx.Err()
}

// Run makes one full pass of the manifest with the given number. Every run opens its own
// sessions, so a config_id changed between the runs shows in welcome. The records are
// ordered by the time they were sent. A cancelled ctx stops taking new requests.
func (r *Runner) Run(ctx context.Context, number int) RunResult {
	items := r.items()
	queue := make(chan item, len(items))
	for _, it := range items {
		queue <- it
	}
	close(queue)

	result := RunResult{Run: number, StartedAt: time.Now()}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for range r.cfg.Sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := range r.session(ctx, queue) {
				record.Run = number
				mu.Lock()
				result.Records = append(result.Records, record)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	result.FinishedAt = time.Now()

	sort.SliceStable(result.Records, func(i, j int) bool { return result.Records[i].SentAt < result.Records[j].SentAt })
	return result
}

// session takes requests from the queue one by one with the pause between them and
// yields their records. A broken connection is opened again for the next request.
func (r *Runner) session(ctx context.Context, queue <-chan item) func(yield func(Record) bool) {
	return func(yield func(Record) bool) {
		var c *conn
		defer func() {
			if c != nil {
				c.close()
			}
		}()

		first := true
		for it := range queue {
			if !first && !sleep(ctx, r.cfg.Pause) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			first = false

			if c == nil {
				var err error
				if c, err = r.dial(ctx); err != nil {
					record := failedRecord(it)
					if !yield(record) {
						return
					}
					continue
				}
			}

			record, broken := c.do(it)
			if broken {
				c.close()
				c = nil
			}
			if !yield(record) {
				return
			}
		}
	}
}

// dial opens a session and remembers its config_id.
func (r *Runner) dial(ctx context.Context) (*conn, error) {
	c, err := dial(ctx, r.cfg.URL, r.cfg.Token, r.cfg.Timeout)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if !slices.Contains(r.configIDs, c.configID) {
		r.configIDs = append(r.configIDs, c.configID)
	}
	r.mu.Unlock()
	return c, nil
}

// items is the manifest of one run: describe of every snapshot, ask of every question
// (with its snapshot, if the question refers to one), passive of every snapshot.
func (r *Runner) items() []item {
	items := []item{}
	for _, scenario := range Scenarios {
		if !slices.Contains(r.cfg.Scenarios, scenario) {
			continue
		}
		switch scenario {
		case ScenarioDescribe, ScenarioPassive:
			for i := range r.dataset.Images {
				items = append(items, item{kind: scenario, image: &r.dataset.Images[i]})
			}
		case ScenarioAsk:
			for i := range r.dataset.Questions {
				question := &r.dataset.Questions[i]
				it := item{kind: scenario, question: question}
				if question.ImageID != nil {
					it.image = r.dataset.Image(*question.ImageID)
				}
				items = append(items, it)
			}
		}
	}
	return items
}

// failedRecord is the record of a request that could not get a session.
func failedRecord(it item) Record {
	record := Record{
		Kind:      it.kind,
		SentAt:    formatTime(time.Now()),
		Timings:   map[string]protocol.Millis{},
		Status:    StatusError,
		ErrorCode: CodeConnection,
	}
	if it.image != nil {
		imageID := it.image.ID
		record.ImageID = &imageID
	}
	if it.question != nil {
		questionID := it.question.ID
		record.QuestionID = &questionID
	}
	return record
}

// sleep waits for d; false if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
