package devenv

import (
	"context"
	"errors"
	"testing"
	"time"
)

type lifecycleClock struct{ now time.Time }

func (c *lifecycleClock) Now() time.Time { return c.now }

type lifecycleWorkflow struct {
	started   []Operation
	scheduled []Environment
}

func (w *lifecycleWorkflow) Start(_ context.Context, operation Operation) (string, error) {
	w.started = append(w.started, operation)
	return "worker#" + operation.ID, nil
}

func (w *lifecycleWorkflow) ScheduleLease(_ context.Context, env Environment) error {
	w.scheduled = append(w.scheduled, env)
	return nil
}

type lifecycleStore struct {
	Store
	env                Environment
	op                 Operation
	checkpoint         Checkpoint
	createMutation     CreateMutation
	beginMutation      BeginMutation
	completion         CompletionMutation
	ackErrors          []error
	ackVersions        []int64
	reconcileQuotaErr  error
	listOperationsErr  error
	listSchedulesErr   error
	listDueErr         error
	due                []DueItem
	replay             MutationResult
	replayFound        bool
	getCheckpointErr   error
	getCheckpointCalls int
}

func (s *lifecycleStore) ReplayMutation(context.Context, Idempotency) (MutationResult, bool, error) {
	return s.replay, s.replayFound, nil
}

func (s *lifecycleStore) Create(_ context.Context, mutation CreateMutation, _ QuotaLimits) (MutationResult, error) {
	s.createMutation = mutation
	s.env = mutation.Environment
	s.op = mutation.Operation
	return MutationResult{Environment: s.env, Operation: s.op}, nil
}

func (s *lifecycleStore) Begin(_ context.Context, mutation BeginMutation, _ QuotaLimits) (MutationResult, error) {
	s.beginMutation = mutation
	env := s.env
	env.State = mutation.NextState
	env.ActiveOperationID = mutation.Operation.ID
	env.Version++
	if mutation.Lease != nil {
		env.Lease = *mutation.Lease
	}
	s.env = env
	s.op = mutation.Operation
	return MutationResult{Environment: env, Operation: mutation.Operation}, nil
}

func (s *lifecycleStore) Complete(_ context.Context, mutation CompletionMutation) (MutationResult, error) {
	s.completion = mutation
	s.env = mutation.Environment
	s.op = mutation.Operation
	return MutationResult{Environment: mutation.Environment, Operation: mutation.Operation}, nil
}

func (s *lifecycleStore) GetEnvironment(context.Context, string) (Environment, error) {
	return s.env, nil
}

func (s *lifecycleStore) GetOperation(context.Context, string) (Operation, error) {
	return s.op, nil
}

func (s *lifecycleStore) GetCheckpoint(context.Context, string) (Checkpoint, error) {
	s.getCheckpointCalls++
	if s.getCheckpointErr != nil {
		return Checkpoint{}, s.getCheckpointErr
	}
	return s.checkpoint, nil
}

func (s *lifecycleStore) AckLeaseSchedule(_ context.Context, _ string, version int64) error {
	s.ackVersions = append(s.ackVersions, version)
	if len(s.ackErrors) == 0 {
		return nil
	}
	err := s.ackErrors[0]
	s.ackErrors = s.ackErrors[1:]
	return err
}

func (s *lifecycleStore) SetOperationExecution(context.Context, string, string) error { return nil }
func (s *lifecycleStore) FailDispatch(context.Context, Operation, error) error        { return nil }

func (s *lifecycleStore) ReconcileQuotas(context.Context) error { return s.reconcileQuotaErr }

func (s *lifecycleStore) ListPendingOperations(context.Context, int32) ([]Operation, error) {
	return nil, s.listOperationsErr
}

func (s *lifecycleStore) ListPendingLeaseSchedules(context.Context, int32) ([]Environment, error) {
	return nil, s.listSchedulesErr
}

func (s *lifecycleStore) ListDue(context.Context, time.Time, int32) ([]DueItem, error) {
	return s.due, s.listDueErr
}

