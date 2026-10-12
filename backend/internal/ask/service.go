// Package ask implements POST /ask: a natural-language question is answered by
// an LLM over read-only tools, and every tool call is logged.
//
// The shape is deliberately small: one bounded in-process queue feeding a few
// worker goroutines, one OpenAI-compatible model client, and five GORM-backed
// read tools. No caching, no auth, no separate worker process — the queue keeps
// three questions in flight per API instance, and that is the whole design.
package ask

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

var (
	// ErrUnavailable means /ask cannot run: a required LLM_* variable is
	// missing, or the service is shutting down. The message names the variable.
	ErrUnavailable = errors.New("ask is unavailable")
	// ErrBusy means the queue stayed full past the wait budget.
	ErrBusy = errors.New("ask queue is full")
	// ErrModel means the model call itself failed (transport, API, empty or
	// unfinished response).
	ErrModel = errors.New("model call failed")
)

// Config is the /ask slice of the environment.
type Config struct {
	APIKey      string
	Model       string
	BaseURL     string
	Concurrency int
	QueueDepth  int
	QueueWait   time.Duration
}

// missing names the first required variable that is not set. No default URL
// ships with the binary on purpose: the inference endpoint comes from the
// environment, never from the repo.
func (c Config) missing() string {
	switch {
	case strings.TrimSpace(c.APIKey) == "":
		return "LLM_API_KEY"
	case strings.TrimSpace(c.BaseURL) == "":
		return "LLM_BASE_URL"
	case strings.TrimSpace(c.Model) == "":
		return "LLM_MODEL"
	}
	return ""
}

type job struct {
	ctx      context.Context
	question string
	queuedAt time.Time
	reply    chan jobResult
}

type jobResult struct {
	answer Answer
	err    error
}

// Service owns the queue, the workers and the model client.
type Service struct {
	cfg         Config
	unavailable string // non-empty → every Ask fails fast with ErrUnavailable
	db          *readDB
	model       llms.Model
	defs        []llms.Tool

	jobs      chan *job
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// active holds the cancel func of every job the service has accepted, so
	// Close can abort work in flight.
	mu     sync.Mutex
	active map[*job]context.CancelFunc
}

// New builds the service. A missing LLM_* variable is not an error: the
// returned service simply reports itself unavailable and /ask answers 503,
// naming the variable. Errors here are real boot failures (database or client
// setup), and the caller should treat them as fatal.
func New(cfg Config, databaseURL string) (*Service, error) {
	s := &Service{cfg: cfg, defs: readToolDefs()}
	if missing := cfg.missing(); missing != "" {
		s.unavailable = missing + " is not configured"
		return s, nil
	}
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	if cfg.Concurrency < 1 {
		return nil, fmt.Errorf("ask concurrency must be at least 1, got %d", cfg.Concurrency)
	}
	if cfg.QueueDepth < 0 {
		return nil, fmt.Errorf("ask queue depth must not be negative, got %d", cfg.QueueDepth)
	}
	if cfg.QueueWait <= 0 {
		return nil, fmt.Errorf("ask queue wait must be positive, got %s", cfg.QueueWait)
	}

	readDB, err := openReadDB(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("ask read database: %w", err)
	}
	s.db = readDB

	model, err := openai.New(
		openai.WithToken(cfg.APIKey),
		openai.WithBaseURL(cfg.BaseURL),
		openai.WithModel(cfg.Model),
	)
	if err != nil {
		_ = readDB.Close()
		return nil, fmt.Errorf("ask model client: %w", err)
	}
	s.model = model

	s.start()
	log.Printf("ask: model=%s concurrency=%d queue_depth=%d queue_wait=%s", cfg.Model, cfg.Concurrency, cfg.QueueDepth, cfg.QueueWait)
	return s, nil
}

// Unavailable returns a service that refuses every question with
// ErrUnavailable. It lets a failure in the optional /ask subsystem (a bad ASK_*
// value, an unreachable read pool) degrade that one endpoint instead of taking
// the API's writing endpoints down with it.
func Unavailable(reason string) *Service {
	return &Service{unavailable: reason}
}

// Disabled returns why /ask cannot run, or "" when it is live.
func (s *Service) Disabled() string { return s.unavailable }

