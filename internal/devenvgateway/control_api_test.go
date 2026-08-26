package devenvgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type staticCredentials struct{}

func (staticCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "token"}, nil
}

func TestSigV4ControlPlaneSignsActorEnvelope(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	var envelopes []controlEnvelope
	var idempotencyKeys []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != gatewayControlPath ||
			!strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		var envelope controlEnvelope
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			http.Error(writer, "bad body", http.StatusBadRequest)
			return
		}
		envelopes = append(envelopes, envelope)
		idempotencyKeys = append(idempotencyKeys, request.Header.Get("Idempotency-Key"))
		writer.Header().Set("Content-Type", "application/json")
		if envelope.Action == "list" {
			_, _ = writer.Write([]byte(`{"environments":[{"id":"env-1","name":"feature","host":"feature.preview.test","state":"RUNNING","ownerEmail":"user@example.com","lease":{"idleExpiresAt":"2026-08-27T16:00:00Z","hardExpiresAt":"2026-08-28T12:00:00Z"}}]}`))
			return
		}
		if envelope.Action == "checkpoints" {
			_, _ = writer.Write([]byte(`{"checkpoints":[{"id":"checkpoint-1","name":"alice-feature","state":"AVAILABLE","pinned":true}]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"environment":{"id":"env-1"}}`))
	}))
	defer server.Close()
	client, err := NewSigV4ControlPlane(server.URL, staticCredentials{}, "us-west-2", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return now }
	identity := gatewayIdentity()

	environments, err := client.List(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(environments) != 1 || environments[0].ID != "env-1" ||
		!environments[0].ExpiresAt.Equal(time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected environments: %#v", environments)
	}
	if err = client.Extend(context.Background(), identity, "env-1", 4*time.Hour, "ui_action_12345"); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := client.ListCheckpoints(context.Background(), identity)
	if err != nil || len(checkpoints) != 1 || checkpoints[0].ID != "checkpoint-1" || !checkpoints[0].Pinned {
		t.Fatalf("unexpected checkpoints: %#v, %v", checkpoints, err)
	}
	if err = client.DeleteCheckpoint(context.Background(), identity, "checkpoint-1", "ui_checkpoint_12345"); err != nil {
		t.Fatal(err)
	}
	if err = client.Activity(context.Background(), "env-1"); err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != 5 || envelopes[0].ActorEmail != identity.Email ||
		envelopes[0].AuditSubject != identity.Subject || envelopes[0].Action != "list" ||
		envelopes[1].Action != "extend" || envelopes[1].EnvironmentID != "env-1" ||
		envelopes[1].Hours != 4 || envelopes[2].Action != "checkpoints" ||
		envelopes[3].Action != "delete-checkpoint" || envelopes[3].CheckpointID != "checkpoint-1" ||
		envelopes[4].Action != "activity" || envelopes[4].ActorEmail != "" ||
		envelopes[4].AuditSubject != "" || idempotencyKeys[0] != "" || idempotencyKeys[1] != "ui_action_12345" ||
		idempotencyKeys[3] != "ui_checkpoint_12345" {
		t.Fatalf("unexpected requests: %#v keys=%v", envelopes, idempotencyKeys)
	}
}

func TestSigV4ControlPlaneHidesAuthorizationFailures(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	client, err := NewSigV4ControlPlane(server.URL, staticCredentials{}, "us-west-2", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.List(context.Background(), gatewayIdentity())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}
