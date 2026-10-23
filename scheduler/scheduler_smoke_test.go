//go:build smoke

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/Cyprinus12138/sg-property-engine/internal/testdb"
	"github.com/Cyprinus12138/sg-property-engine/store"
)

// Smoke: a RunArg job receives the arg passed to TriggerArg, and the
// run completes through the real sync_runs lifecycle.
func TestTriggerArg_ForwardsArgToRunArgJob(t *testing.T) {
	pool := testdb.Open(t)
	s := New(&store.Store{Pool: pool})

	got := make(chan string, 1)
	if err := s.Register(Job{
		Source: "on_demand",
		RunArg: func(_ context.Context, _ string, arg string, _ store.ProgressReporter) (int, int, error) {
			got <- arg
			return 0, 0, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := s.TriggerArg(context.Background(), "on_demand", store.ModeIncremental, "the-example-project")
	if err != nil {
		t.Fatalf("TriggerArg: %v", err)
	}
	if runID == 0 {
		t.Errorf("expected a sync_runs id")
	}

	select {
	case arg := <-got:
		if arg != "the-example-project" {
			t.Errorf("RunArg received arg = %q, want the-example-project", arg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunArg was not invoked within 5s")
	}

	// Let the run lifecycle settle (FinishRun is async in runJob).
	s.WaitDone(context.Background(), "on_demand")
}

// Trigger (no arg) forwards an empty arg — back-compat for the batch
// sources that ignore it.
func TestTrigger_EmptyArgForRunArgJob(t *testing.T) {
	pool := testdb.Open(t)
	s := New(&store.Store{Pool: pool})

	got := make(chan string, 1)
	_ = s.Register(Job{
		Source: "on_demand",
		RunArg: func(_ context.Context, _ string, arg string, _ store.ProgressReporter) (int, int, error) {
			got <- arg
			return 0, 0, nil
		},
	})
	if _, err := s.Trigger(context.Background(), "on_demand", store.ModeIncremental); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	select {
	case arg := <-got:
		if arg != "" {
			t.Errorf("plain Trigger should forward empty arg, got %q", arg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunArg not invoked")
	}
	s.WaitDone(context.Background(), "on_demand")
}