func (s *Service) start() {
	s.jobs = make(chan *job, s.cfg.QueueDepth)
	s.closed = make(chan struct{})
	for i := 0; i < s.cfg.Concurrency; i++ {
		s.wg.Add(1)
		go s.worker()
	}
}

// Ask enqueues the question and blocks for its answer. It returns ErrBusy when
// no worker picks the job up within the wait budget, and ctx.Err() when the
// caller (or the /ask timeout) gives up first.
func (s *Service) Ask(ctx context.Context, question string) (Answer, error) {
	if s.unavailable != "" {
		return Answer{}, fmt.Errorf("%w: %s", ErrUnavailable, s.unavailable)
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return Answer{}, errors.New("question must not be empty")
	}

	// The job's own context is cancellable so Close can abort work in flight
	// instead of waiting out a model loop that nobody is collecting any more.
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()

	j := &job{ctx: jobCtx, question: question, queuedAt: time.Now(), reply: make(chan jobResult, 1)}
	s.track(j, cancelJob)

	wait := time.NewTimer(s.cfg.QueueWait)
	defer wait.Stop()
	select {
	case s.jobs <- j:
	case <-wait.C:
		return Answer{}, fmt.Errorf("%w: %d questions in flight, waited %s", ErrBusy, s.cfg.Concurrency, s.cfg.QueueWait)
	case <-s.closed:
		return Answer{}, fmt.Errorf("%w: shutting down", ErrUnavailable)
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}

	select {
	case result := <-j.reply:
		return result.answer, result.err
	case <-ctx.Done():
		// The answer may have landed in the same instant the deadline fired; a
		// finished answer beats the timeout.
		select {
		case result := <-j.reply:
			return result.answer, result.err
		default:
		}
		return Answer{}, ctx.Err()
	}
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.closed:
			return
		case j := <-s.jobs:
			s.serve(j)
		}
	}
}

func (s *Service) serve(j *job) {
	defer s.untrack(j)

	started := time.Now()
	if err := j.ctx.Err(); err != nil {
		j.reply <- jobResult{err: err} // client already gave up
		return
	}

	answer, err := s.runSafely(j)
	if err == nil {
		answer.Queued = started.Sub(j.queuedAt)
	}
	s.logf(j.ctx, "job queued=%s run=%s err=%v", started.Sub(j.queuedAt).Round(time.Millisecond), time.Since(started).Round(time.Millisecond), err)
	j.reply <- jobResult{answer: answer, err: err}
}

// runSafely keeps a panicking question from taking the worker (and the other
// two in-flight jobs) down with it.
func (s *Service) runSafely(j *job) (answer Answer, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logf(j.ctx, "panic in worker: %v", r)
			err = fmt.Errorf("ask worker panic: %v", r)
		}
	}()
	return s.run(j.ctx, j.question)
}

// Close stops the service. Queued questions are answered with ErrUnavailable
// and in-flight ones are cancelled, so nothing is ever left waiting for a
// worker that no longer exists and shutdown does not have to outwait a model.
func (s *Service) Close(ctx context.Context) error {
	if s.unavailable != "" {
		return nil
	}
	s.closeOnce.Do(func() { close(s.closed) })

	// Abort work in flight, then refuse whatever is still queued.
	s.cancelActive()
	s.drain()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// A worker is still unwinding; closing the pool below makes any
		// outstanding query fail instead of hanging.
	}
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// track registers a job so Close can cancel it.
func (s *Service) track(j *job, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		s.active = make(map[*job]context.CancelFunc)
	}
	s.active[j] = cancel
}

func (s *Service) untrack(j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, j)
}

func (s *Service) cancelActive() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, cancel := range s.active {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// drain answers anything still sitting in the queue: once Close has run, no
// worker is guaranteed to pick it up.
func (s *Service) drain() {
	for {
		select {
		case j := <-s.jobs:
			s.untrack(j)
			j.reply <- jobResult{err: fmt.Errorf("%w: shutting down", ErrUnavailable)}
		default:
			return
		}
	}
}

type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID attaches the HTTP request id so every tool-call log line can
// be traced back to the request that caused it.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func requestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok && id != "" {
		return id
	}
	return "-"
}