func TestFailedArchiveRecoveryIsBoundedAndNonDestructive(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	clock := &lifecycleClock{now: now}
	store := &lifecycleStore{
		env: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateArchiving,
			Profile: ProfileStandard, Visibility: VisibilityRestricted,
			Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 7}, Version: 4,
			ActiveOperationID: "op-1", InstanceID: "i-1", WorkspaceVolumeID: "vol-1",
		},
		op: Operation{
			ID: "op-1", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "owner-1",
			Action: ActionArchive, Status: OperationRunning, ClaimToken: "claim-1",
		},
	}
	workflow := &lifecycleWorkflow{}
	service := NewService(store, workflow, nil, clock)
	result, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: false, Error: "snapshot failed", ClaimToken: "claim-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Environment.State != StateError || result.Environment.FailedAction != ActionArchive {
		t.Fatalf("unexpected recovery state: %+v", result.Environment)
	}
	if result.Environment.InstanceID != "i-1" || result.Environment.WorkspaceVolumeID != "vol-1" {
		t.Fatalf("archive failure discarded workspace resources: %+v", result.Environment)
	}
	if result.Environment.RecoveryStartedAt == nil || !result.Environment.RecoveryStartedAt.Equal(now) {
		t.Fatalf("unexpected recovery start: %v", result.Environment.RecoveryStartedAt)
	}
	wantCleanup := now.Add(DefaultErrorRecoveryTTL)
	if result.Environment.CleanupAfter == nil || !result.Environment.CleanupAfter.Equal(wantCleanup) {
		t.Fatalf("unexpected fixed cleanup deadline: %v", result.Environment.CleanupAfter)
	}
	wantRetry := now.Add(DefaultErrorCleanupTTL)
	if result.Environment.RecoveryRetryAfter == nil || !result.Environment.RecoveryRetryAfter.Equal(wantRetry) {
		t.Fatalf("unexpected recovery retry: %v", result.Environment.RecoveryRetryAfter)
	}
	if result.Environment.Lease.Version != 8 || len(workflow.scheduled) != 1 {
		t.Fatalf("recovery was not scheduled with a fresh generation: %+v", result.Environment.Lease)
	}

	clock.now = now.Add(DefaultErrorRecoveryTTL)
	store.env = result.Environment
	store.env.State = StateArchiving
	store.env.ActiveOperationID = "op-2"
	store.op = Operation{
		ID: "op-2", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "system:lease",
		Action: ActionArchive, Status: OperationRunning, ClaimToken: "claim-2",
	}
	workflow.scheduled = nil
	result, err = service.Complete(context.Background(), systemIdentity(), "op-2", CompleteRequest{
		Succeeded: false, Error: "still failing", ClaimToken: "claim-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Environment.CleanupAfter == nil || !result.Environment.CleanupAfter.Equal(wantCleanup) {
		t.Fatalf("recovery deadline slid forward: %v", result.Environment.CleanupAfter)
	}
	wantRetry = clock.now.Add(DefaultArchiveRetryTTL)
	if result.Environment.RecoveryRetryAfter == nil || !result.Environment.RecoveryRetryAfter.Equal(wantRetry) ||
		len(workflow.scheduled) != 1 {
		t.Fatalf("durable archive retry was not scheduled: retry=%v schedules=%d",
			result.Environment.RecoveryRetryAfter, len(workflow.scheduled))
	}
	if result.Environment.FailedAction != ActionArchive || result.Environment.WorkspaceVolumeID != "vol-1" {
		t.Fatalf("bounded recovery became destructive: %+v", result.Environment)
	}
}

