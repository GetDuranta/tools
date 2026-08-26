package devenvgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GetDuranta/tools/internal/devaccess"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const gatewayControlPath = "/internal/v1/gateway/control"

type SigV4ControlPlane struct {
	endpoint    *url.URL
	credentials aws.CredentialsProvider
	region      string
	client      *http.Client
	signer      *v4.Signer
	now         func() time.Time
}

type controlEnvelope struct {
	ActorEmail    string `json:"actorEmail"`
	AuditSubject  string `json:"auditSubject,omitempty"`
	Action        string `json:"action"`
	EnvironmentID string `json:"environmentId,omitempty"`
	CheckpointID  string `json:"checkpointId,omitempty"`
	Hours         int    `json:"hours,omitempty"`
	OIDCData      string `json:"oidcData,omitempty"`
}

type controlListResponse struct {
	Environments []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Host       string `json:"host"`
		State      string `json:"state"`
		OwnerEmail string `json:"ownerEmail"`
		Lease      struct {
			IdleExpiresAt time.Time `json:"idleExpiresAt"`
			HardExpiresAt time.Time `json:"hardExpiresAt"`
		} `json:"lease"`
	} `json:"environments"`
}

type controlCheckpointResponse struct {
	Checkpoints []struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		State     string     `json:"state"`
		Pinned    bool       `json:"pinned"`
		ExpiresAt *time.Time `json:"expiresAt"`
	} `json:"checkpoints"`
}

func NewSigV4ControlPlane(endpoint string, credentials aws.CredentialsProvider, region string,
	client *http.Client) (*SigV4ControlPlane, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid control API URL")
	}
	if credentials == nil || strings.TrimSpace(region) == "" {
		return nil, errors.New("AWS credentials and region are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &SigV4ControlPlane{
		endpoint: parsed, credentials: credentials, region: region, client: &clientCopy,
		signer: v4.NewSigner(), now: time.Now,
	}, nil
}

func (c *SigV4ControlPlane) List(ctx context.Context, identity Identity) ([]EnvironmentSummary, error) {
	var response controlListResponse
	if err := c.call(ctx, identity, controlEnvelope{Action: "list"}, "", &response); err != nil {
		return nil, err
	}
	environments := make([]EnvironmentSummary, 0, len(response.Environments))
	for _, environment := range response.Environments {
		expiresAt := environment.Lease.IdleExpiresAt
		if expiresAt.IsZero() || (!environment.Lease.HardExpiresAt.IsZero() &&
			environment.Lease.HardExpiresAt.Before(expiresAt)) {
			expiresAt = environment.Lease.HardExpiresAt
		}
		environments = append(environments, EnvironmentSummary{
			ID: environment.ID, Name: environment.Name, Host: environment.Host, State: environment.State,
			OwnerEmail: environment.OwnerEmail, ExpiresAt: expiresAt,
		})
	}
	return environments, nil
}

func (c *SigV4ControlPlane) ListCheckpoints(ctx context.Context, identity Identity) ([]CheckpointSummary, error) {
	var response controlCheckpointResponse
	if err := c.call(ctx, identity, controlEnvelope{Action: "checkpoints"}, "", &response); err != nil {
		return nil, err
	}
	checkpoints := make([]CheckpointSummary, 0, len(response.Checkpoints))
	for _, checkpoint := range response.Checkpoints {
		checkpoints = append(checkpoints, CheckpointSummary{
			ID: checkpoint.ID, Name: checkpoint.Name, State: checkpoint.State,
			Pinned: checkpoint.Pinned, ExpiresAt: checkpoint.ExpiresAt,
		})
	}
	return checkpoints, nil
}

func (c *SigV4ControlPlane) Start(ctx context.Context, identity Identity, id,
	idempotencyKey string) error {
	return c.mutate(ctx, identity, "start", id, 0, idempotencyKey)
}

func (c *SigV4ControlPlane) Extend(ctx context.Context, identity Identity, id string,
	duration time.Duration, idempotencyKey string) error {
	if duration%time.Hour != 0 {
		return ErrNotFound
	}
	return c.mutate(ctx, identity, "extend", id, int(duration/time.Hour), idempotencyKey)
}

func (c *SigV4ControlPlane) Stop(ctx context.Context, identity Identity, id,
	idempotencyKey string) error {
	return c.mutate(ctx, identity, "stop", id, 0, idempotencyKey)
}

func (c *SigV4ControlPlane) Archive(ctx context.Context, identity Identity, id,
	idempotencyKey string) error {
	return c.mutate(ctx, identity, "archive", id, 0, idempotencyKey)
}

func (c *SigV4ControlPlane) Delete(ctx context.Context, identity Identity, id,
	idempotencyKey string) error {
	return c.mutate(ctx, identity, "delete", id, 0, idempotencyKey)
}

func (c *SigV4ControlPlane) DeleteCheckpoint(ctx context.Context, identity Identity, id,
	idempotencyKey string) error {
	if !environmentIDPattern.MatchString(id) || !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return ErrNotFound
	}
	return c.call(ctx, identity, controlEnvelope{Action: "delete-checkpoint", CheckpointID: id},
		idempotencyKey, nil)
}

func (c *SigV4ControlPlane) Activity(ctx context.Context, id string) error {
	if !environmentIDPattern.MatchString(id) {
		return ErrNotFound
	}
	return c.call(ctx, Identity{}, controlEnvelope{Action: "activity", EnvironmentID: id}, "", nil)
}

func (c *SigV4ControlPlane) mutate(ctx context.Context, identity Identity, action, id string,
	hours int, idempotencyKey string) error {
	if !environmentIDPattern.MatchString(id) || !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return ErrNotFound
	}
	return c.call(ctx, identity, controlEnvelope{
		Action: action, EnvironmentID: id, Hours: hours,
	}, idempotencyKey, nil)
}

func (c *SigV4ControlPlane) call(ctx context.Context, identity Identity, envelope controlEnvelope,
	idempotencyKey string, target any) error {
	if envelope.Action != "activity" {
		email, err := devaccess.NormalizeEmail(identity.Email)
		if err != nil || identity.Subject == "" {
			return ErrNotFound
		}
		envelope.ActorEmail = email
		envelope.AuditSubject = identity.Subject
		envelope.OIDCData = identity.OIDCData
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	endpoint := *c.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + gatewayControlPath
	endpoint.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	credentials, err := c.credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve AWS credentials: %w", err)
	}
	hash := sha256.Sum256(body)
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	if err = c.signer.SignHTTP(ctx, credentials, request, hex.EncodeToString(hash[:]),
		"execute-api", c.region, now); err != nil {
		return fmt.Errorf("sign control request: %w", err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("call control API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnauthorized ||
		response.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("control API returned %d", response.StatusCode)
	}
	if target == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err = decoder.Decode(target); err != nil {
		return fmt.Errorf("decode control API response: %w", err)
	}
	return nil
}
