package devenv

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestLogtoPreviewAppsCreatesExactOriginSPA(t *testing.T) {
	requests := 0
	client := &LogtoPreviewApps{
		Endpoint: "https://logto.example.test", ManagementResource: "https://default.logto.app/api",
		ClientID: "client", ClientSecret: "secret",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			switch requests {
			case 1:
				if request.URL.Path != "/oidc/token" {
					t.Fatalf("unexpected token path: %s", request.URL.Path)
				}
				return testHTTPResponse(http.StatusOK, `{"access_token":"token","expires_in":3600}`), nil
			case 2:
				if request.URL.Path != "/api/applications" || request.URL.Query().Get("types") != "SPA" {
					t.Fatalf("unexpected list request: %s", request.URL.String())
				}
				return testHTTPResponse(http.StatusOK, `[]`), nil
			case 3:
				var payload map[string]any
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				metadata := payload["oidcClientMetadata"].(map[string]any)
				redirect := metadata["redirectUris"].([]any)[0]
				logout := metadata["postLogoutRedirectUris"].([]any)[0]
				if payload["type"] != "SPA" || redirect != "https://alice-task-a1b2.example.test/a/callback" ||
					logout != "https://alice-task-a1b2.example.test/a/signin" {
					t.Fatalf("unexpected application payload: %#v", payload)
				}
				return testHTTPResponse(http.StatusOK, `{"id":"app-1","name":"ddev-env-1"}`), nil
			default:
				t.Fatalf("unexpected request %d", requests)
				return nil, nil
			}
		})},
	}
	id, err := client.Ensure(context.Background(), Environment{
		ID: "env-1", Host: "alice-task-a1b2.example.test",
	})
	if err != nil || id != "app-1" {
		t.Fatalf("Ensure() = %q, %v", id, err)
	}
}

func TestLogtoPreviewAppsDeleteIsIdempotent(t *testing.T) {
	client := &LogtoPreviewApps{
		Endpoint: "https://logto.example.test", ManagementResource: "https://default.logto.app/api",
		ClientID: "client", ClientSecret: "secret",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/oidc/token" {
				return testHTTPResponse(http.StatusOK, `{"access_token":"token","expires_in":3600}`), nil
			}
			return testHTTPResponse(http.StatusNotFound, `{"message":"missing"}`), nil
		})},
	}
	if err := client.Delete(context.Background(), "app-1"); err != nil {
		t.Fatal(err)
	}
}

func TestLogtoPreviewAppsRecreatesDeletedApplication(t *testing.T) {
	requests := 0
	client := &LogtoPreviewApps{
		Endpoint: "https://logto.example.test", ManagementResource: "https://default.logto.app/api",
		ClientID: "client", ClientSecret: "secret",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			switch requests {
			case 1:
				return testHTTPResponse(http.StatusOK, `{"access_token":"token","expires_in":3600}`), nil
			case 2:
				return testHTTPResponse(http.StatusNotFound, `{"message":"missing"}`), nil
			case 3:
				return testHTTPResponse(http.StatusOK, `[]`), nil
			case 4:
				return testHTTPResponse(http.StatusOK, `{"id":"app-2","name":"ddev-env-1"}`), nil
			default:
				t.Fatalf("unexpected request %d", requests)
				return nil, nil
			}
		})},
	}
	id, err := client.Ensure(context.Background(), Environment{
		ID: "env-1", Host: "alice-task-a1b2.example.test", LogtoAppID: "app-deleted",
	})
	if err != nil || id != "app-2" {
		t.Fatalf("Ensure() = %q, %v", id, err)
	}
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
	}
}