func TestArchiveCompletionSettlesCheckpointQuotaReservation(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	newStore := func() *lifecycleStore {
		return &lifecycleStore{
			env: Environment{
				ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateArchiving,
				Profile: ProfileStandard, Visibility: VisibilityRestricted,
				Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 7}, Version: 4,
				ActiveOperationID: "op-1", WorkspaceVolumeID: "vol-1",
				CheckpointQuotaReserved: true, PinnedCheckpointQuotaReserved: true,
				CheckpointQuotaName: "preserved-checkpoint",
			},
			op: Operation{
				ID: "op-1", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "system:lease",
				Action: ActionArchive, Status: OperationRunning, ClaimToken: "claim-1",
			},
		}
	}

	failedStore := newStore()
	service := NewService(failedStore, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	failed, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: false, Error: "snapshot failed", ClaimToken: "claim-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failedStore.completion.CheckpointDelta != 0 || failedStore.completion.PinnedCheckpointDelta != 0 ||
		!failed.Environment.CheckpointQuotaReserved || !failed.Environment.PinnedCheckpointQuotaReserved ||
		failed.Environment.CheckpointQuotaName != "preserved-checkpoint" {
		t.Fatalf("failed archive lost quota reservation: %+v", failedStore.completion)
	}

	succeededStore := newStore()
	service = NewService(succeededStore, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	result, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: true, ClaimToken: "claim-1", Result: WorkflowResult{SnapshotID: "snap-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if succeededStore.completion.CheckpointDelta != 0 || succeededStore.completion.PinnedCheckpointDelta != 0 ||
		succeededStore.completion.Checkpoint == nil || !succeededStore.completion.Checkpoint.Pinned ||
		succeededStore.completion.Checkpoint.Name != "preserved-checkpoint" {
		t.Fatalf("successful archive double-counted quota reservation: %+v", succeededStore.completion)
	}
	if result.Environment.CheckpointQuotaReserved || result.Environment.PinnedCheckpointQuotaReserved ||
		result.Environment.CheckpointQuotaName != "" {
		t.Fatalf("successful archive retained reservation flags: %+v", result.Environment)
	}
}

func TestSuccessfulStartReleasesFailedArchiveQuotaReservation(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := &lifecycleStore{
		env: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateStarting,
			Profile: ProfileStandard, Visibility: VisibilityRestricted,
			Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 7}, Version: 4,
			ActiveOperationID: "op-1", CheckpointQuotaReserved: true,
			PinnedCheckpointQuotaReserved: true, CheckpointQuotaName: "preserved-checkpoint",
		},
		op: Operation{
			ID: "op-1", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "owner-1",
			Action: ActionStart, Status: OperationRunning, ClaimToken: "claim-1",
		},
	}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	result, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: true, ClaimToken: "claim-1", Result: WorkflowResult{
			InstanceID: "i-1", InstanceRoleARN: "role-1", WorkspaceVolumeID: "vol-1",
			Host: "env.example.com", PrivateUpstream: "https://10.0.0.1:8443", TLSCertSHA256: "aa",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.completion.CheckpointDelta != -1 || store.completion.PinnedCheckpointDelta != -1 ||
		result.Environment.CheckpointQuotaReserved || result.Environment.PinnedCheckpointQuotaReserved ||
		result.Environment.CheckpointQuotaName != "" {
		t.Fatalf("successful start retained archive quota reservation: %+v", store.completion)
	}
}

func TestErrorRecoveryCannotExtendOriginalLease(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	leaseDeadline := now.Add(time.Hour)
	env := Environment{Lease: Lease{
		IdleExpiresAt: leaseDeadline, HardExpiresAt: leaseDeadline.Add(time.Hour),
	}}
	if got := errorRecoveryDeadline(env, now); !got.Equal(leaseDeadline) {
		t.Fatalf("error recovery extended compute past its lease: %s", got)
	}
}

func TestSuccessfulRestoreReleasesCheckpointReference(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	checkpoint := Checkpoint{
		ID: "checkpoint-1", OwnerID: "owner-1", SnapshotID: "snap-1",
		State: CheckpointAvailable, ReferenceCount: 2,
	}
	store := &lifecycleStore{
		env: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateStarting,
			Profile: ProfileStandard, Visibility: VisibilityRestricted,
			Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 4}, Version: 9,
			ActiveOperationID: "op-1", CurrentCheckpoint: checkpoint.ID, CurrentSnapshotID: checkpoint.SnapshotID,
		},
		op: Operation{
			ID: "op-1", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "owner-1",
			Action: ActionStart, Status: OperationRunning, ClaimToken: "claim-1",
		},
		checkpoint: checkpoint,
	}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	result, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: true, ClaimToken: "claim-1", Result: WorkflowResult{
			InstanceID: "i-1", InstanceRoleARN: "role-1", WorkspaceVolumeID: "vol-1",
			Host: "env.example.com", PrivateUpstream: "https://10.0.0.1:8443", TLSCertSHA256: "aa",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.completion.ReleaseCheckpoint == nil || store.completion.ReleaseCheckpoint.ID != checkpoint.ID {
		t.Fatalf("checkpoint reference was not released: %+v", store.completion.ReleaseCheckpoint)
	}
	if result.Environment.CurrentCheckpoint != "" || result.Environment.CurrentSnapshotID != "" {
		t.Fatalf("restored environment retained checkpoint binding: %+v", result.Environment)
	}
}

