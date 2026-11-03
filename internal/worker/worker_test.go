package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
)

type updateRecord struct {
	Timestamp time.Time
	Job       board.Job
}

type mockStore struct {
	mu            sync.Mutex
	jobs          map[string]board.Job
	updates       []updateRecord
	updateHook    func(job board.Job)
	runningJobsFn func(ctx context.Context) ([]board.Job, error)
	updateJobFn   func(ctx context.Context, job board.Job) error
}

func newMockStore() *mockStore {
	return &mockStore{
		jobs:    make(map[string]board.Job),
		updates: make([]updateRecord, 0),
	}
}

func (m *mockStore) CreateUser(ctx context.Context, email, passwordHash string) (board.User, error) {
	return board.User{}, errors.New("not implemented")
}

func (m *mockStore) UserByEmail(ctx context.Context, email string) (board.User, error) {
	return board.User{}, errors.New("not implemented")
}

func (m *mockStore) CreateSession(ctx context.Context, userID, token string, expires time.Time) error {
	return errors.New("not implemented")
}

func (m *mockStore) DeleteSession(ctx context.Context, token string) error {
	return errors.New("not implemented")
}

func (m *mockStore) UserByToken(ctx context.Context, token string) (board.User, error) {
	return board.User{}, errors.New("not implemented")
}

func (m *mockStore) ListJobs(ctx context.Context, userID string) ([]board.Job, error) {
	return nil, errors.New("not implemented")
}

func (m *mockStore) CreateJob(ctx context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	jCopy := cloneJob(job)
	m.jobs[job.ID] = jCopy
	return nil
}

func (m *mockStore) Job(ctx context.Context, id string) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return board.Job{}, board.ErrNotFound
	}
	return cloneJob(j), nil
}

func (m *mockStore) UpdateJob(ctx context.Context, job board.Job) error {
	m.mu.Lock()
	if m.updateJobFn != nil {
		fn := m.updateJobFn
		m.mu.Unlock()
		return fn(ctx, job)
	}

	jCopy := cloneJob(job)
	m.jobs[job.ID] = jCopy
	m.updates = append(m.updates, updateRecord{
		Timestamp: time.Now(),
		Job:       jCopy,
	})
	hook := m.updateHook
	m.mu.Unlock()

	if hook != nil {
		hook(jCopy)
	}
	return nil
}

func (m *mockStore) RunningJobs(ctx context.Context) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runningJobsFn != nil {
		return m.runningJobsFn(ctx)
	}
	var res []board.Job
	for _, j := range m.jobs {
		if j.Status == board.StatusRunning {
			res = append(res, cloneJob(j))
		}
	}
	return res, nil
}

func (m *mockStore) getUpdates() []updateRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]updateRecord, len(m.updates))
	copy(cp, m.updates)
	return cp
}

func cloneJob(j board.Job) board.Job {
	cp := j
	cp.Checklist = make([]board.Step, len(j.Checklist))
	copy(cp.Checklist, j.Checklist)
	return cp
}

// writeBrochure helper creates a dummy brochure file in the expected path
func writeBrochure(t *testing.T, dataDir, userID, jobID string, content []byte) {
	t.Helper()
	dir := filepath.Join(dataDir, userID, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create brochure dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "body"), content, 0o644); err != nil {
		t.Fatalf("failed to write brochure body: %v", err)
	}
}

// 1. Test DB Retry Logic
func TestWorker_DBRetry(t *testing.T) {
	t.Run("SuccessAfterRetries", func(t *testing.T) {
		attempts := 0
		opener := func() (string, error) {
			attempts++
			if attempts < 3 {
				return "", errors.New("connection refused")
			}
			return "connected", nil
		}

		res, err := Retry(context.Background(), 1*time.Second, 5*time.Millisecond, opener)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if res != "connected" {
			t.Fatalf("expected 'connected', got %q", res)
		}
		if attempts != 3 {
			t.Fatalf("expected 3 attempts, got %d", attempts)
		}
	})

	t.Run("TimeoutExceeded", func(t *testing.T) {
		attempts := 0
		expectedErr := errors.New("db down")
		opener := func() (string, error) {
			attempts++
			return "", expectedErr
		}

		_, err := Retry(context.Background(), 25*time.Millisecond, 5*time.Millisecond, opener)
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %v, got %v", expectedErr, err)
		}
		if attempts < 2 {
			t.Fatalf("expected multiple attempts before timeout, got %d", attempts)
		}
	})

	t.Run("ContextCancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		opener := func() (string, error) {
			attempts++
			if attempts == 2 {
				cancel()
			}
			return "", errors.New("transient")
		}

		_, err := Retry(ctx, 1*time.Second, 5*time.Millisecond, opener)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("ImmediateSuccess", func(t *testing.T) {
		attempts := 0
		opener := func() (int, error) {
			attempts++
			return 42, nil
		}

		val, err := Retry(context.Background(), 100*time.Millisecond, 10*time.Millisecond, opener)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if val != 42 || attempts != 1 {
			t.Fatalf("expected val=42, attempts=1; got val=%d, attempts=%d", val, attempts)
		}
	})
}

