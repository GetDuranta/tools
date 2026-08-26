package devenv

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultIdleTTL                   = 4 * time.Hour
	DefaultHardTTL                   = 24 * time.Hour
	DefaultStoppedTTL                = 7 * 24 * time.Hour
	DefaultCheckpointTTL             = 30 * 24 * time.Hour
	DefaultErrorCleanupTTL           = 15 * time.Minute
	DefaultErrorRecoveryTTL          = 6 * time.Hour
	DefaultArchiveRetryTTL           = time.Hour
	DefaultOperationClaimTTL         = 40 * time.Minute
	DefaultOperationMaxAge           = 6 * time.Hour
	DefaultOperationAttempts         = 30
	DefaultMaxOwnerSlots             = 10
	DefaultMaxWorkspaces             = 50
	DefaultMaxRunning                = 50
	DefaultMaxGPURunning             = 10
	DefaultMaxOwnerCheckpoints       = 30
	DefaultMaxOwnerPinnedCheckpoints = 10
	DefaultMaxCheckpoints            = 500
	DefaultMaxPinnedCheckpoints      = 100
	IdempotencyTTL                   = 24 * time.Hour
	MinExtension                     = time.Hour
	MaxExtension                     = 4 * time.Hour
)

var displayNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,62}[a-zA-Z0-9]$`)

type State string

const (
	StateProvisioning State = "PROVISIONING"
	StateRunning      State = "RUNNING"
	StateStopping     State = "STOPPING"
	StateStopped      State = "STOPPED"
	StateStarting     State = "STARTING"
	StateArchiving    State = "ARCHIVING"
	StateArchived     State = "ARCHIVED"
	StateDeleting     State = "DELETING"
	StateDeleted      State = "DELETED"
	StateError        State = "ERROR"
)

func (s State) Validate() error {
	switch s {
	case StateProvisioning, StateRunning, StateStopping, StateStopped, StateStarting,
		StateArchiving, StateArchived, StateDeleting, StateDeleted, StateError:
		return nil
	default:
		return fmt.Errorf("invalid environment state %q", s)
	}
}

func (s State) HoldsWorkspace() bool {
	switch s {
	case StateArchived, StateDeleted:
		return false
	default:
		return true
	}
}

type Profile string

const (
	ProfileStandard Profile = "standard"
	ProfileGPUCVML  Profile = "gpu-cvml"
)

func (p Profile) Validate() error {
	switch p {
	case ProfileStandard, ProfileGPUCVML:
		return nil
	default:
		return fmt.Errorf("invalid runtime profile %q", p)
	}
}

type Action string

const (
	ActionCreate           Action = "create"
	ActionStart            Action = "start"
	ActionExtend           Action = "extend"
	ActionStop             Action = "stop"
	ActionArchive          Action = "archive"
	ActionDelete           Action = "delete"
	ActionDeleteCheckpoint Action = "delete_checkpoint"
)

type OperationStatus string

const (
	OperationQueued    OperationStatus = "QUEUED"
	OperationRunning   OperationStatus = "RUNNING"
	OperationSucceeded OperationStatus = "SUCCEEDED"
	OperationFailed    OperationStatus = "FAILED"
)

type CheckpointState string

const (
	CheckpointAvailable CheckpointState = "AVAILABLE"
	CheckpointDeleting  CheckpointState = "DELETING"
	CheckpointDeleted   CheckpointState = "DELETED"
)

type IdentitySource string

const (
	IdentitySourceAWSIAM  IdentitySource = "aws_iam"
	IdentitySourceALBOIDC IdentitySource = "alb_oidc"
	IdentitySourceSystem  IdentitySource = "system"
)

type Identity struct {
	PrincipalID  string         `json:"principalId"`
	Source       IdentitySource `json:"source"`
	Email        string         `json:"email,omitempty"`
	AuditSubject string         `json:"auditSubject,omitempty"`
	AccountID    string         `json:"accountId,omitempty"`
	SessionName  string         `json:"sessionName,omitempty"`
	RoleARN      string         `json:"roleArn,omitempty"`
	Internal     bool           `json:"-"`
}

type Visibility string

const (
	VisibilityOrganization Visibility = "organization"
	VisibilityRestricted   Visibility = "restricted"
)

func (v Visibility) Validate() error {
	switch v {
	case VisibilityOrganization, VisibilityRestricted:
		return nil
	default:
		return fmt.Errorf("invalid visibility %q", v)
	}
}

func (i Identity) Validate() error {
	if i.PrincipalID == "" || i.Source == "" {
		return errors.New("incomplete authenticated identity")
	}
	switch i.Source {
	case IdentitySourceAWSIAM, IdentitySourceALBOIDC, IdentitySourceSystem:
		return nil
	default:
		return errors.New("invalid authenticated identity source")
	}
}

type Source struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	BundleKey  string `json:"bundleKey,omitempty"`
}

func (s Source) Validate() error {
	if strings.TrimSpace(s.Repository) == "" {
		return errors.New("source repository is required")
	}
	if strings.TrimSpace(s.Ref) == "" && strings.TrimSpace(s.BundleKey) == "" {
		return errors.New("source ref or bundleKey is required")
	}
	return nil
}

type Lease struct {
	IdleExpiresAt time.Time `json:"idleExpiresAt"`
	HardExpiresAt time.Time `json:"hardExpiresAt"`
	Version       int64     `json:"version"`
}

type Environment struct {
	ID                            string     `json:"id"`
	OwnerID                       string     `json:"ownerId"`
	OwnerSubject                  string     `json:"ownerSubject,omitempty"`
	OwnerAccountID                string     `json:"ownerAccountId"`
	OwnerEmail                    string     `json:"ownerEmail,omitempty"`
	Name                          string     `json:"name"`
	State                         State      `json:"state"`
	Profile                       Profile    `json:"runtimeProfile"`
	Visibility                    Visibility `json:"visibility"`
	Source                        Source     `json:"source"`
	Lease                         Lease      `json:"lease"`
	Version                       int64      `json:"version"`
	ActiveOperationID             string     `json:"activeOperationId,omitempty"`
	InstanceID                    string     `json:"instanceId,omitempty"`
	InstanceRoleARN               string     `json:"instanceRoleArn,omitempty"`
	WorkspaceVolumeID             string     `json:"workspaceVolumeId,omitempty"`
	CurrentCheckpoint             string     `json:"currentCheckpointId,omitempty"`
	CurrentSnapshotID             string     `json:"-"`
	URL                           string     `json:"url,omitempty"`
	Host                          string     `json:"host,omitempty"`
	PrivateUpstream               string     `json:"-"`
	TLSCertSHA256                 string     `json:"-"`
	LogtoAppID                    string     `json:"logtoAppId,omitempty"`
	ACLVersion                    int64      `json:"aclVersion"`
	CreatedAt                     time.Time  `json:"createdAt"`
	UpdatedAt                     time.Time  `json:"updatedAt"`
	StoppedAt                     *time.Time `json:"stoppedAt,omitempty"`
	ArchiveAfter                  *time.Time `json:"archiveAfter,omitempty"`
	CleanupAfter                  *time.Time `json:"cleanupAfter,omitempty"`
	RecoveryStartedAt             *time.Time `json:"recoveryStartedAt,omitempty"`
	RecoveryRetryAfter            *time.Time `json:"recoveryRetryAfter,omitempty"`
	FailedAction                  Action     `json:"failedAction,omitempty"`
	DirtyShutdown                 bool       `json:"dirtyShutdown,omitempty"`
	LastMeaningfulAt              *time.Time `json:"lastMeaningfulActivityAt,omitempty"`
	LastMeaningfulKind            string     `json:"lastMeaningfulActivityKind,omitempty"`
	WorkspaceSlot                 bool       `json:"-"`
	RunningSlot                   bool       `json:"-"`
	GPURunningSlot                bool       `json:"-"`
	CheckpointQuotaReserved       bool       `json:"-"`
	PinnedCheckpointQuotaReserved bool       `json:"-"`
	CheckpointQuotaName           string     `json:"-"`
}

func (e Environment) Validate() error {
	if e.ID == "" || e.OwnerID == "" {
		return errors.New("environment id and owner are required")
	}
	if err := ValidateDisplayName(e.Name); err != nil {
		return err
	}
	if err := e.State.Validate(); err != nil {
		return err
	}
	if err := e.Profile.Validate(); err != nil {
		return err
	}
	if err := e.Visibility.Validate(); err != nil {
		return err
	}
	return e.Source.Validate()
}

type Operation struct {
	ID               string          `json:"id"`
	EnvironmentID    string          `json:"environmentId,omitempty"`
	CheckpointID     string          `json:"checkpointId,omitempty"`
	OwnerID          string          `json:"ownerId"`
	ActorPrincipal   string          `json:"actorPrincipalId"`
	Action           Action          `json:"action"`
	Status           OperationStatus `json:"status"`
	RequestedAt      time.Time       `json:"requestedAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	FirstAttemptAt   *time.Time      `json:"firstAttemptAt,omitempty"`
	ClaimUntil       *time.Time      `json:"claimUntil,omitempty"`
	ClaimToken       string          `json:"-"`
	AttemptCount     int             `json:"attemptCount"`
	ExecutionARN     string          `json:"executionArn,omitempty"`
	Error            string          `json:"error,omitempty"`
	CheckpointName   string          `json:"checkpointName,omitempty"`
	CheckpointPinned bool            `json:"checkpointPinned,omitempty"`
	IdempotencyKey   string          `json:"-"`
	RequestHash      string          `json:"-"`
	IdempotencyEnds  time.Time       `json:"-"`
}