func TestFailedArchiveMovesReferenceToCompletedRecoverySnapshot(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	checkpoint := Checkpoint{
		ID: "checkpoint-source", OwnerID: "owner-1", SnapshotID: "snap-source",
		State: CheckpointAvailable, ReferenceCount: 1,
	}
	store := &lifecycleStore{
		env: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateArchiving,
			Profile: ProfileStandard, Visibility: VisibilityRestricted,
			Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 4}, Version: 9,
			ActiveOperationID: "op-1", WorkspaceVolumeID: "vol-1",
			CurrentCheckpoint: checkpoint.ID, CurrentSnapshotID: checkpoint.SnapshotID,
		},
		op: Operation{
			ID: "op-1", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "owner-1",
			Action: ActionArchive, Status: OperationRunning, ClaimToken: "claim-1",
		},
		checkpoint: checkpoint,
	}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	result, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: false, Error: "cleanup failed", ClaimToken: "claim-1",
		Result: WorkflowResult{SnapshotID: "snap-recovery"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.completion.ReleaseCheckpoint == nil || store.completion.ReleaseCheckpoint.ID != checkpoint.ID {
		t.Fatalf("source checkpoint reference was not released: %+v", store.completion.ReleaseCheckpoint)
	}
	if result.Environment.CurrentCheckpoint != "" || result.Environment.CurrentSnapshotID != "snap-recovery" {
		t.Fatalf("completed recovery snapshot was not made canonical: %+v", result.Environment)
	}
}

func TestFailedArchiveCannotDetachCheckpointForSameSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	checkpoint := Checkpoint{
		ID: "checkpoint-source", OwnerID: "owner-1", SnapshotID: "snap-source",
		State: CheckpointAvailable, ReferenceCount: 1,
	}
	store := &lifecycleStore{
		env: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateArchiving,
			Profile: ProfileStandard, Visibility: VisibilityRestricted,
			Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 4}, Version: 9,
			ActiveOperationID: "op-1", WorkspaceVolumeID: "vol-1",
			CurrentCheckpoint: checkpoint.ID, CurrentSnapshotID: checkpoint.SnapshotID,
		},
		op: Operation{
			ID: "op-1", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "owner-1",
			Action: ActionArchive, Status: OperationRunning, ClaimToken: "claim-1",
		},
		checkpoint: checkpoint,
	}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	result, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: false, Error: "invalid reused snapshot", ClaimToken: "claim-1",
		Result: WorkflowResult{SnapshotID: checkpoint.SnapshotID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.completion.ReleaseCheckpoint != nil || result.Environment.CurrentCheckpoint != checkpoint.ID {
		t.Fatalf("same snapshot escaped its checkpoint reference: %+v", result.Environment)
	}
}

func TestCreateReplayDoesNotRequireSourceCheckpoint(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	replayed := MutationResult{
		Environment: Environment{ID: "env-1", State: StateRunning, Lease: Lease{Version: 4}},
		Operation:   Operation{ID: "op-1", Action: ActionCreate, Status: OperationSucceeded},
		Replayed:    true,
	}
	store := &lifecycleStore{replay: replayed, replayFound: true, getCheckpointErr: ErrNotFound}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	result, err := service.Create(context.Background(), Identity{
		PrincipalID: "owner:v1:owner-1", Source: IdentitySourceAWSIAM,
	}, CreateRequest{
		Name: "workspace", Profile: ProfileStandard, Visibility: VisibilityRestricted,
		Source:           Source{Repository: "repo", BundleKey: "environments/uploads/owner-1/request/source.tgz"},
		FromCheckpointID: "checkpoint-deleted",
	}, "create-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation.ID != replayed.Operation.ID || store.getCheckpointCalls != 0 {
		t.Fatalf("idempotent replay depended on deleted checkpoint: result=%+v calls=%d",
			result, store.getCheckpointCalls)
	}
}

