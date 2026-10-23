package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/store"
	"github.com/robfig/cron/v3"
)

// JobRunner executes a sync for one source in the given mode and returns
// (rowsIn, rowsUpserted, err). The reporter is bound to the in-flight
// sync_runs row; jobs should call it as they cross meaningful stages or
// after every batch so operators can see progress in the runs API.
type JobRunner func(ctx context.Context, mode string, report store.ProgressReporter) (int, int, error)

// ArgJobRunner is a JobRunner that also receives a free-form `arg`
// string supplied at trigger time — for parameterised, on-demand
// sources (e.g. "re-enrich this one project"). Kept as a separate
// runner type so the batch jobs keep their arg-free JobRunner signature.
type ArgJobRunner func(ctx context.Context, mode, arg string, report store.ProgressReporter) (int, int, error)

// Job describes one ingestion job — its source name, default cron, and
// runner. A job sets exactly one of Run / RunArg: Run for the cron-style
// whole-dataset syncs, RunArg for on-demand sources that take a
// per-trigger argument (e.g. a project name).
type Job struct {
	Source string
	Cron   string
	Run    JobRunner
	// RunArg, when set, replaces Run and receives the trigger-time arg.
	// A job with RunArg should generally leave Cron empty — there's no
	// meaningful argument for an unattended scheduled firing.
	RunArg ArgJobRunner
}

type Scheduler struct {
	store *store.Store
	cron  *cron.Cron
	jobs  map[string]Job

	mu     sync.Mutex
	active map[string]int64 // source -> run_id currently running
}

func New(s *store.Store) *Scheduler {
	return &Scheduler{
		store:  s,
		cron:   cron.New(),
		jobs:   map[string]Job{},
		active: map[string]int64{},
	}
}

func (s *Scheduler) Register(j Job) error {
	if _, exists := s.jobs[j.Source]; exists {
		return fmt.Errorf("duplicate source: %s", j.Source)
	}
	if (j.Run == nil) == (j.RunArg == nil) {
		return fmt.Errorf("source %s: set exactly one of Run / RunArg", j.Source)
	}
	s.jobs[j.Source] = j
	if j.Cron != "" {
		src := j.Source
		_, err := s.cron.AddFunc(j.Cron, func() {
			if _, err := s.Trigger(context.Background(), src, store.ModeIncremental); err != nil {
				slog.Warn("scheduled run failed", "source", src, "err", err)
			}
		})
		if err != nil {
			return fmt.Errorf("register cron for %s: %w", j.Source, err)
		}
	}
	return nil
}

// progressReporter returns a ProgressReporter bound to runID that throttles DB
// writes: updates fire at most once per minInterval, except for stage changes
// and the terminal 100%, which always flush immediately so operators see them.
func (s *Scheduler) progressReporter(runID int64) store.ProgressReporter {
	const minInterval = 2 * time.Second
	var (
		mu        sync.Mutex
		lastAt    time.Time
		lastStage string
	)
	return func(stage string, progress float64) {
		mu.Lock()
		now := time.Now()
		if stage == lastStage && progress < 100 && now.Sub(lastAt) < minInterval {
			mu.Unlock()
			return
		}
		lastAt = now
		lastStage = stage
		mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.UpdateProgress(ctx, runID, stage, progress); err != nil {
			slog.Warn("sync progress update failed", "run_id", runID, "err", err)
		}
	}
}

func (s *Scheduler) Start() { s.cron.Start() }
func (s *Scheduler) Stop()  { <-s.cron.Stop().Done() }

// WaitDone blocks until source is no longer running (or ctx is cancelled).
// Must be called after Trigger so the source is already in the active map.
func (s *Scheduler) WaitDone(ctx context.Context, source string) {
	for {
		s.mu.Lock()
		_, running := s.active[source]
		s.mu.Unlock()
		if !running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Scheduler) Sources() []string {
	out := make([]string, 0, len(s.jobs))
	for k := range s.jobs {
		out = append(out, k)
	}
	return out
}

// Trigger runs a job asynchronously. Returns the new sync_runs id immediately.
// If the source is already running, returns store.ErrLocked.
func (s *Scheduler) Trigger(ctx context.Context, source, mode string) (int64, error) {
	return s.TriggerArg(ctx, source, mode, "")
}

// TriggerArg is Trigger with a free-form argument forwarded to a
// RunArg job. For a plain Run job the arg is ignored. Same async +
// advisory-lock + already-running semantics as Trigger.
func (s *Scheduler) TriggerArg(ctx context.Context, source, mode, arg string) (int64, error) {
	job, ok := s.jobs[source]
	if !ok {
		return 0, fmt.Errorf("unknown source: %s", source)
	}
	if mode == "" {
		mode = store.ModeIncremental
	}

	s.mu.Lock()
	if _, busy := s.active[source]; busy {
		s.mu.Unlock()
		return 0, store.ErrLocked
	}
	// Reserve the slot before starting the goroutine so two quick triggers
	// can't both pass the check.
	s.active[source] = -1
	s.mu.Unlock()

	id, err := s.store.StartRun(ctx, source, mode)
	if err != nil {
		s.mu.Lock()
		delete(s.active, source)
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Lock()
	s.active[source] = id
	s.mu.Unlock()

	go s.runJob(job, id, mode, arg)
	return id, nil
}

func (s *Scheduler) runJob(job Job, runID int64, mode, arg string) {
	defer func() {
		s.mu.Lock()
		delete(s.active, job.Source)
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	slog.Info("sync start", "source", job.Source, "mode", mode, "run_id", runID)
	report := s.progressReporter(runID)
	var rowsIn, rowsUp int
	err := s.store.WithLock(ctx, job.Source, func(ctx context.Context) error {
		var (
			in, up int
			runErr error
		)
		if job.RunArg != nil {
			in, up, runErr = job.RunArg(ctx, mode, arg, report)
		} else {
			in, up, runErr = job.Run(ctx, mode, report)
		}
		rowsIn, rowsUp = in, up
		return runErr
	})
	// ErrLocked here means another process / goroutine won the advisory
	// lock race — the job never executed in this goroutine. Mark the row
	// as 'skipped' (not 'error') so the run history doesn't show a false
	// failure when a duplicate engine instance is in flight.
	if errors.Is(err, store.ErrLocked) {
		slog.Warn("sync skipped (advisory lock held)", "source", job.Source, "run_id", runID)
		if finErr := s.store.MarkSkipped(context.Background(), runID, "another process held the advisory lock"); finErr != nil {
			slog.Warn("sync skipped: update failed", "source", job.Source, "err", finErr)
		}
		return
	}
	if finErr := s.store.FinishRun(context.Background(), runID, rowsIn, rowsUp, err); finErr != nil {
		slog.Warn("sync finish update failed", "source", job.Source, "err", finErr)
	}
	slog.Info("sync done",
		"source", job.Source, "mode", mode, "run_id", runID,
		"rows_in", rowsIn, "rows_upserted", rowsUp, "err", err)
}