// 2. Test Missing or Unreadable Brochure
func TestWorker_MissingBrochure(t *testing.T) {
	st := newMockStore()
	dataDir := t.TempDir()

	job := board.Job{
		ID:        "job-missing",
		UserID:    "user-1",
		Title:     "Penthouse",
		Address:   "1 Main St",
		Status:    board.StatusRunning,
		RunID:     "run-101",
		Filename:  "brochure.pdf",
		Checklist: board.NewChecklist(),
	}
	_ = st.CreateJob(context.Background(), job)

	w := New(Config{
		Store:     st,
		DataDir:   dataDir,
		StepPause: 10 * time.Millisecond,
	})

	err := w.ProcessJob(context.Background(), job)
	if err != nil {
		t.Fatalf("ProcessJob returned unexpected error: %v", err)
	}

	updated, err := st.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}

	if updated.Status != board.StatusFailed {
		t.Errorf("expected status=%s, got %s", board.StatusFailed, updated.Status)
	}
	if updated.Error != "Brochure is missing" {
		t.Errorf("expected error='Brochure is missing', got %q", updated.Error)
	}
	if updated.RunID != "run-101" {
		t.Errorf("expected runId preserved 'run-101', got %q", updated.RunID)
	}
	// Checklist steps should not have been completed
	for i, step := range updated.Checklist {
		if step.State != board.StepStatePending {
			t.Errorf("expected step %d state to be pending, got %s", i, step.State)
		}
	}
}

func TestWorker_UnreadableBrochure(t *testing.T) {
	st := newMockStore()
	dataDir := t.TempDir()

	job := board.Job{
		ID:        "job-unreadable",
		UserID:    "user-2",
		Title:     "Loft",
		Address:   "2 Main St",
		Status:    board.StatusRunning,
		RunID:     "run-102",
		Filename:  "brochure.pdf",
		Checklist: board.NewChecklist(),
	}
	_ = st.CreateJob(context.Background(), job)

	// Create body as a directory, which makes os.ReadFile fail reliably across platforms
	bodyDir := filepath.Join(dataDir, job.UserID, job.ID, "body")
	if err := os.MkdirAll(bodyDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	w := New(Config{
		Store:     st,
		DataDir:   dataDir,
		StepPause: 10 * time.Millisecond,
	})

	err := w.ProcessJob(context.Background(), job)
	if err != nil {
		t.Fatalf("ProcessJob returned unexpected error: %v", err)
	}

	updated, err := st.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}

	if updated.Status != board.StatusFailed {
		t.Errorf("expected status=%s, got %s", board.StatusFailed, updated.Status)
	}
	if updated.Error != "Brochure is missing" {
		t.Errorf("expected error='Brochure is missing', got %q", updated.Error)
	}
}

