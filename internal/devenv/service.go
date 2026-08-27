package devenv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Service struct {
	store    Store
	workflow Workflow
	cloud    Cloud
	clock    Clock
	quotas   QuotaLimits
}

func NewService(store Store, workflow Workflow, cloud Cloud, clock Clock) *Service {
	if clock == nil {
		clock = WallClock{}
	}
	if cloud == nil {
		cloud = UnavailableCloud{}
	}
	return &Service{store: store, workflow: workflow, cloud: cloud, clock: clock, quotas: DefaultQuotaLimits()}
}

func (s *Service) SetQuotaLimits(limits QuotaLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	s.quotas = limits
	return nil
}

type CreateRequest struct {
	Name             string     `json:"name"`
	Profile          Profile    `json:"runtimeProfile"`
	Source           Source     `json:"source"`
	FromCheckpointID string     `json:"fromCheckpointId,omitempty"`
	Visibility       Visibility `json:"visibility,omitempty"`
}

type StartRequest struct {
	Profile *Profile `json:"runtimeProfile,omitempty"`
}

type ExtendRequest struct {
	Hours int `json:"hours"`
}

type ArchiveRequest struct {
	CheckpointName string `json:"checkpointName,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
}

type ActivityRequest struct {
	Kind string `json:"kind"`
}

type CompleteRequest struct {
	Succeeded  bool           `json:"succeeded"`
	Error      string         `json:"error,omitempty"`
	Result     WorkflowResult `json:"result,omitempty"`
	ClaimToken string         `json:"-"`
}

type ReconcileResult struct {
	QueuedOperationIDs []string `json:"queuedOperationIds"`
	Failures           []string `json:"failures,omitempty"`
}

type LeaseExpiry struct {
	EnvironmentID string `json:"environmentId"`
	LeaseVersion  int64  `json:"leaseVersion"`
}

func (s *Service) Create(ctx context.Context, identity Identity, req CreateRequest,
	idempotencyKey string) (MutationResult, error) {
	if err := identity.Validate(); err != nil {
		return MutationResult{}, errors.Join(ErrUnauthorized, err)
	}
	if err := ValidateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, &FieldError{Field: "Idempotency-Key", Err: err}
	}
	if err := ValidateDisplayName(req.Name); err != nil {
		return MutationResult{}, &FieldError{Field: "name", Err: err}
	}
	if req.Profile == "" {
		req.Profile = ProfileStandard
	}
	if err := req.Profile.Validate(); err != nil {
		return MutationResult{}, &FieldError{Field: "runtimeProfile", Err: err}
	}
	if !s.profileEnabled(req.Profile) {
		return MutationResult{}, &FieldError{Field: "runtimeProfile", Err: ErrNotReady}
	}
	if err := req.Source.Validate(); err != nil {
		return MutationResult{}, &FieldError{Field: "source", Err: err}
	}
	if err := validateOwnedBundleKey(identity, req.Source.BundleKey); err != nil {
		return MutationResult{}, &FieldError{Field: "source.bundleKey", Err: err}
	}
	if req.Visibility == "" {
		req.Visibility = VisibilityOrganization
	}
	if err := req.Visibility.Validate(); err != nil {
		return MutationResult{}, &FieldError{Field: "visibility", Err: err}
	}
	now := s.clock.Now().UTC()
	hash, err := HashMutation(ActionCreate, req)
	if err != nil {
		return MutationResult{}, err
	}
	idempotency := newIdempotency(identity, idempotencyKey, hash, now)
	replayed, ok, err := s.store.ReplayMutation(ctx, idempotency)
	if err != nil {
		return MutationResult{}, err
	}
	if ok {
		return s.resume(ctx, replayed)
	}
	var sourceCheckpoint *Checkpoint
	if req.FromCheckpointID != "" {
		checkpoint, getErr := s.store.GetCheckpoint(ctx, req.FromCheckpointID)
		err = getErr
		if err != nil {
			return MutationResult{}, err
		}
		if checkpoint.OwnerID != identity.PrincipalID || checkpoint.State != CheckpointAvailable ||
			checkpoint.SnapshotID == "" {
			return MutationResult{}, ErrNotFound
		}
		sourceCheckpoint = &checkpoint
	}
	envID, err := newID("env")
	if err != nil {
		return MutationResult{}, err
	}
	opID, err := newID("op")
	if err != nil {
		return MutationResult{}, err
	}
	env := Environment{
		ID: envID, OwnerID: identity.PrincipalID, OwnerSubject: identity.AuditSubject,
		OwnerAccountID: identity.AccountID, OwnerEmail: identity.Email,
		Name: req.Name, State: StateProvisioning, Profile: req.Profile, Visibility: req.Visibility, Source: req.Source,
		Lease: NewLease(now), Version: 1, ActiveOperationID: opID, CurrentCheckpoint: req.FromCheckpointID,
		CreatedAt: now, UpdatedAt: now,
		WorkspaceSlot: true, RunningSlot: true, GPURunningSlot: req.Profile == ProfileGPUCVML,
	}
	if sourceCheckpoint != nil {
		env.CurrentSnapshotID = sourceCheckpoint.SnapshotID
	}
	op := newOperation(opID, envID, "", identity, ActionCreate, idempotencyKey, hash, now)
	result, err := s.store.Create(ctx, CreateMutation{
		Environment: env, Operation: op, Idempotency: idempotency,
		SourceCheckpoint: sourceCheckpoint,
	}, s.quotas)
	if err != nil {
		return result, err
	}
	if result.Replayed {
		return s.resume(ctx, result)
	}
	result, err = s.dispatch(ctx, result)
	if err != nil {
		return MutationResult{}, err
	}
	return result, s.scheduleLease(ctx, result.Environment)
}

func (s *Service) List(ctx context.Context, identity Identity) ([]Environment, error) {
	if err := identity.Validate(); err != nil {
		return nil, errors.Join(ErrUnauthorized, err)
	}
	return s.store.ListEnvironments(ctx, identity.PrincipalID)
}

func (s *Service) Get(ctx context.Context, identity Identity, id string) (Environment, error) {
	if err := identity.Validate(); err != nil {
		return Environment{}, errors.Join(ErrUnauthorized, err)
	}
	env, err := s.store.GetEnvironment(ctx, id)
	if err != nil {
		return Environment{}, err
	}
	if env.OwnerID != identity.PrincipalID {
		return Environment{}, ErrNotFound
	}
	return env, nil
}

func (s *Service) Start(ctx context.Context, identity Identity, id string, req StartRequest,
	idempotencyKey string) (MutationResult, error) {
	if req.Profile != nil {
		if err := req.Profile.Validate(); err != nil {
			return MutationResult{}, &FieldError{Field: "runtimeProfile", Err: err}
		}
	}
	return s.begin(ctx, identity, id, ActionStart, req.Profile, req, idempotencyKey, true)
}

func (s *Service) Stop(ctx context.Context, identity Identity, id, idempotencyKey string) (MutationResult, error) {
	return s.begin(ctx, identity, id, ActionStop, nil, struct{}{}, idempotencyKey, true)
}

func (s *Service) Archive(ctx context.Context, identity Identity, id string, req ArchiveRequest,
	idempotencyKey string) (MutationResult, error) {
	if req.CheckpointName != "" {
		if err := ValidateDisplayName(req.CheckpointName); err != nil {
			return MutationResult{}, &FieldError{Field: "checkpointName", Err: err}
		}
	}
	return s.begin(ctx, identity, id, ActionArchive, nil, req, idempotencyKey, true)
}

func (s *Service) Delete(ctx context.Context, identity Identity, id, idempotencyKey string) (MutationResult, error) {
	return s.begin(ctx, identity, id, ActionDelete, nil, struct{}{}, idempotencyKey, true)
}

func (s *Service) Extend(ctx context.Context, identity Identity, id string, req ExtendRequest,
	idempotencyKey string) (MutationResult, error) {
	if err := identity.Validate(); err != nil {
		return MutationResult{}, errors.Join(ErrUnauthorized, err)
	}
	if err := ValidateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, &FieldError{Field: "Idempotency-Key", Err: err}
	}
	hash, err := HashMutation(ActionExtend, req)
	if err != nil {
		return MutationResult{}, err
	}
	now := s.clock.Now().UTC()
	idempotency := newIdempotency(identity, idempotencyKey, hash, now)
	replayed, ok, err := s.store.ReplayMutation(ctx, idempotency)
	if err != nil {
		return MutationResult{}, err
	}
	if ok {
		return s.resume(ctx, replayed)
	}
	env, err := s.Get(ctx, identity, id)
	if err != nil {
		return MutationResult{}, err
	}
	if !CanExtend(env.State) {
		return MutationResult{}, errors.Join(ErrConflict, fmt.Errorf("cannot extend environment in %s", env.State))
	}
	lease, err := env.Lease.Extend(s.clock.Now(), time.Duration(req.Hours)*time.Hour)
	if err != nil {
		return MutationResult{}, &FieldError{Field: "hours", Err: err}
	}
	opID, err := newID("op")
	if err != nil {
		return MutationResult{}, err
	}
	op := newOperation(opID, id, "", identity, ActionExtend, idempotencyKey, hash, now)
	op.Status = OperationSucceeded
	result, err := s.store.Extend(ctx, ExtendMutation{
		EnvironmentID: id, OwnerID: identity.PrincipalID, ExpectedState: env.State,
		ExpectedLease: env.Lease.Version, ExpectedVersion: env.Version, Lease: lease, Operation: op,
		Idempotency: idempotency,
	})
	if err != nil {
		return MutationResult{}, err
	}
	return result, s.scheduleLease(ctx, result.Environment)
}

func (s *Service) Operation(ctx context.Context, identity Identity, id string) (Operation, error) {
	if err := identity.Validate(); err != nil {
		return Operation{}, errors.Join(ErrUnauthorized, err)
	}
	op, err := s.store.GetOperation(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	if op.OwnerID != identity.PrincipalID {
		return Operation{}, ErrNotFound
	}
	return op, nil
}

func (s *Service) Checkpoints(ctx context.Context, identity Identity) ([]Checkpoint, error) {
	if err := identity.Validate(); err != nil {
		return nil, errors.Join(ErrUnauthorized, err)
	}
	return s.store.ListCheckpoints(ctx, identity.PrincipalID)
}

func (s *Service) DeleteCheckpoint(ctx context.Context, identity Identity, id,
	idempotencyKey string) (Operation, error) {
	if err := identity.Validate(); err != nil {
		return Operation{}, errors.Join(ErrUnauthorized, err)
	}
	if err := ValidateIdempotencyKey(idempotencyKey); err != nil {
		return Operation{}, &FieldError{Field: "Idempotency-Key", Err: err}
	}
	payload := struct {
		ID string `json:"id"`
	}{ID: id}
	hash, err := HashMutation(ActionDeleteCheckpoint, payload)
	if err != nil {
		return Operation{}, err
	}
	now := s.clock.Now().UTC()
	idempotency := newIdempotency(identity, idempotencyKey, hash, now)
	replayed, ok, err := s.store.ReplayMutation(ctx, idempotency)
	if err != nil {
		return Operation{}, err
	}
	if ok {
		resumed, resumeErr := s.resume(ctx, replayed)
		return resumed.Operation, resumeErr
	}
	checkpoint, err := s.store.GetCheckpoint(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	if checkpoint.OwnerID != identity.PrincipalID {
		return Operation{}, ErrNotFound
	}
	if checkpoint.ReferenceCount != 0 {
		return Operation{}, errors.Join(ErrConflict, errors.New("checkpoint is referenced by an environment"))
	}
	opID, err := newID("op")
	if err != nil {
		return Operation{}, err
	}
	op := newOperation(opID, checkpoint.EnvironmentID, checkpoint.ID, identity,
		ActionDeleteCheckpoint, idempotencyKey, hash, now)
	op, wasReplayed, err := s.store.BeginCheckpointDelete(ctx, CheckpointMutation{
		Checkpoint: checkpoint, Operation: op,
		Idempotency: idempotency,
	})
	if err != nil || wasReplayed {
		return op, err
	}
	arn, err := s.workflow.Start(ctx, op)
	if err != nil {
		_ = s.store.FailDispatch(ctx, op, err)
		return Operation{}, err
	}
	err = s.store.SetOperationExecution(ctx, op.ID, arn)
	if err != nil {
		return Operation{}, err
	}
	op.ExecutionARN = arn
	return op, nil
}

func (s *Service) Complete(ctx context.Context, identity Identity, operationID string,
	req CompleteRequest) (MutationResult, error) {
	if err := identity.Validate(); err != nil || !identity.Internal {
		return MutationResult{}, ErrUnauthorized
	}
	op, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return MutationResult{}, err
	}
	if op.Status == OperationSucceeded || op.Status == OperationFailed {
		env, getErr := s.store.GetEnvironment(ctx, op.EnvironmentID)
		return MutationResult{Environment: env, Operation: op, Replayed: true}, getErr
	}
	env, err := s.store.GetEnvironment(ctx, op.EnvironmentID)
	if err != nil {
		return MutationResult{}, err
	}
	if env.ActiveOperationID != op.ID {
		return MutationResult{}, ErrConflict
	}
	mutation := CompletionMutation{
		Environment: env, Operation: op, ExpectedEnvironmentState: env.State,
		ExpectedEnvironmentVersion: env.Version, ExpectedOperationStatus: op.Status,
		ExpectedOperationClaim: req.ClaimToken,
		DeleteRouteHost:        env.Host,
		QuotaLimits:            s.quotas,
	}
	now := s.clock.Now().UTC()
	mutation.Environment.Version++
	mutation.Environment.UpdatedAt = now
	mutation.Environment.ActiveOperationID = ""
	mutation.Operation.UpdatedAt = now
	if !req.Succeeded {
		applyRuntimeResult(&mutation.Environment, req.Result)
		if op.Action == ActionArchive && req.Result.SnapshotID != "" {
			if env.CurrentCheckpoint == "" || req.Result.SnapshotID != env.CurrentSnapshotID {
				if err = s.prepareCheckpointRelease(ctx, &mutation); err != nil {
					return MutationResult{}, err
				}
				mutation.Environment.CurrentCheckpoint = ""
			}
			mutation.Environment.CurrentSnapshotID = req.Result.SnapshotID
		}
		mutation.Environment.State = StateError
		if shouldResetRecovery(env, op) {
			startedAt := now
			mutation.Environment.RecoveryStartedAt = &startedAt
			cleanupAfter := errorRecoveryDeadline(env, startedAt)
			mutation.Environment.CleanupAfter = &cleanupAfter
		}
		if mutation.Environment.CleanupAfter == nil {
			cleanupAfter := errorRecoveryDeadline(mutation.Environment, *mutation.Environment.RecoveryStartedAt)
			mutation.Environment.CleanupAfter = &cleanupAfter
		}
		deadline := *mutation.Environment.CleanupAfter
		mutation.Environment.FailedAction = op.Action
		retryAfter := now.Add(DefaultErrorCleanupTTL)
		if retryAfter.After(deadline) {
			retryAfter = deadline
		}
		if !now.Before(deadline) {
			if op.Action != ActionDelete {
				mutation.Environment.FailedAction = ActionArchive
			}
			retryAfter = now.Add(DefaultArchiveRetryTTL)
		}
		mutation.Environment.RecoveryRetryAfter = &retryAfter
		mutation.Environment.DirtyShutdown = req.Result.DirtyShutdown
		mutation.Environment.Lease.Version++
		mutation.ScheduleExpiry = true
		mutation.Operation.Status = OperationFailed
		mutation.Operation.Error = strings.TrimSpace(req.Error)
		if mutation.Operation.Error == "" {
			mutation.Operation.Error = "workflow failed"
		}
		result, completeErr := s.store.Complete(ctx, mutation)
		if completeErr != nil {
			return MutationResult{}, completeErr
		}
		if mutation.ScheduleExpiry {
			return result, s.scheduleLease(ctx, result.Environment)
		}
		return result, nil
	}
	final, err := CompletionState(env.State)
	if err != nil {
		return MutationResult{}, ErrConflict
	}
	mutation.Environment.State = final
	mutation.Operation.Status = OperationSucceeded
	mutation.Environment.DirtyShutdown = req.Result.DirtyShutdown
	mutation.Environment.CleanupAfter = nil
	mutation.Environment.RecoveryStartedAt = nil
	mutation.Environment.RecoveryRetryAfter = nil
	mutation.Environment.FailedAction = ""
	switch final {
	case StateRunning:
		applyRuntimeResult(&mutation.Environment, req.Result)
		if mutation.Environment.InstanceID == "" || mutation.Environment.InstanceRoleARN == "" ||
			mutation.Environment.Host == "" || mutation.Environment.PrivateUpstream == "" ||
			mutation.Environment.TLSCertSHA256 == "" {
			return MutationResult{}, &FieldError{Field: "result", Err: errors.New("running metadata is incomplete")}
		}
		if err = s.prepareCheckpointRelease(ctx, &mutation); err != nil {
			return MutationResult{}, err
		}
		mutation.Environment.ACLVersion++
		mutation.Environment.StoppedAt = nil
		mutation.Environment.ArchiveAfter = nil
		mutation.Environment.CurrentCheckpoint = ""
		mutation.Environment.CurrentSnapshotID = ""
		mutation.Environment.Lease.Version++
		mutation.Route = gatewayRoute(mutation.Environment)
		mutation.ScheduleExpiry = true
		releaseCheckpointQuotaReservation(&mutation, env)
	case StateStopped:
		stoppedAt := now
		archiveAfter := ArchiveDeadline(now)
		mutation.Environment.StoppedAt = &stoppedAt
		mutation.Environment.ArchiveAfter = &archiveAfter
		mutation.Environment.Lease.Version++
		mutation.RunningDelta, mutation.GPURunningDelta = releaseRunningSlots(&mutation.Environment)
		mutation.ScheduleExpiry = true
	case StateArchived:
		if req.Result.SnapshotID == "" {
			return MutationResult{}, &FieldError{Field: "result.snapshotId", Err: errors.New("snapshot is required")}
		}
		archiveOperation := op
		if env.CheckpointQuotaReserved {
			archiveOperation.CheckpointName = env.CheckpointQuotaName
			archiveOperation.CheckpointPinned = env.PinnedCheckpointQuotaReserved
		}
		checkpoint := checkpointForArchive(archiveOperation, env, req.Result.SnapshotID, now)
		if err = s.prepareCheckpointRelease(ctx, &mutation); err != nil {
			return MutationResult{}, err
		}
		mutation.Checkpoint = &checkpoint
		if !env.CheckpointQuotaReserved {
			mutation.CheckpointDelta = 1
			if checkpoint.Pinned {
				mutation.PinnedCheckpointDelta = 1
			}
		}
		mutation.Environment.CurrentCheckpoint = checkpoint.ID
		mutation.Environment.CurrentSnapshotID = checkpoint.SnapshotID
		mutation.Environment.InstanceID = ""
		mutation.Environment.WorkspaceVolumeID = ""
		mutation.Environment.PrivateUpstream = ""
		mutation.Environment.Host = ""
		mutation.Environment.URL = ""
		mutation.Environment.CheckpointQuotaReserved = false
		mutation.Environment.PinnedCheckpointQuotaReserved = false
		mutation.Environment.CheckpointQuotaName = ""
		releaseAllSlots(&mutation)
	case StateDeleted:
		if err = s.prepareCheckpointRelease(ctx, &mutation); err != nil {
			return MutationResult{}, err
		}
		mutation.Environment.InstanceID = ""
		mutation.Environment.WorkspaceVolumeID = ""
		mutation.Environment.CurrentCheckpoint = ""
		mutation.Environment.CurrentSnapshotID = ""
		mutation.Environment.PrivateUpstream = ""
		mutation.Environment.Host = ""
		mutation.Environment.URL = ""
		releaseCheckpointQuotaReservation(&mutation, env)
		releaseAllSlots(&mutation)
	}
	result, err := s.store.Complete(ctx, mutation)
	if err != nil {
		return MutationResult{}, err
	}
	if mutation.ScheduleExpiry {
		err = s.scheduleLease(ctx, result.Environment)
	}
	return result, err
}

func (s *Service) CompleteCheckpointDelete(ctx context.Context, identity Identity, operationID string,
	claimToken string, workflowErr error) (Operation, error) {
	if err := identity.Validate(); err != nil || !identity.Internal {
		return Operation{}, ErrUnauthorized
	}
	op, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return Operation{}, err
	}
	checkpoint, err := s.store.GetCheckpoint(ctx, op.CheckpointID)
	if err != nil {
		return Operation{}, err
	}
	if op.ClaimToken == "" || op.ClaimToken != claimToken {
		return Operation{}, ErrConflict
	}
	return s.store.CompleteCheckpointDelete(ctx, op, checkpoint, s.clock.Now().UTC(), workflowErr)
}

func (s *Service) BrowserLink(ctx context.Context, identity Identity, id string) (BrowserLink, error) {
	env, err := s.Get(ctx, identity, id)
	if err != nil {
		return BrowserLink{}, err
	}
	return s.cloud.BrowserLink(ctx, identity, env)
}

func (s *Service) SourceUpload(ctx context.Context, identity Identity) (SourceUpload, error) {
	if err := identity.Validate(); err != nil {
		return SourceUpload{}, errors.Join(ErrUnauthorized, err)
	}
	return s.cloud.SourceUpload(ctx, identity)
}

func (s *Service) Activity(ctx context.Context, identity Identity, id string,
	req ActivityRequest) (Environment, error) {
	if err := identity.Validate(); err != nil {
		return Environment{}, errors.Join(ErrUnauthorized, err)
	}
	switch req.Kind {
	case "terminal", "preview", "job":
	default:
		return Environment{}, &FieldError{Field: "kind", Err: errors.New("must be terminal, preview, or job")}
	}
	env, err := s.store.GetEnvironment(ctx, id)
	if err != nil {
		return Environment{}, err
	}
	if env.State != StateRunning {
		return Environment{}, errors.Join(ErrConflict, fmt.Errorf("cannot record activity in %s", env.State))
	}
	instanceCaller := env.InstanceID != "" && env.InstanceRoleARN != "" &&
		identity.SessionName == env.InstanceID && identity.RoleARN == env.InstanceRoleARN
	if identity.PrincipalID != env.OwnerID && !instanceCaller && !identity.Internal {
		return Environment{}, ErrNotFound
	}
	now := s.clock.Now().UTC()
	lease, err := env.Lease.RecordActivity(now)
	if err != nil {
		return Environment{}, err
	}
	env, err = s.store.RecordActivity(ctx, id, env.Version, lease, now, req.Kind)
	if err != nil {
		return Environment{}, err
	}
	return env, s.scheduleLease(ctx, env)
}

func (s *Service) ExpireLease(ctx context.Context, event LeaseExpiry) (*Operation, error) {
	env, err := s.store.GetEnvironment(ctx, event.EnvironmentID)
	if err != nil {
		return nil, err
	}
	if env.Lease.Version != event.LeaseVersion {
		return nil, nil
	}
	now := s.clock.Now().UTC()
	dueAt := env.ScheduledActionAt()
	if now.Before(dueAt) {
		return nil, nil
	}
	if env.ActiveOperationID != "" {
		op, getErr := s.store.GetOperation(ctx, env.ActiveOperationID)
		if getErr != nil {
			return nil, getErr
		}
		_, dispatchErr := s.dispatch(ctx, MutationResult{Environment: env, Operation: op})
		return &op, dispatchErr
	}
	var action Action
	switch env.State {
	case StateRunning:
		action = ActionStop
	case StateStopped:
		action = ActionArchive
	case StateError:
		action, err = recoveryAction(env, now)
		if err != nil {
			return nil, err
		}
	case StateArchived, StateDeleted, StateDeleting:
		return nil, nil
	default:
		return nil, fmt.Errorf("environment in %s has no active operation to recover", env.State)
	}
	identity := Identity{PrincipalID: "system:lease", Source: IdentitySourceSystem, Internal: true}
	key := fmt.Sprintf("lease/%s/%d/%d/%s", env.ID, env.Lease.Version, env.Version, action)
	result, err := s.begin(ctx, identity, env.ID, action, nil, struct{}{}, key, false)
	if err != nil {
		return nil, err
	}
	return &result.Operation, nil
}

func (s *Service) Reconcile(ctx context.Context, identity Identity, limit int32) (ReconcileResult, error) {
	if err := identity.Validate(); err != nil || !identity.Internal {
		return ReconcileResult{}, errors.Join(ErrUnauthorized, err)
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	result := ReconcileResult{}
	var failures []error
	addFailure := func(label string, err error) {
		if err == nil {
			return
		}
		wrapped := fmt.Errorf("%s: %w", label, err)
		result.Failures = append(result.Failures, wrapped.Error())
		failures = append(failures, wrapped)
	}

	addFailure("quota reconciliation", s.store.ReconcileQuotas(ctx))

	pendingOperations, operationsErr := s.store.ListPendingOperations(ctx, limit)
	addFailure("list pending operations", operationsErr)
	if operationsErr == nil {
		for _, operation := range pendingOperations {
			env, getErr := s.store.GetEnvironment(ctx, operation.EnvironmentID)
			if getErr != nil && operation.Action != ActionDeleteCheckpoint {
				addFailure(operation.ID, getErr)
				continue
			}
			_, dispatchErr := s.dispatch(ctx, MutationResult{Environment: env, Operation: operation})
			addFailure(operation.ID, dispatchErr)
		}
	}

	pendingSchedules, schedulesErr := s.store.ListPendingLeaseSchedules(ctx, limit)
	addFailure("list pending lease schedules", schedulesErr)
	if schedulesErr == nil {
		for _, env := range pendingSchedules {
			addFailure(env.ID, s.scheduleLease(ctx, env))
		}
	}

	due, dueErr := s.store.ListDue(ctx, s.clock.Now(), limit)
	addFailure("list due resources", dueErr)
	if dueErr == nil {
		for _, item := range due {
			var op Operation
			if item.Environment != nil {
				expired, expiryErr := s.ExpireLease(ctx, LeaseExpiry{
					EnvironmentID: item.Environment.ID, LeaseVersion: item.Environment.Lease.Version,
				})
				if expiryErr == nil && expired != nil {
					op = *expired
				} else if expiryErr != nil && !errors.Is(expiryErr, ErrConflict) {
					addFailure(item.Environment.ID, expiryErr)
				}
			} else if item.Checkpoint != nil {
				checkpoint := *item.Checkpoint
				var deleteErr error
				if checkpoint.ReferenceCount == 0 {
					key := fmt.Sprintf("reconcile/delete-checkpoint/%s/%d", checkpoint.ID, checkpoint.CreatedAt.Unix())
					op, deleteErr = s.deleteCheckpointAs(ctx, identity, checkpoint, key)
				} else {
					op, deleteErr = s.expireArchivedEnvironment(ctx, identity, checkpoint)
				}
				if deleteErr != nil && !errors.Is(deleteErr, ErrConflict) {
					addFailure(checkpoint.ID, deleteErr)
				}
			}
			if op.ID != "" {
				result.QueuedOperationIDs = append(result.QueuedOperationIDs, op.ID)
			}
		}
	}
	return result, errors.Join(failures...)
}

func (s *Service) expireArchivedEnvironment(ctx context.Context, identity Identity,
	checkpoint Checkpoint) (Operation, error) {
	env, err := s.store.GetEnvironment(ctx, checkpoint.EnvironmentID)
	if err != nil {
		return Operation{}, err
	}
	if env.State != StateArchived || env.CurrentCheckpoint != checkpoint.ID ||
		env.CurrentSnapshotID != checkpoint.SnapshotID {
		return Operation{}, nil
	}
	key := fmt.Sprintf("reconcile/delete-archived/%s/%d", checkpoint.ID, checkpoint.CreatedAt.Unix())
	result, err := s.begin(ctx, identity, env.ID, ActionDelete, nil, struct{}{}, key, false)
	return result.Operation, err
}

func (s *Service) begin(ctx context.Context, identity Identity, id string, action Action, profile *Profile,
	payload any, idempotencyKey string, enforceOwner bool) (MutationResult, error) {
	if err := identity.Validate(); err != nil {
		return MutationResult{}, errors.Join(ErrUnauthorized, err)
	}
	if err := ValidateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, &FieldError{Field: "Idempotency-Key", Err: err}
	}
	hash, err := HashMutation(action, payload)
	if err != nil {
		return MutationResult{}, err
	}
	now := s.clock.Now().UTC()
	idempotency := Idempotency{OwnerID: identity.PrincipalID, Key: idempotencyKey,
		RequestHash: hash, ExpiresAt: now.Add(IdempotencyTTL)}
	replayed, ok, err := s.store.ReplayMutation(ctx, idempotency)
	if err != nil {
		return MutationResult{}, err
	}
	if ok {
		return s.resume(ctx, replayed)
	}
	env, err := s.store.GetEnvironment(ctx, id)
	if err != nil {
		return MutationResult{}, err
	}
	if enforceOwner && env.OwnerID != identity.PrincipalID {
		return MutationResult{}, ErrNotFound
	}
	if env.ActiveOperationID != "" {
		return MutationResult{}, errors.Join(ErrConflict, errors.New("another operation is active"))
	}
	startProfile := env.Profile
	if profile != nil {
		startProfile = *profile
	}
	if action == ActionStart && !s.profileEnabled(startProfile) {
		return MutationResult{}, &FieldError{Field: "runtimeProfile", Err: ErrNotReady}
	}
	if action == ActionStart && profile != nil && *profile != env.Profile &&
		(env.State == StateStopped || env.State == StateError || env.InstanceID != "" || env.WorkspaceVolumeID != "") {
		return MutationResult{}, errors.Join(ErrConflict,
			errors.New("archive before changing the runtime profile"))
	}
	next, err := Transition(env.State, action)
	if err != nil {
		return MutationResult{}, errors.Join(ErrConflict, err)
	}
	opID, err := newID("op")
	if err != nil {
		return MutationResult{}, err
	}
	var lease *Lease
	if action == ActionStart {
		fresh := NewLease(now)
		fresh.Version = env.Lease.Version + 1
		lease = &fresh
	}
	op := newOperation(opID, id, "", identity, action, idempotencyKey, hash, now)
	if action == ActionArchive {
		if archive, ok := payload.(ArchiveRequest); ok {
			op.CheckpointName = archive.CheckpointName
			op.CheckpointPinned = archive.Pinned
		}
	}
	if !enforceOwner {
		op.OwnerID = env.OwnerID
	}
	result, err := s.store.Begin(ctx, BeginMutation{
		EnvironmentID: id, OwnerID: env.OwnerID, ExpectedState: env.State, ExpectedVersion: env.Version,
		NextState:      next,
		CurrentProfile: env.Profile,
		Profile:        profile, Operation: op,
		Lease:       lease,
		Idempotency: idempotency,
	}, s.quotas)
	if err != nil || result.Replayed {
		return result, err
	}
	result, err = s.dispatch(ctx, result)
	if err != nil {
		return MutationResult{}, err
	}
	if lease != nil {
		err = s.scheduleLease(ctx, result.Environment)
	}
	return result, err
}

func (s *Service) profileEnabled(profile Profile) bool {
	return profile != ProfileGPUCVML || s.quotas.MaxGPURunning > 0
}

func (s *Service) deleteCheckpointAs(ctx context.Context, identity Identity, checkpoint Checkpoint,
	idempotencyKey string) (Operation, error) {
	if checkpoint.ReferenceCount != 0 {
		return Operation{}, errors.Join(ErrConflict, errors.New("checkpoint is referenced by an environment"))
	}
	now := s.clock.Now().UTC()
	payload := struct {
		ID string `json:"id"`
	}{ID: checkpoint.ID}
	hash, err := HashMutation(ActionDeleteCheckpoint, payload)
	if err != nil {
		return Operation{}, err
	}
	opID, err := newID("op")
	if err != nil {
		return Operation{}, err
	}
	op := newOperation(opID, checkpoint.EnvironmentID, checkpoint.ID, identity,
		ActionDeleteCheckpoint, idempotencyKey, hash, now)
	op.OwnerID = checkpoint.OwnerID
	op, replayed, err := s.store.BeginCheckpointDelete(ctx, CheckpointMutation{
		Checkpoint: checkpoint, Operation: op,
		Idempotency: Idempotency{OwnerID: identity.PrincipalID, Key: idempotencyKey,
			RequestHash: hash, ExpiresAt: now.Add(IdempotencyTTL)},
	})
	if err != nil || replayed {
		return op, err
	}
	arn, err := s.workflow.Start(ctx, op)
	if err != nil {
		_ = s.store.FailDispatch(ctx, op, err)
		return Operation{}, err
	}
	err = s.store.SetOperationExecution(ctx, op.ID, arn)
	if err != nil {
		return Operation{}, err
	}
	op.ExecutionARN = arn
	return op, nil
}

func (s *Service) dispatch(ctx context.Context, result MutationResult) (MutationResult, error) {
	arn, err := s.workflow.Start(ctx, result.Operation)
	if err != nil {
		_ = s.store.FailDispatch(ctx, result.Operation, err)
		return MutationResult{}, err
	}
	err = s.store.SetOperationExecution(ctx, result.Operation.ID, arn)
	if err != nil {
		return MutationResult{}, err
	}
	result.Operation.ExecutionARN = arn
	return result, nil
}

func applyRuntimeResult(env *Environment, result WorkflowResult) {
	if result.InstanceID != "" {
		env.InstanceID = result.InstanceID
	}
	if result.InstanceRoleARN != "" {
		env.InstanceRoleARN = result.InstanceRoleARN
	}
	if result.WorkspaceVolumeID != "" {
		env.WorkspaceVolumeID = result.WorkspaceVolumeID
	}
	if result.Host != "" {
		env.Host = result.Host
		env.URL = "https://" + result.Host + "/a/"
	}
	if result.PrivateUpstream != "" {
		env.PrivateUpstream = result.PrivateUpstream
	}
	if result.TLSCertSHA256 != "" {
		env.TLSCertSHA256 = strings.ToLower(result.TLSCertSHA256)
	}
	if result.LogtoAppID != "" {
		env.LogtoAppID = result.LogtoAppID
	}
}

func shouldResetRecovery(env Environment, operation Operation) bool {
	if env.RecoveryStartedAt == nil {
		return true
	}
	return operation.ActorPrincipal != "system:lease"
}

func errorRecoveryDeadline(env Environment, startedAt time.Time) time.Time {
	deadline := startedAt.Add(DefaultErrorRecoveryTTL)
	leaseDeadline := env.Lease.DueAt()
	if !leaseDeadline.IsZero() && leaseDeadline.Before(deadline) {
		deadline = leaseDeadline
	}
	return deadline
}

func recoveryAction(env Environment, now time.Time) (Action, error) {
	if env.FailedAction != ActionDelete && env.CleanupAfter != nil && !now.Before(*env.CleanupAfter) {
		return ActionArchive, nil
	}
	switch env.FailedAction {
	case ActionCreate, ActionStart:
		return ActionStart, nil
	case ActionStop:
		return ActionStop, nil
	case ActionArchive:
		return ActionArchive, nil
	case ActionDelete:
		return ActionDelete, nil
	default:
		return "", errors.New("error environment has no recovery action")
	}
}

func (s *Service) prepareCheckpointRelease(ctx context.Context, mutation *CompletionMutation) error {
	id := mutation.Environment.CurrentCheckpoint
	if id == "" {
		return nil
	}
	checkpoint, err := s.store.GetCheckpoint(ctx, id)
	if err != nil {
		return err
	}
	if checkpoint.OwnerID != mutation.Environment.OwnerID || checkpoint.State != CheckpointAvailable ||
		checkpoint.ReferenceCount < 1 || checkpoint.SnapshotID == "" ||
		(mutation.Environment.CurrentSnapshotID != "" &&
			checkpoint.SnapshotID != mutation.Environment.CurrentSnapshotID) {
		return errors.Join(ErrConflict, errors.New("environment checkpoint reference is invalid"))
	}
	mutation.ReleaseCheckpoint = &checkpoint
	return nil
}

func gatewayRoute(env Environment) *GatewayRoute {
	route := &GatewayRoute{
		WorkspaceID: env.ID, Name: env.Name, Host: env.Host, Upstream: env.PrivateUpstream,
		State: StateRunning, ACLVersion: env.ACLVersion, Visibility: env.Visibility,
		TLSCertSHA256: env.TLSCertSHA256,
	}
	if env.Visibility == VisibilityRestricted {
		route.AllowedPrincipals = []string{env.OwnerID}
	}
	return route
}

func checkpointForArchive(op Operation, env Environment, snapshotID string, now time.Time) Checkpoint {
	name := op.CheckpointName
	if name == "" {
		nameTime := op.RequestedAt
		if nameTime.IsZero() {
			nameTime = now
		}
		owner := strings.Split(env.OwnerEmail, "@")[0]
		owner = sanitizeNamePart(owner)
		if owner == "" {
			owner = "owner"
		}
		name = owner + "-" + env.Name + "-" + nameTime.UTC().Format("20060102-150405")
		if len(name) > 64 {
			name = strings.TrimRight(name[:64], "-._")
		}
	}
	checkpoint := Checkpoint{
		ID: "checkpoint-" + op.ID, EnvironmentID: env.ID, OwnerID: env.OwnerID,
		Name: name, SnapshotID: snapshotID, State: CheckpointAvailable,
		Pinned: op.CheckpointPinned, ReferenceCount: 1, CreatedAt: now,
	}
	if !checkpoint.Pinned {
		expires := CheckpointDeadline(now)
		checkpoint.ExpiresAt = &expires
	}
	return checkpoint
}

func sanitizeNamePart(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			result.WriteRune(character)
		} else if result.Len() > 0 && !strings.HasSuffix(result.String(), "-") {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

func releaseRunningSlots(env *Environment) (int, int) {
	running, gpu := 0, 0
	if env.RunningSlot {
		running = -1
		env.RunningSlot = false
	}
	if env.GPURunningSlot {
		gpu = -1
		env.GPURunningSlot = false
	}
	return running, gpu
}

func releaseAllSlots(mutation *CompletionMutation) {
	mutation.RunningDelta, mutation.GPURunningDelta = releaseRunningSlots(&mutation.Environment)
	if mutation.Environment.WorkspaceSlot {
		mutation.OwnerWorkspaceDelta = -1
		mutation.GlobalWorkspaceDelta = -1
		mutation.Environment.WorkspaceSlot = false
	}
}

func releaseCheckpointQuotaReservation(mutation *CompletionMutation, env Environment) {
	if !env.CheckpointQuotaReserved {
		return
	}
	mutation.CheckpointDelta = -1
	if env.PinnedCheckpointQuotaReserved {
		mutation.PinnedCheckpointDelta = -1
	}
	mutation.Environment.CheckpointQuotaReserved = false
	mutation.Environment.PinnedCheckpointQuotaReserved = false
	mutation.Environment.CheckpointQuotaName = ""
}

func (s *Service) resume(ctx context.Context, result MutationResult) (MutationResult, error) {
	var err error
	if result.Operation.Status == OperationQueued && result.Operation.ExecutionARN == "" &&
		result.Operation.Action != ActionExtend {
		result, err = s.dispatch(ctx, result)
		if err != nil {
			return MutationResult{}, err
		}
	}
	switch result.Operation.Action {
	case ActionCreate, ActionStart, ActionExtend:
		err = s.scheduleLease(ctx, result.Environment)
	}
	return result, err
}

func (s *Service) scheduleLease(ctx context.Context, env Environment) error {
	for attempt := 0; attempt < 3; attempt++ {
		if err := s.workflow.ScheduleLease(ctx, env); err != nil {
			return err
		}
		err := s.store.AckLeaseSchedule(ctx, env.ID, env.Lease.Version)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		current, getErr := s.store.GetEnvironment(ctx, env.ID)
		if getErr != nil {
			return errors.Join(err, getErr)
		}
		if current.Lease.Version == env.Lease.Version {
			return nil
		}
		env = current
	}
	return errors.Join(ErrConflict, errors.New("lease schedule changed repeatedly during repair"))
}

func HashMutation(action Action, payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(action+"\n"), body...))
	return hex.EncodeToString(digest[:]), nil
}

func newIdempotency(identity Identity, key, hash string, now time.Time) Idempotency {
	return Idempotency{OwnerID: identity.PrincipalID, Key: key, RequestHash: hash, ExpiresAt: now.Add(IdempotencyTTL)}
}

func newOperation(id, envID, checkpointID string, identity Identity, action Action,
	key, hash string, now time.Time) Operation {
	return Operation{
		ID: id, EnvironmentID: envID, CheckpointID: checkpointID, OwnerID: identity.PrincipalID,
		ActorPrincipal: identity.PrincipalID, Action: action, Status: OperationQueued,
		RequestedAt: now, UpdatedAt: now, IdempotencyKey: key, RequestHash: hash,
		IdempotencyEnds: now.Add(IdempotencyTTL),
	}
}

func newID(prefix string) (string, error) {
	random := make([]byte, 16)
	_, err := rand.Read(random)
	if err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(random), nil
}

func ValidateIdempotencyKey(key string) error {
	if len(key) < 8 || len(key) > 128 || strings.TrimSpace(key) != key {
		return errors.New("Idempotency-Key must contain 8-128 non-space-edge characters")
	}
	return nil
}
