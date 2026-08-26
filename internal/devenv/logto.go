package devenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LogtoPreviewApps struct {
	Endpoint           string
	ManagementResource string
	ClientID           string
	ClientSecret       string
	HTTPClient         *http.Client

	mutex        sync.Mutex
	accessToken  string
	tokenExpires time.Time
}

type logtoApplication struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *LogtoPreviewApps) Ensure(ctx context.Context, environment Environment) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	name := "ddev-" + environment.ID
	applicationID := environment.LogtoAppID
	if applicationID == "" {
		var err error
		applicationID, err = c.findApplication(ctx, name)
		if err != nil {
			return "", err
		}
	}
	origin := "https://" + environment.Host
	payload := map[string]any{
		"name":        name,
		"description": "Managed Duranta development environment " + environment.ID,
		"oidcClientMetadata": map[string]any{
			"redirectUris":           []string{origin + "/a/callback"},
			"postLogoutRedirectUris": []string{origin + "/a/signin"},
		},
		"customClientMetadata": map[string]any{"corsAllowedOrigins": []string{origin}},
		"customData":           map[string]any{"durantaEnvironmentId": environment.ID},
	}
	if applicationID == "" {
		payload["type"] = "SPA"
		var created logtoApplication
		if err := c.call(ctx, http.MethodPost, "/api/applications", payload, &created); err != nil {
			return "", err
		}
		return created.ID, nil
	}
	if err := c.call(ctx, http.MethodPatch, "/api/applications/"+url.PathEscape(applicationID), payload, nil); err == nil {
		return applicationID, nil
	} else {
		var responseErr *logtoResponseError
		if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusNotFound {
			return "", err
		}
	}
	applicationID, err := c.findApplication(ctx, name)
	if err != nil {
		return "", err
	}
	if applicationID != "" {
		if err = c.call(ctx, http.MethodPatch, "/api/applications/"+url.PathEscape(applicationID), payload, nil); err != nil {
			return "", err
		}
		return applicationID, nil
	}
	payload["type"] = "SPA"
	var created logtoApplication
	if err = c.call(ctx, http.MethodPost, "/api/applications", payload, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *LogtoPreviewApps) findApplication(ctx context.Context, name string) (string, error) {
	applications, err := c.listApplications(ctx)
	if err != nil {
		return "", err
	}
	for _, application := range applications {
		if application.Name == name {
			return application.ID, nil
		}
	}
	return "", nil
}

func (c *LogtoPreviewApps) Delete(ctx context.Context, applicationID string) error {
	if applicationID == "" {
		return nil
	}
	err := c.call(ctx, http.MethodDelete, "/api/applications/"+url.PathEscape(applicationID), nil, nil)
	var responseErr *logtoResponseError
	if errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *LogtoPreviewApps) listApplications(ctx context.Context) ([]logtoApplication, error) {
	applications := []logtoApplication{}
	for page := 1; ; page++ {
		var batch []logtoApplication
		path := "/api/applications?types=SPA&page=" + strconv.Itoa(page) + "&page_size=100"
		if err := c.call(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		applications = append(applications, batch...)
		if len(batch) < 100 {
			return applications, nil
		}
	}
}

func (c *LogtoPreviewApps) call(ctx context.Context, method, path string, body, response any) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	var raw []byte
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Endpoint, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	result, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer result.Body.Close()
	if result.StatusCode < 200 || result.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(result.Body, 64<<10))
		return &logtoResponseError{StatusCode: result.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	if response == nil {
		return nil
	}
	return json.NewDecoder(result.Body).Decode(response)
}

func (c *LogtoPreviewApps) token(ctx context.Context) (string, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.accessToken != "" && time.Now().UTC().Before(c.tokenExpires.Add(-time.Minute)) {
		return c.accessToken, nil
	}
	values := url.Values{
		"grant_type": {"client_credentials"}, "resource": {c.ManagementResource}, "scope": {"all"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.Endpoint, "/")+"/oidc/token", strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(c.ClientID, c.ClientSecret)
	request.Header.Set("content-type", "application/x-www-form-urlencoded")
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	result, err := httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer result.Body.Close()
	if result.StatusCode < 200 || result.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(result.Body, 64<<10))
		return "", &logtoResponseError{StatusCode: result.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err = json.NewDecoder(result.Body).Decode(&token); err != nil {
		return "", err
	}
	if token.AccessToken == "" || token.ExpiresIn < 1 {
		return "", errors.New("Logto returned an invalid access token")
	}
	c.accessToken = token.AccessToken
	c.tokenExpires = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

func (c *LogtoPreviewApps) validate() error {
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		c.ManagementResource == "" || c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("incomplete Logto preview application configuration")
	}
	return nil
}

type logtoResponseError struct {
	StatusCode int
	Message    string
}

func (e *logtoResponseError) Error() string {
	return fmt.Sprintf("Logto returned HTTP %d: %s", e.StatusCode, e.Message)
}