// 3. Test Step-by-Step Sequential Processing with Pause and Final Status="success"
func TestWorker_SequentialStepProcessing(t *testing.T) {
	st := newMockStore()
	dataDir := t.TempDir()

	job := board.Job{
		ID:        "job-seq",
		UserID:    "user-3",
		Title:     "Suburban House",
		Address:   "3 Elm St",
		Status:    board.StatusRunning,
		RunID:     "run-seq-1",
		Filename:  "brochure.txt",
		Checklist: board.NewChecklist(),
	}
	_ = st.CreateJob(context.Background(), job)
	writeBrochure(t, dataDir, job.UserID, job.ID, []byte("valid brochure contents"))

	stepPause := 20 * time.Millisecond
	w := New(Config{
		Store:     st,
		DataDir:   dataDir,
		StepPause: stepPause,
	})

	startTime := time.Now()
	err := w.ProcessJob(context.Background(), job)
	if err != nil {
		t.Fatalf("ProcessJob returned unexpected error: %v", err)
	}

	updates := st.getUpdates()
	// We expect 4 updates:
	// Update 0: Step 0 done, status running
	// Update 1: Step 1 done, status running
	// Update 2: Step 2 done, status running
	// Update 3: status success
	if len(updates) != 4 {
		t.Fatalf("expected exactly 4 updates, got %d", len(updates))
	}

	// Verify Update 0 (Step 0 done)
	if updates[0].Job.Checklist[0].State != board.StepStateDone ||
		updates[0].Job.Checklist[1].State != board.StepStatePending ||
		updates[0].Job.Checklist[2].State != board.StepStatePending ||
		updates[0].Job.Status != board.StatusRunning {
		t.Errorf("unexpected state in update 0: %+v", updates[0].Job)
	}

	// Verify Update 1 (Step 1 done)
	if updates[1].Job.Checklist[0].State != board.StepStateDone ||
		updates[1].Job.Checklist[1].State != board.StepStateDone ||
		updates[1].Job.Checklist[2].State != board.StepStatePending ||
		updates[1].Job.Status != board.StatusRunning {
		t.Errorf("unexpected state in update 1: %+v", updates[1].Job)
	}

	// Verify Update 2 (Step 2 done)
	if updates[2].Job.Checklist[0].State != board.StepStateDone ||
		updates[2].Job.Checklist[1].State != board.StepStateDone ||
		updates[2].Job.Checklist[2].State != board.StepStateDone ||
		updates[2].Job.Status != board.StatusRunning {
		t.Errorf("unexpected state in update 2: %+v", updates[2].Job)
	}

	// Verify Update 3 (Final status success)
	if updates[3].Job.Status != board.StatusSuccess {
		t.Errorf("expected final status=%s, got %s", board.StatusSuccess, updates[3].Job.Status)
	}

	// Verify pauses occurred between steps (>= stepPause with tolerance)
	delta01 := updates[1].Timestamp.Sub(updates[0].Timestamp)
	delta12 := updates[2].Timestamp.Sub(updates[1].Timestamp)
	if delta01 < stepPause-5*time.Millisecond {
		t.Errorf("expected pause between step 0 and 1 >= %v, got %v", stepPause, delta01)
	}
	if delta12 < stepPause-5*time.Millisecond {
		t.Errorf("expected pause between step 1 and 2 >= %v, got %v", stepPause, delta12)
	}

	// Total elapsed time must be at least 2 * stepPause
	totalElapsed := time.Since(startTime)
	if totalElapsed < 2*stepPause-10*time.Millisecond {
		t.Errorf("expected total elapsed >= %v, got %v", 2*stepPause, totalElapsed)
	}

	// Verify final job state in store
	finalJob, err := st.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("failed to get final job: %v", err)
	}
	if finalJob.Status != board.StatusSuccess {
		t.Errorf("expected status=success, got %s", finalJob.Status)
	}
	if finalJob.RunID != "run-seq-1" {
		t.Errorf("expected runId preserved, got %s", finalJob.RunID)
	}
}