func TestStartUsesMonotonicLeaseGeneration(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := &lifecycleStore{env: Environment{
		ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateStopped,
		Profile: ProfileStandard, Visibility: VisibilityRestricted,
		Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 11}, Version: 4,
	}}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	_, err := service.Start(context.Background(), Identity{
		PrincipalID: "owner-1", Source: IdentitySourceAWSIAM,
	}, "env-1", StartRequest{}, "start-env-1")
	if err != nil {
		t.Fatal(err)
	}
	if store.beginMutation.Lease == nil || store.beginMutation.Lease.Version != 12 {
		t.Fatalf("lease generation reset across restart: %+v", store.beginMutation.Lease)
	}
}

func TestStaleScheduleAckRepairsCurrentGeneration(t *testing.T) {
	old := Environment{ID: "env-1", Lease: Lease{Version: 3}}
	current := Environment{ID: "env-1", Lease: Lease{Version: 4}}
	store := &lifecycleStore{env: current, ackErrors: []error{ErrConflict, nil}}
	workflow := &lifecycleWorkflow{}
	service := NewService(store, workflow, nil, nil)
	if err := service.scheduleLease(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if len(workflow.scheduled) != 2 || workflow.scheduled[0].Lease.Version != 3 ||
		workflow.scheduled[1].Lease.Version != 4 {
		t.Fatalf("current schedule was not repaired: %+v", workflow.scheduled)
	}
}

func TestRunningCompletionFencesEarlierScheduleWriter(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := &lifecycleStore{
		env: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateProvisioning,
			Profile: ProfileStandard, Visibility: VisibilityRestricted,
			Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 3}, Version: 1,
			ActiveOperationID: "op-1",
		},
		op: Operation{
			ID: "op-1", EnvironmentID: "env-1", OwnerID: "owner-1", ActorPrincipal: "owner-1",
			Action: ActionCreate, Status: OperationRunning, ClaimToken: "claim-1",
		},
	}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	result, err := service.Complete(context.Background(), systemIdentity(), "op-1", CompleteRequest{
		Succeeded: true, ClaimToken: "claim-1", Result: WorkflowResult{
			InstanceID: "i-1", InstanceRoleARN: "role-1", WorkspaceVolumeID: "vol-1",
			Host: "env.example.com", PrivateUpstream: "https://10.0.0.1:8443", TLSCertSHA256: "aa",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Environment.Lease.Version != 4 {
		t.Fatalf("running completion reused a stale schedule generation: %d", result.Environment.Lease.Version)
	}
}

func TestReconcileContinuesAfterIndependentFailures(t *testing.T) {
	store := &lifecycleStore{
		reconcileQuotaErr: errors.New("quota"), listOperationsErr: errors.New("operations"),
		listSchedulesErr: errors.New("schedules"), listDueErr: errors.New("due"),
	}
	service := NewService(store, &lifecycleWorkflow{}, nil, nil)
	result, err := service.Reconcile(context.Background(), systemIdentity(), 100)
	if err == nil || len(result.Failures) != 4 {
		t.Fatalf("independent reconciliation failures were lost: result=%+v err=%v", result, err)
	}
}

func TestArchiveCheckpointStartsReferenced(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	requestedAt := now.Add(-time.Hour)
	checkpoint := checkpointForArchive(Operation{ID: "op-1", RequestedAt: requestedAt}, Environment{
		ID: "env-1", OwnerID: "owner-1", OwnerEmail: "owner@example.com", Name: "workspace",
	}, "snap-1", now)
	if checkpoint.ReferenceCount != 1 || checkpoint.ExpiresAt == nil {
		t.Fatalf("unexpected archived checkpoint retention: %+v", checkpoint)
	}
	if checkpoint.Name != "owner-workspace-20260827-110000" || !checkpoint.CreatedAt.Equal(now) {
		t.Fatalf("checkpoint name is not stable across workflow completion: %+v", checkpoint)
	}
}

func TestExpiredReferencedCheckpointDeletesArchivedEnvironmentFirst(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	checkpoint := Checkpoint{
		ID: "checkpoint-1", EnvironmentID: "env-1", OwnerID: "owner-1", SnapshotID: "snap-1",
		State: CheckpointAvailable, ReferenceCount: 1, CreatedAt: now.Add(-DefaultCheckpointTTL),
	}
	store := &lifecycleStore{
		env: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateArchived,
			Profile: ProfileStandard, Visibility: VisibilityRestricted,
			Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 4}, Version: 7,
			CurrentCheckpoint: checkpoint.ID, CurrentSnapshotID: checkpoint.SnapshotID,
		},
		due: []DueItem{{Checkpoint: &checkpoint}},
	}
	workflow := &lifecycleWorkflow{}
	service := NewService(store, workflow, nil, &lifecycleClock{now: now})
	result, err := service.Reconcile(context.Background(), systemIdentity(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if store.beginMutation.NextState != StateDeleting || store.beginMutation.Operation.Action != ActionDelete {
		t.Fatalf("archived environment was not retired before its only checkpoint: %+v", store.beginMutation)
	}
	if len(result.QueuedOperationIDs) != 1 || len(workflow.started) != 1 {
		t.Fatalf("archive retention did not queue deletion: result=%+v started=%+v", result, workflow.started)
	}
}

func TestErrorExpiryRetriesRecordedAction(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	cleanupAfter := now.Add(time.Hour)
	store := &lifecycleStore{env: Environment{
		ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateError,
		Profile: ProfileStandard, Visibility: VisibilityRestricted,
		Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 8}, Version: 5,
		FailedAction: ActionStop, CleanupAfter: &cleanupAfter, RecoveryRetryAfter: &now,
		WorkspaceVolumeID: "vol-1",
	}}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	if _, err := service.ExpireLease(context.Background(), LeaseExpiry{
		EnvironmentID: "env-1", LeaseVersion: 8,
	}); err != nil {
		t.Fatal(err)
	}
	if store.beginMutation.Operation.Action != ActionStop || store.beginMutation.NextState != StateStopping {
		t.Fatalf("ERROR expiry chose a destructive fallback: %+v", store.beginMutation)
	}
}

func TestErrorExpiryEscalatesToArchiveAfterRecoveryDeadline(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	startedAt := now.Add(-DefaultErrorRecoveryTTL)
	store := &lifecycleStore{env: Environment{
		ID: "env-1", OwnerID: "owner-1", Name: "workspace", State: StateError,
		Profile: ProfileStandard, Visibility: VisibilityRestricted,
		Source: Source{Repository: "repo", Ref: "main"}, Lease: Lease{Version: 9}, Version: 6,
		FailedAction: ActionStart, RecoveryStartedAt: &startedAt, CleanupAfter: &now,
		RecoveryRetryAfter: &now, InstanceID: "i-1", WorkspaceVolumeID: "vol-1",
	}}
	service := NewService(store, &lifecycleWorkflow{}, nil, &lifecycleClock{now: now})
	if _, err := service.ExpireLease(context.Background(), LeaseExpiry{
		EnvironmentID: "env-1", LeaseVersion: 9,
	}); err != nil {
		t.Fatal(err)
	}
	if store.beginMutation.Operation.Action != ActionArchive || store.beginMutation.NextState != StateArchiving {
		t.Fatalf("post-deadline recovery did not preserve data through archive: %+v", store.beginMutation)
	}
}

func TestPostDeadlineArchiveRecoveryStopsComputeWithoutCollectingStorage(t *testing.T) {
	cleanupAfter := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	retryAfter := cleanupAfter.Add(DefaultArchiveRetryTTL)
	env := Environment{
		State: StateError, FailedAction: ActionArchive, CleanupAfter: &cleanupAfter,
		RecoveryRetryAfter: &retryAfter, InstanceID: "i-1", WorkspaceVolumeID: "vol-1",
	}
	if got := environmentSafetyDeadline(env); !got.Equal(cleanupAfter) {
		t.Fatalf("compute deadline moved with archive retry: %s", got)
	}
	if environmentGarbageCollectable(env, retryAfter.Add(DefaultErrorCleanupTTL)) {
		t.Fatal("failed archive became eligible for destructive orphan collection")
	}
}

func TestOperationClaimOutlivesMaximumExecutionOverlap(t *testing.T) {
	if DefaultOperationClaimTTL < 35*time.Minute {
		t.Fatalf("operation claim TTL is too short: %s", DefaultOperationClaimTTL)
	}
}

func systemIdentity() Identity {
	return Identity{PrincipalID: "system:test", Source: IdentitySourceSystem, Internal: true}
}
