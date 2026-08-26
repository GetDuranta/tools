package devenvgateway

import (
	"context"
	"errors"
	"time"

	"github.com/GetDuranta/tools/internal/devaccess"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = devaccess.ErrConflict
)

const (
	WorkspaceStateRunning         = "RUNNING"
	WorkspaceVisibilityOrg        = "organization"
	WorkspaceVisibilityRestricted = "restricted"
)

type Identity struct {
	Subject    string
	Email      string
	OwnerID    string
	Principals []string
	OIDCData   string
}

type Workspace struct {
	ID                string
	Name              string
	Host              string
	Upstream          string
	TLSCertSHA256     string
	State             string
	Visibility        string
	ACLVersion        int64
	AllowedPrincipals []string
}

func (w Workspace) Running() bool { return w.State == WorkspaceStateRunning }

func (w Workspace) Allows(principals []string) bool {
	if len(principals) == 0 {
		return false
	}
	if w.Visibility == WorkspaceVisibilityOrg {
		return true
	}
	return w.Visibility == WorkspaceVisibilityRestricted && intersects(w.AllowedPrincipals, principals)
}

type BootstrapGrant = devaccess.BootstrapGrant

type Session struct {
	TokenHash   string
	WorkspaceID string
	Host        string
	Subject     string
	Principals  []string
	ACLVersion  int64
	ExpiresAt   time.Time
}

type Store interface {
	devaccess.GrantWriter
	WorkspaceForPrincipals(context.Context, string, []string) (Workspace, error)
	ConsumeBootstrap(context.Context, string, string, Session, time.Time) (BootstrapGrant, error)
	AuthorizedWorkspace(context.Context, string, string, time.Time) (Workspace, error)
}

type IdentityVerifier interface {
	Verify(context.Context, string) (Identity, error)
}

type EnvironmentSummary struct {
	ID         string
	Name       string
	Host       string
	State      string
	OwnerEmail string
	ExpiresAt  time.Time
}

type CheckpointSummary struct {
	ID        string
	Name      string
	State     string
	Pinned    bool
	ExpiresAt *time.Time
}

type ControlPlane interface {
	List(context.Context, Identity) ([]EnvironmentSummary, error)
	ListCheckpoints(context.Context, Identity) ([]CheckpointSummary, error)
	Start(context.Context, Identity, string, string) error
	Extend(context.Context, Identity, string, time.Duration, string) error
	Stop(context.Context, Identity, string, string) error
	Archive(context.Context, Identity, string, string) error
	Delete(context.Context, Identity, string, string) error
	DeleteCheckpoint(context.Context, Identity, string, string) error
	Activity(context.Context, string) error
}