// 4. Test Crash Recovery
func TestWorker_CrashRecovery(t *testing.T) {
	t.Run("ResumeFromStep1WithStep0AlreadyDone", func(t *testing.T) {
		st := newMockStore()
		dataDir := t.TempDir()

		checklist := board.NewChecklist()
		checklist[0].State = board.StepStateDone // Step 0 was already completed before crash

		job := board.Job{
			ID:        "job-crash-1",
			UserID:    "user-crash",
			Title:     "Beach House",
			Address:   "4 Ocean Ave",
			Status:    board.StatusRunning,
			RunID:     "original-run-id-999",
			Filename:  "brochure.pdf",
			Checklist: checklist,
		}
		_ = st.CreateJob(context.Background(), job)
		writeBrochure(t, dataDir, job.UserID, job.ID, []byte("%PDF-dummy-content"))

		w := New(Config{
			Store:     st,
			DataDir:   dataDir,
			StepPause: 10 * time.Millisecond,
		})

		err := w.ProcessJob(context.Background(), job)
		if err != nil {
			t.Fatalf("ProcessJob returned unexpected error: %v", err)
		}

		updates := st.getUpdates()
		// Since step 0 was already done, it must not repeat step 0.
		// Updates:
		// Update 0: Step 1 done (status running)
		// Update 1: Step 2 done (status running)
		// Update 2: Status success
		if len(updates) != 3 {
			t.Fatalf("expected exactly 3 updates, got %d", len(updates))
		}

		if updates[0].Job.Checklist[1].State != board.StepStateDone {
			t.Errorf("expected update 0 to mark step 1 done, got %+v", updates[0].Job.Checklist)
		}
		if updates[1].Job.Checklist[2].State != board.StepStateDone {
			t.Errorf("expected update 1 to mark step 2 done, got %+v", updates[1].Job.Checklist)
		}
		if updates[2].Job.Status != board.StatusSuccess {
			t.Errorf("expected update 2 to set status success, got %s", updates[2].Job.Status)
		}

		// Verify runId is completely unchanged
		for i, u := range updates {
			if u.Job.RunID != "original-run-id-999" {
				t.Errorf("update %d changed runId to %s", i, u.Job.RunID)
			}
		}

		finalJob, _ := st.Job(context.Background(), job.ID)
		if finalJob.Status != board.StatusSuccess {
			t.Errorf("expected status=success, got %s", finalJob.Status)
		}
		if finalJob.RunID != "original-run-id-999" {
			t.Errorf("expected runId to remain original, got %s", finalJob.RunID)
		}
	})

	t.Run("ResumeWhenAllStepsAlreadyDone", func(t *testing.T) {
		st := newMockStore()
		dataDir := t.TempDir()

		checklist := board.NewChecklist()
		for i := range checklist {
			checklist[i].State = board.StepStateDone
		}

		job := board.Job{
			ID:        "job-crash-all-done",
			UserID:    "user-crash",
			Title:     "Townhouse",
			Address:   "5 Pine St",
			Status:    board.StatusRunning,
			RunID:     "all-done-run-id",
			Filename:  "brochure.pdf",
			Checklist: checklist,
		}
		_ = st.CreateJob(context.Background(), job)
		writeBrochure(t, dataDir, job.UserID, job.ID, []byte("%PDF-townhouse"))

		w := New(Config{
			Store:     st,
			DataDir:   dataDir,
			StepPause: 10 * time.Millisecond,
		})

		err := w.ProcessJob(context.Background(), job)
		if err != nil {
			t.Fatalf("ProcessJob returned unexpected error: %v", err)
		}

		updates := st.getUpdates()
		if len(updates) != 1 {
			t.Fatalf("expected 1 update (marking success), got %d", len(updates))
		}
		if updates[0].Job.Status != board.StatusSuccess {
			t.Errorf("expected status=success, got %s", updates[0].Job.Status)
		}
		if updates[0].Job.RunID != "all-done-run-id" {
			t.Errorf("expected runId preserved, got %s", updates[0].Job.RunID)
		}
	})
}

// 5. Test Graceful Shutdown During Step Pause
func TestWorker_GracefulShutdown_DuringPause(t *testing.T) {
	st := newMockStore()
	dataDir := t.TempDir()

	job := board.Job{
		ID:        "job-shutdown",
		UserID:    "user-shut",
		Title:     "Studio",
		Address:   "6 Oak St",
		Status:    board.StatusRunning,
		RunID:     "shut-run-1",
		Filename:  "brochure.txt",
		Checklist: board.NewChecklist(),
	}
	_ = st.CreateJob(context.Background(), job)
	writeBrochure(t, dataDir, job.UserID, job.ID, []byte("valid brochure"))

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel context as soon as step 0 is saved
	st.updateHook = func(j board.Job) {
		if j.Checklist[0].State == board.StepStateDone {
			cancel()
		}
	}

	w := New(Config{
		Store:     st,
		DataDir:   dataDir,
		StepPause: 1 * time.Second, // Long pause, should be interrupted immediately
	})

	start := time.Now()
	err := w.ProcessJob(ctx, job)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("expected shutdown to interrupt pause quickly, took %v", elapsed)
	}

	// Verify in store: only step 0 was saved; step 1 is still pending, status is still running
	savedJob, err := st.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if savedJob.Checklist[0].State != board.StepStateDone {
		t.Errorf("expected step 0 to be done, got %s", savedJob.Checklist[0].State)
	}
	if savedJob.Checklist[1].State != board.StepStatePending {
		t.Errorf("expected step 1 to still be pending, got %s", savedJob.Checklist[1].State)
	}
	if savedJob.Status != board.StatusRunning {
		t.Errorf("expected status to remain running, got %s", savedJob.Status)
	}
}

