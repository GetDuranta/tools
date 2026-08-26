package devenvgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeControlPlane struct {
	mu           sync.Mutex
	environments []EnvironmentSummary
	checkpoints  []CheckpointSummary
	actions      []string
}

func (c *fakeControlPlane) List(context.Context, Identity) ([]EnvironmentSummary, error) {
	return c.environments, nil
}

func (c *fakeControlPlane) ListCheckpoints(context.Context, Identity) ([]CheckpointSummary, error) {
	return c.checkpoints, nil
}

func (c *fakeControlPlane) record(action, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, action+":"+id)
	return nil
}

func (c *fakeControlPlane) Start(_ context.Context, _ Identity, id, _ string) error {
	return c.record("start", id)
}

func (c *fakeControlPlane) Extend(_ context.Context, _ Identity, id string, duration time.Duration, _ string) error {
	return c.record("extend="+duration.String(), id)
}

func (c *fakeControlPlane) Stop(_ context.Context, _ Identity, id, _ string) error {
	return c.record("stop", id)
}

func (c *fakeControlPlane) Archive(_ context.Context, _ Identity, id, _ string) error {
	return c.record("archive", id)
}

func (c *fakeControlPlane) Delete(_ context.Context, _ Identity, id, _ string) error {
	return c.record("delete", id)
}

func (c *fakeControlPlane) DeleteCheckpoint(_ context.Context, _ Identity, id, _ string) error {
	return c.record("delete-checkpoint", id)
}

func (c *fakeControlPlane) Activity(_ context.Context, id string) error {
	return c.record("activity", id)
}

func TestControlUIListsOwnedEnvironmentsAndRunsCSRFProtectedActions(t *testing.T) {
	store := newMemoryGatewayStore(gatewayWorkspace("http://10.0.1.2:8080"))
	control := &fakeControlPlane{environments: []EnvironmentSummary{
		{ID: "env-1", Name: "running-env", Host: "feature.preview.test", State: "RUNNING", OwnerEmail: "user@example.com"},
		{ID: "env-2", Name: "stopped-env", Host: "stopped.preview.test", State: "STOPPED", OwnerEmail: "user@example.com"},
	}, checkpoints: []CheckpointSummary{{ID: "checkpoint-1", Name: "alice-feature-20260827", State: "AVAILABLE"}}}
	gateway, err := NewHandler(HandlerConfig{
		Store: store, Verifier: fixedIdentityVerifier{identity: gatewayIdentity()}, ControlPlane: control,
		ControlHost: "control.preview.test", PreviewSuffix: "preview.test", ExternalScheme: "https",
	})
	if err != nil {
		t.Fatal(err)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "https://control.preview.test/", nil)
	listRequest.Host = "control.preview.test"
	listRequest.Header.Set("X-Amzn-Oidc-Data", "signed")
	listResponse := httptest.NewRecorder()
	gateway.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "running-env") ||
		!strings.Contains(listResponse.Body.String(), "/env/env-1/stop") ||
		!strings.Contains(listResponse.Body.String(), "/env/env-2/start") ||
		!strings.Contains(listResponse.Body.String(), "/checkpoint/checkpoint-1/confirm") ||
		!strings.Contains(listResponse.Body.String(), "alice-feature-20260827") ||
		strings.Contains(listResponse.Body.String(), "stopped.preview.test&amp;return") {
		t.Fatalf("unexpected list: status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var csrf *http.Cookie
	for _, cookie := range listResponse.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrf = cookie
		}
	}
	if csrf == nil || csrf.Domain != "" || csrf.Path != "/" || !csrf.Secure || !csrf.HttpOnly ||
		csrf.SameSite != http.SameSiteStrictMode {
		t.Fatalf("bad CSRF cookie: %#v", csrf)
	}

	form := url.Values{"csrf": {csrf.Value}, "hours": {"4"}, "idempotency": {"ui_test_action_123"}}
	actionRequest := httptest.NewRequest(http.MethodPost, "https://control.preview.test/env/env-1/extend",
		strings.NewReader(form.Encode()))
	actionRequest.Host = "control.preview.test"
	actionRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	actionRequest.Header.Set("Origin", "https://control.preview.test")
	actionRequest.Header.Set("X-Amzn-Oidc-Data", "signed")
	actionRequest.AddCookie(csrf)
	actionResponse := httptest.NewRecorder()
	gateway.ServeHTTP(actionResponse, actionRequest)
	if actionResponse.Code != http.StatusSeeOther || actionResponse.Header().Get("Location") != "/" ||
		len(control.actions) != 1 || control.actions[0] != "extend=4h0m0s:env-1" {
		t.Fatalf("action failed: status=%d location=%q actions=%v", actionResponse.Code,
			actionResponse.Header().Get("Location"), control.actions)
	}
}

