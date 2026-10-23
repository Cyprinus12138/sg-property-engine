package scheduler

import (
	"context"
	"testing"

	"github.com/Cyprinus12138/sg-property-engine/store"
)

// Pure unit tests for the Run/RunArg registration invariant.
// No DB needed — Register only validates + records the job when Cron is
// empty.

func TestRegister_RequiresExactlyOneRunner(t *testing.T) {
	noop := func(context.Context, string, store.ProgressReporter) (int, int, error) { return 0, 0, nil }
	noopArg := func(context.Context, string, string, store.ProgressReporter) (int, int, error) { return 0, 0, nil }

	cases := []struct {
		name    string
		job     Job
		wantErr bool
	}{
		{"neither runner", Job{Source: "a"}, true},
		{"both runners", Job{Source: "b", Run: noop, RunArg: noopArg}, true},
		{"run only", Job{Source: "c", Run: noop}, false},
		{"runarg only", Job{Source: "d", RunArg: noopArg}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil) // no store needed: cron empty, no trigger
			err := s.Register(tc.job)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Register err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestRegister_DuplicateSourceRejected(t *testing.T) {
	noop := func(context.Context, string, store.ProgressReporter) (int, int, error) { return 0, 0, nil }
	s := New(nil)
	if err := s.Register(Job{Source: "dup", Run: noop}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := s.Register(Job{Source: "dup", Run: noop}); err == nil {
		t.Errorf("duplicate source: want error")
	}
}