// 6. Test Concurrent Processing Across Multiple Accounts
func TestWorker_Run_ConcurrencyAcrossAccounts(t *testing.T) {
	st := newMockStore()
	dataDir := t.TempDir()

	// Create 2 jobs from different users
	job1 := board.Job{
		ID:        "job-user1",
		UserID:    "user-alpha",
		Title:     "House 1",
		Address:   "101 First Ave",
		Status:    board.StatusRunning,
		RunID:     "run-alpha",
		Filename:  "b1.txt",
		Checklist: board.NewChecklist(),
	}
	job2 := board.Job{
		ID:        "job-user2",
		UserID:    "user-beta",
		Title:     "House 2",
		Address:   "202 Second Ave",
		Status:    board.StatusRunning,
		RunID:     "run-beta",
		Filename:  "b2.txt",
		Checklist: board.NewChecklist(),
	}
	_ = st.CreateJob(context.Background(), job1)
	_ = st.CreateJob(context.Background(), job2)
	writeBrochure(t, dataDir, job1.UserID, job1.ID, []byte("content 1"))
	writeBrochure(t, dataDir, job2.UserID, job2.ID, []byte("content 2"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := New(Config{
		Store:        st,
		DataDir:      dataDir,
		StepPause:    20 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- w.Run(ctx)
	}()

	// Wait for both jobs to finish
	deadline := time.Now().Add(2 * time.Second)
	for {
		j1, _ := st.Job(context.Background(), job1.ID)
		j2, _ := st.Job(context.Background(), job2.ID)
		if j1.Status == board.StatusSuccess && j2.Status == board.StatusSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for concurrent jobs to complete; j1=%s, j2=%s", j1.Status, j2.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	err := <-runErrCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from Run, got %v", err)
	}
}

// 7. Test That Poll Does Not Duplicate In-Flight Job Execution
func TestWorker_Run_NoDuplicateExecution(t *testing.T) {
	st := newMockStore()
	dataDir := t.TempDir()

	job := board.Job{
		ID:        "job-dedup",
		UserID:    "user-dedup",
		Title:     "Duplex",
		Address:   "303 Third Ave",
		Status:    board.StatusRunning,
		RunID:     "run-dedup",
		Filename:  "b.txt",
		Checklist: board.NewChecklist(),
	}
	_ = st.CreateJob(context.Background(), job)
	writeBrochure(t, dataDir, job.UserID, job.ID, []byte("content"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := New(Config{
		Store:        st,
		DataDir:      dataDir,
		StepPause:    30 * time.Millisecond,
		PollInterval: 5 * time.Millisecond, // Polls every 5ms, while step pause is 30ms
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- w.Run(ctx)
	}()

	// Wait for job to succeed
	deadline := time.Now().Add(2 * time.Second)
	for {
		j, _ := st.Job(context.Background(), job.ID)
		if j.Status == board.StatusSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for job to finish")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	_ = <-runErrCh

	// If deduplication works, step 0 was saved exactly once, step 1 exactly once, step 2 exactly once
	updates := st.getUpdates()
	step0DoneCount := 0
	for _, u := range updates {
		if u.Job.Checklist[0].State == board.StepStateDone && u.Job.Checklist[1].State == board.StepStatePending {
			step0DoneCount++
		}
	}
	if step0DoneCount != 1 {
		t.Errorf("expected step 0 to be marked done exactly 1 time, got %d", step0DoneCount)
	}
}

// 8. Test Non-Running Job is Skipped
func TestWorker_NonRunningJobSkipped(t *testing.T) {
	st := newMockStore()
	w := New(Config{Store: st, DataDir: t.TempDir()})

	draftJob := board.Job{
		ID:     "draft-1",
		Status: board.StatusDraft,
	}
	err := w.ProcessJob(context.Background(), draftJob)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(st.getUpdates()) != 0 {
		t.Errorf("expected 0 updates for draft job, got %d", len(st.getUpdates()))
	}
}
