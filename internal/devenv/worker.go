package devenv

import (
	"context"
	"errors"
	"fmt"
)

type LifecycleExecutor interface {
	Execute(context.Context, Environment, Operation) (WorkflowResult, error)
	DeleteCheckpoint(context.Context, Checkpoint) error
	ReconcileOrphans(context.Context, []Environment, []Checkpoint) error
}

type ExecutionError struct {
	Result WorkflowResult
	Err    error
}

type RetryableError struct {
	Err error
}

func (e *RetryableError) Error() string {
	return e.Err.Error()
}

func (e *RetryableError) Unwrap() error {
	return e.Err
}

func (e *ExecutionError) Error() string {
	return e.Err.Error()
}

func (e *ExecutionError) Unwrap() error {
	return e.Err
}

type Worker struct {
	Service  *Service
	Store    Store
	Executor LifecycleExecutor
}

func (w *Worker) HandleOperation(ctx context.Context, event Operation) error {
	if w.Service == nil || w.Store == nil || w.Executor == nil || event.ID == "" {
		return errors.New("worker is not configured")
	}
	claimToken, err := newID("claim")
	if err != nil {
		return err
	}
	op, claimed, exhausted, err := w.Store.ClaimOperation(ctx, event.ID, claimToken,
		w.Service.clock.Now().UTC(), DefaultOperationClaimTTL, DefaultOperationAttempts, DefaultOperationMaxAge)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	identity := Identity{PrincipalID: "system:worker", Source: IdentitySourceSystem, Internal: true}
	if exhausted {
		failure := fmt.Errorf("operation exceeded %s or %d attempts", DefaultOperationMaxAge, DefaultOperationAttempts)
		if op.Action == ActionDeleteCheckpoint {
			_, err = w.Service.CompleteCheckpointDelete(ctx, identity, op.ID, claimToken, failure)
			return err
		}
		_, err = w.Service.Complete(ctx, identity, op.ID, CompleteRequest{
			Succeeded: false, Error: failure.Error(), ClaimToken: claimToken,
		})
		return err
	}
	if op.Action == ActionDeleteCheckpoint {
		checkpoint, getErr := w.Store.GetCheckpoint(ctx, op.CheckpointID)
		if getErr != nil {
			return getErr
		}
		executeErr := w.Executor.DeleteCheckpoint(ctx, checkpoint)
		_, completeErr := w.Service.CompleteCheckpointDelete(ctx, identity, op.ID, claimToken, executeErr)
		if completeErr != nil {
			return completeErr
		}
		return executeErr
	}
	env, err := w.Store.GetEnvironment(ctx, op.EnvironmentID)
	if err != nil {
		return err
	}
	if env.ActiveOperationID != op.ID {
		return nil
	}
	result, executeErr := w.Executor.Execute(ctx, env, op)
	var retryable *RetryableError
	if errors.As(executeErr, &retryable) {
		releaseErr := w.Store.ReleaseOperationClaim(ctx, op.ID, claimToken, w.Service.clock.Now().UTC())
		if releaseErr != nil {
			return errors.Join(executeErr, fmt.Errorf("release retryable operation claim: %w", releaseErr))
		}
		return executeErr
	}
	if errors.Is(executeErr, context.DeadlineExceeded) || errors.Is(executeErr, context.Canceled) {
		return executeErr
	}
	if executeErr != nil {
		var partial *ExecutionError
		if errors.As(executeErr, &partial) {
			result = partial.Result
		}
	}
	_, completeErr := w.Service.Complete(ctx, identity, op.ID, CompleteRequest{
		Succeeded: executeErr == nil, Error: errorText(executeErr), Result: result, ClaimToken: claimToken,
	})
	if completeErr != nil {
		return completeErr
	}
	return executeErr
}

func (w *Worker) HandleLease(ctx context.Context, event LeaseExpiry) error {
	if w.Service == nil {
		return errors.New("worker is not configured")
	}
	_, err := w.Service.ExpireLease(ctx, event)
	return err
}

func (w *Worker) HandlePeriodic(ctx context.Context) (ReconcileResult, error) {
	if w.Service == nil || w.Executor == nil {
		return ReconcileResult{}, errors.New("worker is not configured")
	}
	identity := Identity{PrincipalID: "system:reconciler", Source: IdentitySourceSystem, Internal: true}
	result, reconcileErr := w.Service.Reconcile(ctx, identity, 100)
	var failures []error
	if reconcileErr != nil {
		result.Failures = append(result.Failures, "control reconciliation: "+reconcileErr.Error())
		failures = append(failures, reconcileErr)
	}
	environments, environmentsErr := w.Store.ListAllEnvironments(ctx)
	if environmentsErr != nil {
		result.Failures = append(result.Failures, "list environments: "+environmentsErr.Error())
		failures = append(failures, environmentsErr)
	}
	checkpoints, checkpointsErr := w.Store.ListAllCheckpoints(ctx)
	if checkpointsErr != nil {
		result.Failures = append(result.Failures, "list checkpoints: "+checkpointsErr.Error())
		failures = append(failures, checkpointsErr)
	}
	if environmentsErr == nil && checkpointsErr == nil {
		if orphanErr := w.Executor.ReconcileOrphans(ctx, environments, checkpoints); orphanErr != nil {
			result.Failures = append(result.Failures, "orphan reconciliation: "+orphanErr.Error())
			failures = append(failures, orphanErr)
		}
	}
	if len(result.Failures) > 0 && len(failures) == 0 {
		failures = append(failures, errors.New("reconciliation reported failures"))
	}
	return result, errors.Join(failures...)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