func TestControlUIDestructiveActionsRequireConfirmationAndValidCSRF(t *testing.T) {
	store := newMemoryGatewayStore(gatewayWorkspace("http://10.0.1.2:8080"))
	control := &fakeControlPlane{environments: []EnvironmentSummary{{
		ID: "env-1", Name: "feature", Host: "feature.preview.test", State: "RUNNING",
	}}}
	gateway, err := NewHandler(HandlerConfig{
		Store: store, Verifier: fixedIdentityVerifier{identity: gatewayIdentity()}, ControlPlane: control,
		ControlHost: "control.preview.test", PreviewSuffix: "preview.test", ExternalScheme: "https",
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmRequest := httptest.NewRequest(http.MethodGet,
		"https://control.preview.test/env/env-1/confirm?action=delete", nil)
	confirmRequest.Host = "control.preview.test"
	confirmRequest.Header.Set("X-Amzn-Oidc-Data", "signed")
	confirmResponse := httptest.NewRecorder()
	gateway.ServeHTTP(confirmResponse, confirmRequest)
	if confirmResponse.Code != http.StatusOK || !strings.Contains(confirmResponse.Body.String(), "Confirm delete") ||
		len(control.actions) != 0 {
		t.Fatalf("unexpected confirmation: %d %s", confirmResponse.Code, confirmResponse.Body.String())
	}

	badRequest := httptest.NewRequest(http.MethodPost, "https://control.preview.test/env/env-1/delete",
		strings.NewReader("csrf=wrong"))
	badRequest.Host = "control.preview.test"
	badRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badRequest.Header.Set("Origin", "https://attacker.test")
	badRequest.Header.Set("X-Amzn-Oidc-Data", "signed")
	badResponse := httptest.NewRecorder()
	gateway.ServeHTTP(badResponse, badRequest)
	if badResponse.Code != http.StatusNotFound || len(control.actions) != 0 || badResponse.Body.String() != "Not found\n" {
		t.Fatalf("unsafe action was accepted: %d %v", badResponse.Code, control.actions)
	}
}

func TestControlUICheckpointDeletionRequiresConfirmation(t *testing.T) {
	store := newMemoryGatewayStore(gatewayWorkspace("http://10.0.1.2:8080"))
	control := &fakeControlPlane{checkpoints: []CheckpointSummary{{
		ID: "checkpoint-1", Name: "alice-feature", State: "AVAILABLE",
	}}}
	gateway, err := NewHandler(HandlerConfig{
		Store: store, Verifier: fixedIdentityVerifier{identity: gatewayIdentity()}, ControlPlane: control,
		ControlHost: "control.preview.test", PreviewSuffix: "preview.test", ExternalScheme: "https",
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmRequest := httptest.NewRequest(http.MethodGet,
		"https://control.preview.test/checkpoint/checkpoint-1/confirm", nil)
	confirmRequest.Host = "control.preview.test"
	confirmRequest.Header.Set("X-Amzn-Oidc-Data", "signed")
	confirmResponse := httptest.NewRecorder()
	gateway.ServeHTTP(confirmResponse, confirmRequest)
	if confirmResponse.Code != http.StatusOK ||
		!strings.Contains(confirmResponse.Body.String(), "Confirm checkpoint deletion") {
		t.Fatalf("unexpected confirmation: %d %s", confirmResponse.Code, confirmResponse.Body.String())
	}
	var csrf *http.Cookie
	for _, cookie := range confirmResponse.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrf = cookie
		}
	}
	match := regexp.MustCompile(`name="idempotency" value="([^"]+)"`).FindStringSubmatch(confirmResponse.Body.String())
	if csrf == nil || len(match) != 2 {
		t.Fatalf("confirmation is missing action tokens: cookie=%#v body=%s", csrf, confirmResponse.Body.String())
	}
	form := url.Values{"csrf": {csrf.Value}, "idempotency": {match[1]}}
	deleteRequest := httptest.NewRequest(http.MethodPost,
		"https://control.preview.test/checkpoint/checkpoint-1/delete", strings.NewReader(form.Encode()))
	deleteRequest.Host = "control.preview.test"
	deleteRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	deleteRequest.Header.Set("Origin", "https://control.preview.test")
	deleteRequest.Header.Set("X-Amzn-Oidc-Data", "signed")
	deleteRequest.AddCookie(csrf)
	deleteResponse := httptest.NewRecorder()
	gateway.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusSeeOther || len(control.actions) != 1 ||
		control.actions[0] != "delete-checkpoint:checkpoint-1" {
		t.Fatalf("checkpoint delete failed: status=%d actions=%v", deleteResponse.Code, control.actions)
	}
}

func TestPreviewActivityIsNonBlockingAndThrottled(t *testing.T) {
	control := &fakeControlPlane{}
	store := newMemoryGatewayStore(gatewayWorkspace("http://10.0.1.2:8080"))
	gateway, err := NewHandler(HandlerConfig{
		Store: store, Verifier: fixedIdentityVerifier{identity: gatewayIdentity()}, ControlPlane: control,
		ControlHost: "control.preview.test", PreviewSuffix: "preview.test",
		Now: func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway.recordPreviewActivity("env-1")
	gateway.recordPreviewActivity("env-1")
	deadline := time.Now().Add(time.Second)
	for {
		control.mu.Lock()
		actions := append([]string(nil), control.actions...)
		control.mu.Unlock()
		if len(actions) > 0 {
			if len(actions) != 1 || actions[0] != "activity:env-1" {
				t.Fatalf("unexpected activities: %v", actions)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("activity was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
}
