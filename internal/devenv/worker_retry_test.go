package devenv

import (
	"context"
	"errors"
	"testing"
	"time"
)

type retryWorkerClock struct{ now time.Time }

func (c retryWorkerClock) Now() time.Time { return c.now }

type retryWorkerStore struct {
	Store
	environment Environment
	operation   Operation
	releasedID  string
	released    string
	releasedAt  time.Time
	releaseErr  error
}

func (s *retryWorkerStore) ClaimOperation(_ context.Context, _ string, token string, _ time.Time,
	_ time.Duration, _ int, _ time.Duration) (Operation, bool, bool, error) {
	op := s.operation
	op.Status = OperationRunning
	op.ClaimToken = token
	return op, true, false, nil
}

func (s *retryWorkerStore) GetEnvironment(context.Context, string) (Environment, error) {
	return s.environment, nil
}

func (s *retryWorkerStore) ReleaseOperationClaim(_ context.Context, id, token string, now time.Time) error {
	s.releasedID = id
	s.released = token
	s.releasedAt = now
	return s.releaseErr
}

type retryWorkerExecutor struct{ err error }

func (e retryWorkerExecutor) Execute(context.Context, Environment, Operation) (WorkflowResult, error) {
	return WorkflowResult{}, e.err
}

func (retryWorkerExecutor) DeleteCheckpoint(context.Context, Checkpoint) error { return nil }

func (retryWorkerExecutor) ReconcileOrphans(context.Context, []Environment, []Checkpoint) error {
	return nil
}

func TestWorkerReleasesTypedRetryableClaim(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	retryErr := errors.New("remote command timed out")
	store := &retryWorkerStore{
		environment: Environment{ID: "env-1", ActiveOperationID: "op-1"},
		operation:   Operation{ID: "op-1", EnvironmentID: "env-1", Action: ActionCreate},
	}
	worker := Worker{
		Service:  &Service{clock: retryWorkerClock{now: now}},
		Store:    store,
		Executor: retryWorkerExecutor{err: &ExecutionError{Err: &RetryableError{Err: retryErr}}},
	}
	if err := worker.HandleOperation(context.Background(), Operation{ID: "op-1"}); !errors.Is(err, retryErr) {
		t.Fatalf("HandleOperation() error = %v", err)
	}
	if store.releasedID != "op-1" || store.released == "" || !store.releasedAt.Equal(now) {
		t.Fatalf("retryable claim was not released: id=%q token=%q at=%s",
			store.releasedID, store.released, store.releasedAt)
	}
}

func TestWorkerKeepsCrashClaimForContextCancellation(t *testing.T) {
	store := &retryWorkerStore{
		environment: Environment{ID: "env-1", ActiveOperationID: "op-1"},
		operation:   Operation{ID: "op-1", EnvironmentID: "env-1", Action: ActionCreate},
	}
	worker := Worker{
		Service:  &Service{clock: retryWorkerClock{now: time.Now()}},
		Store:    store,
		Executor: retryWorkerExecutor{err: context.Canceled},
	}
	if err := worker.HandleOperation(context.Background(), Operation{ID: "op-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleOperation() error = %v", err)
	}
	if store.released != "" {
		t.Fatalf("crash claim was released with token %q", store.released)
	}
}

func TestWorkerReportsRetryClaimReleaseFailure(t *testing.T) {
	retryErr := errors.New("retry")
	releaseErr := errors.New("release")
	store := &retryWorkerStore{
		environment: Environment{ID: "env-1", ActiveOperationID: "op-1"},
		operation:   Operation{ID: "op-1", EnvironmentID: "env-1", Action: ActionCreate},
		releaseErr:  releaseErr,
	}
	worker := Worker{
		Service:  &Service{clock: retryWorkerClock{now: time.Now()}},
		Store:    store,
		Executor: retryWorkerExecutor{err: &RetryableError{Err: retryErr}},
	}
	err := worker.HandleOperation(context.Background(), Operation{ID: "op-1"})
	if !errors.Is(err, retryErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("HandleOperation() error = %v", err)
	}
}