type GatewayRoute struct {
	WorkspaceID       string
	Name              string
	Host              string
	Upstream          string
	State             State
	ACLVersion        int64
	Visibility        Visibility
	AllowedPrincipals []string
	TLSCertSHA256     string
}

type WorkflowResult struct {
	InstanceID        string `json:"instanceId,omitempty"`
	InstanceRoleARN   string `json:"instanceRoleArn,omitempty"`
	WorkspaceVolumeID string `json:"workspaceVolumeId,omitempty"`
	Host              string `json:"host,omitempty"`
	PrivateUpstream   string `json:"privateUpstream,omitempty"`
	TLSCertSHA256     string `json:"tlsCertSHA256,omitempty"`
	LogtoAppID        string `json:"logtoAppId,omitempty"`
	SnapshotID        string `json:"snapshotId,omitempty"`
	DirtyShutdown     bool   `json:"dirtyShutdown,omitempty"`
}

type Checkpoint struct {
	ID             string          `json:"id"`
	EnvironmentID  string          `json:"environmentId"`
	OwnerID        string          `json:"ownerId"`
	Name           string          `json:"name"`
	SnapshotID     string          `json:"snapshotId"`
	State          CheckpointState `json:"state"`
	Pinned         bool            `json:"pinned"`
	ReferenceCount int             `json:"referenceCount"`
	CreatedAt      time.Time       `json:"createdAt"`
	ExpiresAt      *time.Time      `json:"expiresAt,omitempty"`
}

type BrowserLink struct {
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type SourceUpload struct {
	BundleKey string            `json:"bundleKey"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

func ValidateDisplayName(name string) error {
	if !displayNamePattern.MatchString(name) {
		return errors.New("name must be 3-64 letters, digits, dots, underscores, or hyphens")
	}
	return nil
}
