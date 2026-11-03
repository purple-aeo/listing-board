package worker

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"listingboard/internal/board"
	"listingboard/internal/store"
)

// Config configures the background Worker daemon.
type Config struct {
	Store        board.Store
	DataDir      string
	StepPause    time.Duration
	PollInterval time.Duration
}

// Worker processes running listing sheets through sequential checklist steps.
type Worker struct {
	store        board.Store
	dataDir      string
	stepPause    time.Duration
	pollInterval time.Duration

	mu      sync.Mutex
	running map[string]struct{}
	wg      sync.WaitGroup
}

// New creates a new background Worker instance.
func New(cfg Config) *Worker {
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if cfg.StepPause < 0 {
		cfg.StepPause = 2 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	return &Worker{
		store:        cfg.Store,
		dataDir:      cfg.DataDir,
		stepPause:    cfg.StepPause,
		pollInterval: cfg.PollInterval,
		running:      make(map[string]struct{}),
	}
}

// ActiveJobs returns the count of currently in-flight jobs.
func (w *Worker) ActiveJobs() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.running)
}

// Run executes the worker polling loop until the given context is cancelled.
// On context cancellation, it ceases polling and waits for all active jobs to exit.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// Initial poll immediately upon startup
	w.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			w.wg.Wait()
			return ctx.Err()
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

func (w *Worker) poll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	jobs, err := w.store.RunningJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("worker: query running jobs error: %v", err)
		}
		return
	}
	for _, job := range jobs {
		w.startJob(ctx, job)
	}
}

func (w *Worker) startJob(ctx context.Context, job board.Job) {
	w.mu.Lock()
	if ctx.Err() != nil {
		w.mu.Unlock()
		return
	}
	if _, exists := w.running[job.ID]; exists {
		w.mu.Unlock()
		return
	}
	w.running[job.ID] = struct{}{}
	w.wg.Add(1)
	w.mu.Unlock()

	go func(j board.Job) {
		defer func() {
			w.mu.Lock()
			delete(w.running, j.ID)
			w.mu.Unlock()
			w.wg.Done()
		}()

		if err := w.ProcessJob(ctx, j); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("worker: error processing job %s: %v", j.ID, err)
			}
		}
	}(job)
}

// ProcessJob processes a single running job:
// 1. Verifies brochure file exists and is readable at {DATA_DIR}/{userID}/{jobID}/body.
//    If missing or unreadable, marks job failed with "Brochure is missing".
// 2. Finds the first pending step, skipping steps already marked done.
// 3. Sequentially updates steps to done, pauses stepPause between steps, and sets status success after the third step.
func (w *Worker) ProcessJob(ctx context.Context, job board.Job) error {
	if job.Status != board.StatusRunning {
		return nil
	}

	// Defensive copy of checklist to avoid sharing slice buffer across routines
	checklist := make([]board.Step, len(job.Checklist))
	copy(checklist, job.Checklist)
	job.Checklist = checklist

	// Verify brochure file exists and is readable
	brochurePath := filepath.Join(w.dataDir, job.UserID, job.ID, "body")
	if _, err := os.ReadFile(brochurePath); err != nil {
		job.Status = board.StatusFailed
		job.Error = "Brochure is missing"
		job.UpdatedAt = time.Now().UTC()
		return w.store.UpdateJob(ctx, job)
	}

	// Find the first pending step
	firstPending := -1
	for i, step := range job.Checklist {
		if step.State == board.StepStatePending {
			firstPending = i
			break
		}
	}

	if firstPending != -1 {
		for i := firstPending; i < len(job.Checklist); i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			job.Checklist[i].State = board.StepStateDone
			job.UpdatedAt = time.Now().UTC()
			if err := w.store.UpdateJob(ctx, job); err != nil {
				return err
			}

			if i < len(job.Checklist)-1 && w.stepPause > 0 {
				t := time.NewTimer(w.stepPause)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					return ctx.Err()
				}
			}
		}
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	job.Status = board.StatusSuccess
	job.UpdatedAt = time.Now().UTC()
	return w.store.UpdateJob(ctx, job)
}

// WaitStore retries opening the database store until timeout or until ctx is cancelled.
func WaitStore(ctx context.Context, dsn string, timeout, interval time.Duration) (*store.Store, error) {
	return Retry(ctx, timeout, interval, func() (*store.Store, error) {
		return store.Open(dsn)
	})
}

// Retry retries fn until it succeeds, timeout expires, or ctx is cancelled.
func Retry[T any](ctx context.Context, timeout, interval time.Duration, fn func() (T, error)) (T, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	var lastErr error

	for {
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		default:
		}

		val, err := fn()
		if err == nil {
			return val, nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			var zero T
			return zero, lastErr
		}

		select {
		case <-time.After(interval):
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}
}
