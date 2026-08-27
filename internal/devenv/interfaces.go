package devenv

import (
	"context"
	"errors"
	"time"
)

type Idempotency struct {
	OwnerID     string
	Key         string
	RequestHash string
	ExpiresAt   time.Time
}

type QuotaLimits struct {
	MaxOwnerWorkspaces        int
	MaxWorkspaces             int
	MaxRunning                int
	MaxGPURunning             int
	MaxOwnerCheckpoints       int
	MaxOwnerPinnedCheckpoints int
	MaxCheckpoints            int
	MaxPinnedCheckpoints      int
}

func (q QuotaLimits) Validate() error {
	if q.MaxOwnerWorkspaces < 1 || q.MaxWorkspaces < 1 || q.MaxRunning < 1 || q.MaxGPURunning < 0 ||
		q.MaxOwnerCheckpoints < 1 || q.MaxOwnerPinnedCheckpoints < 1 || q.MaxCheckpoints < 1 ||
		q.MaxPinnedCheckpoints < 1 {
		return errors.New("quota limits must be positive")
	}
	if q.MaxOwnerWorkspaces > q.MaxWorkspaces || q.MaxGPURunning > q.MaxRunning ||
		q.MaxOwnerCheckpoints > q.MaxCheckpoints ||
		q.MaxOwnerPinnedCheckpoints > q.MaxOwnerCheckpoints ||
		q.MaxOwnerPinnedCheckpoints > q.MaxPinnedCheckpoints || q.MaxPinnedCheckpoints > q.MaxCheckpoints {
		return errors.New("quota limits are inconsistent")
	}
	return nil
}

func DefaultQuotaLimits() QuotaLimits {
	return QuotaLimits{
		MaxOwnerWorkspaces:        DefaultMaxOwnerSlots,
		MaxWorkspaces:             DefaultMaxWorkspaces,
		MaxRunning:                DefaultMaxRunning,
		MaxGPURunning:             DefaultMaxGPURunning,
		MaxOwnerCheckpoints:       DefaultMaxOwnerCheckpoints,
		MaxOwnerPinnedCheckpoints: DefaultMaxOwnerPinnedCheckpoints,
		MaxCheckpoints:            DefaultMaxCheckpoints,
		MaxPinnedCheckpoints:      DefaultMaxPinnedCheckpoints,
	}
}

type MutationResult struct {
	Environment Environment
	Operation   Operation
	Replayed    bool
}

type CreateMutation struct {
	Environment      Environment
	Operation        Operation
	Idempotency      Idempotency
	SourceCheckpoint *Checkpoint
}

type BeginMutation struct {
	EnvironmentID   string
	OwnerID         string
	ExpectedState   State
	ExpectedVersion int64
	NextState       State
	CurrentProfile  Profile
	Profile         *Profile
	Lease           *Lease
	Operation       Operation
	Idempotency     Idempotency
}

type ExtendMutation struct {
	EnvironmentID   string
	OwnerID         string
	ExpectedState   State
	ExpectedLease   int64
	ExpectedVersion int64
	Lease           Lease
	Operation       Operation
	Idempotency     Idempotency
}

type CheckpointMutation struct {
	Checkpoint  Checkpoint
	Operation   Operation
	Idempotency Idempotency
}

type CompletionMutation struct {
	Environment                Environment
	Operation                  Operation
	ExpectedEnvironmentState   State
	ExpectedEnvironmentVersion int64
	ExpectedOperationStatus    OperationStatus
	ExpectedOperationClaim     string
	Checkpoint                 *Checkpoint
	ReleaseCheckpoint          *Checkpoint
	Route                      *GatewayRoute
	DeleteRouteHost            string
	OwnerWorkspaceDelta        int
	GlobalWorkspaceDelta       int
	RunningDelta               int
	GPURunningDelta            int
	CheckpointDelta            int
	PinnedCheckpointDelta      int
	ScheduleExpiry             bool
	QuotaLimits                QuotaLimits
}

type DueItem struct {
	Environment *Environment
	Checkpoint  *Checkpoint
}

type Store interface {
	ReplayMutation(context.Context, Idempotency) (MutationResult, bool, error)
	Create(context.Context, CreateMutation, QuotaLimits) (MutationResult, error)
	Begin(context.Context, BeginMutation, QuotaLimits) (MutationResult, error)
	Extend(context.Context, ExtendMutation) (MutationResult, error)
	RecordActivity(context.Context, string, int64, Lease, time.Time, string) (Environment, error)
	GetEnvironment(context.Context, string) (Environment, error)
	ListEnvironments(context.Context, string) ([]Environment, error)
	ListAllEnvironments(context.Context) ([]Environment, error)
	GetOperation(context.Context, string) (Operation, error)
	ClaimOperation(context.Context, string, string, time.Time, time.Duration, int, time.Duration) (Operation, bool, bool, error)
	GetCheckpoint(context.Context, string) (Checkpoint, error)
	ListCheckpoints(context.Context, string) ([]Checkpoint, error)
	ListAllCheckpoints(context.Context) ([]Checkpoint, error)
	BeginCheckpointDelete(context.Context, CheckpointMutation) (Operation, bool, error)
	Complete(context.Context, CompletionMutation) (MutationResult, error)
	CompleteCheckpointDelete(context.Context, Operation, Checkpoint, time.Time, error) (Operation, error)
	ListDue(context.Context, time.Time, int32) ([]DueItem, error)
	ListPendingOperations(context.Context, int32) ([]Operation, error)
	ListPendingLeaseSchedules(context.Context, int32) ([]Environment, error)
	AckLeaseSchedule(context.Context, string, int64) error
	ReconcileQuotas(context.Context) error
	SetOperationExecution(context.Context, string, string) error
	FailDispatch(context.Context, Operation, error) error
}

type Workflow interface {
	Start(context.Context, Operation) (string, error)
	ScheduleLease(context.Context, Environment) error
}

type Cloud interface {
	BrowserLink(context.Context, Identity, Environment) (BrowserLink, error)
	SourceUpload(context.Context, Identity) (SourceUpload, error)
}

type Clock interface {
	Now() time.Time
}

type WallClock struct{}

func (WallClock) Now() time.Time {
	return time.Now().UTC()
}

type UnavailableCloud struct{}

func (UnavailableCloud) BrowserLink(context.Context, Identity, Environment) (BrowserLink, error) {
	return BrowserLink{}, errors.Join(ErrNotReady, errors.New("browser-link provider is not configured"))
}

func (UnavailableCloud) SourceUpload(context.Context, Identity) (SourceUpload, error) {
	return SourceUpload{}, errors.Join(ErrNotReady, errors.New("source uploader is not configured"))
}
