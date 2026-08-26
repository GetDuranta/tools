package devenvgateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryGatewayStore struct {
	mu        sync.Mutex
	workspace Workspace
	grants    map[string]BootstrapGrant
	sessions  map[string]Session
	putErr    error
}

func newMemoryGatewayStore(workspace Workspace) *memoryGatewayStore {
	return &memoryGatewayStore{
		workspace: workspace, grants: make(map[string]BootstrapGrant), sessions: make(map[string]Session),
	}
}

func (s *memoryGatewayStore) WorkspaceForPrincipals(_ context.Context, host string,
	principals []string) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if host != s.workspace.Host || !s.workspace.Running() || !s.workspace.Allows(principals) {
		return Workspace{}, ErrNotFound
	}
	return s.workspace, nil
}

func (s *memoryGatewayStore) PutBootstrap(_ context.Context, grant BootstrapGrant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return s.putErr
	}
	if _, exists := s.grants[grant.CodeHash]; exists {
		return ErrConflict
	}
	s.grants[grant.CodeHash] = grant
	return nil
}

func (s *memoryGatewayStore) ConsumeBootstrap(_ context.Context, codeHash, host string,
	session Session, now time.Time) (BootstrapGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, exists := s.grants[codeHash]
	if !exists || grant.Host != host || !now.Before(grant.ExpiresAt) || !s.workspace.Running() ||
		s.workspace.ID != grant.WorkspaceID || s.workspace.ACLVersion != grant.ACLVersion {
		return BootstrapGrant{}, ErrNotFound
	}
	delete(s.grants, codeHash)
	session.WorkspaceID = grant.WorkspaceID
	session.Host = grant.Host
	session.Subject = grant.Subject
	session.Principals = grant.Principals
	session.ACLVersion = grant.ACLVersion
	s.sessions[session.TokenHash] = session
	return grant, nil
}

func (s *memoryGatewayStore) AuthorizedWorkspace(_ context.Context, tokenHash, host string,
	now time.Time) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, exists := s.sessions[tokenHash]
	if !exists || session.Host != host || !now.Before(session.ExpiresAt) || !s.workspace.Running() ||
		s.workspace.ID != session.WorkspaceID || s.workspace.ACLVersion != session.ACLVersion {
		return Workspace{}, ErrNotFound
	}
	return s.workspace, nil
}

type fixedIdentityVerifier struct {
	identity Identity
	err      error
}

func (v fixedIdentityVerifier) Verify(context.Context, string) (Identity, error) {
	return v.identity, v.err
}

func gatewayIdentity() Identity {
	return Identity{
		Subject: "subject-1", Email: "user@example.com", OwnerID: "owner:v1:one",
		Principals: []string{"owner:v1:one", "group:developers"},
	}
}

func gatewayWorkspace(upstream string) Workspace {
	return Workspace{
		ID: "env-1", Name: "feature", Host: "feature.preview.test", Upstream: upstream,
		State: WorkspaceStateRunning, Visibility: WorkspaceVisibilityOrg, ACLVersion: 4,
	}
}

func newTestGateway(t *testing.T, store Store, random io.Reader, now time.Time,
	upstreams UpstreamPolicy) *Gateway {
	t.Helper()
	if random == nil {
		random = bytes.NewReader(bytes.Repeat([]byte{0x44}, 4096))
	}
	gateway, err := NewHandler(HandlerConfig{
		Store: store, Verifier: fixedIdentityVerifier{identity: gatewayIdentity()},
		ControlHost: "control.preview.test", PreviewSuffix: "preview.test", ExternalScheme: "http",
		SessionTTL: 2 * time.Hour, CodeTTL: time.Minute, Now: func() time.Time { return now },
		Random: random, Upstreams: upstreams,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func TestOpenIssuesBoundHashOnlyBootstrapLink(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	rawCode := bytes.Repeat([]byte{0x31}, 32)
	store := newMemoryGatewayStore(gatewayWorkspace("http://10.0.1.2:8080"))
	gateway := newTestGateway(t, store, bytes.NewReader(rawCode), now, DefaultUpstreamPolicy())
	request := httptest.NewRequest(http.MethodGet,
		"http://control.preview.test/__auth/open?host=feature.preview.test&return=%2Fa%2Fproperties%3Ftab%3Dai", nil)
	request.Host = "control.preview.test"
	request.Header.Set("X-Amzn-Oidc-Data", "signed-token")
	response := httptest.NewRecorder()

	gateway.ServeHTTP(response, request)

	if response.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != "feature.preview.test" || location.Path != bootstrapPath || location.RawQuery != "" {
		t.Fatalf("unexpected redirect: %s", location)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(location.Fragment)
	if err != nil || !bytes.Equal(decoded, rawCode) {
		t.Fatalf("bad fragment: %q, %v", location.Fragment, err)
	}
	grant := store.grants[hashToken(rawCode)]
	if grant.CodeHash != hashToken(rawCode) || grant.Subject != gatewayIdentity().OwnerID ||
		grant.WorkspaceID != "env-1" || grant.Host != "feature.preview.test" || grant.ACLVersion != 4 ||
		grant.ReturnPath != "/a/properties?tab=ai" || !grant.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("unexpected grant: %#v", grant)
	}
	if strings.Contains(response.Body.String(), location.Fragment) {
		t.Fatal("bootstrap code leaked into response body")
	}
}

func TestOpenUnknownAndUnauthorizedAreIndistinguishable(t *testing.T) {
	now := time.Now()
	workspace := gatewayWorkspace("http://10.0.1.2:8080")
	workspace.Visibility = WorkspaceVisibilityRestricted
	workspace.AllowedPrincipals = []string{"group:other"}
	store := newMemoryGatewayStore(workspace)
	unauthorized := newTestGateway(t, store, nil, now, DefaultUpstreamPolicy())
	unknown, err := NewHandler(HandlerConfig{
		Store: store, Verifier: fixedIdentityVerifier{err: errors.New("bad token")},
		ControlHost: "control.preview.test", PreviewSuffix: "preview.test", ExternalScheme: "http",
	})
	if err != nil {
		t.Fatal(err)
	}

	call := func(handler http.Handler) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet,
			"http://control.preview.test/__auth/open?host=feature.preview.test", nil)
		request.Host = "control.preview.test"
		request.Header.Set("X-Amzn-Oidc-Data", "token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	left, right := call(unauthorized), call(unknown)
	if left.Code != http.StatusNotFound || left.Code != right.Code || left.Body.String() != right.Body.String() ||
		left.Header().Get("Content-Type") != right.Header().Get("Content-Type") {
		t.Fatalf("responses differ: %#v / %#v", left.Result(), right.Result())
	}
}

func TestBootstrapExchangeIsHostBoundAndSingleUse(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	rawCode := bytes.Repeat([]byte{0x52}, 32)
	rawSession := bytes.Repeat([]byte{0x61}, 32)
	store := newMemoryGatewayStore(gatewayWorkspace("http://10.0.1.2:8080"))
	if err := store.PutBootstrap(context.Background(), BootstrapGrant{
		CodeHash: hashToken(rawCode), WorkspaceID: "env-1", Host: "feature.preview.test",
		Subject: "owner:v1:one", Principals: []string{"owner:v1:one"}, ACLVersion: 4,
		ReturnPath: "/a/properties", ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	gateway := newTestGateway(t, store, bytes.NewReader(bytes.Repeat(rawSession, 3)), now, DefaultUpstreamPolicy())
	encoded := base64.RawURLEncoding.EncodeToString(rawCode)

	wrongHost := httptest.NewRequest(http.MethodPost, "http://other.preview.test"+exchangePath, strings.NewReader(encoded))
	wrongHost.Host = "other.preview.test"
	wrongHost.Header.Set("Content-Type", "text/plain")
	wrongHost.Header.Set("Origin", "http://other.preview.test")
	wrongResponse := httptest.NewRecorder()
	gateway.ServeHTTP(wrongResponse, wrongHost)
	if wrongResponse.Code != http.StatusNotFound {
		t.Fatalf("wrong host status=%d", wrongResponse.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "http://feature.preview.test"+exchangePath, strings.NewReader(encoded))
	request.Host = "feature.preview.test"
	request.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	request.Header.Set("Origin", "http://feature.preview.test")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"path":"/a/properties"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != defaultSessionCookie || cookie.Domain != "" || cookie.Path != "/" ||
		!cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.MaxAge != 2*60*60 {
		t.Fatalf("unsafe cookie: %#v", cookie)
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(cookie.Value)
	if !bytes.Equal(decoded, rawSession) {
		t.Fatal("unexpected session token")
	}

	replay := httptest.NewRecorder()
	replayRequest := httptest.NewRequest(http.MethodPost, "http://feature.preview.test"+exchangePath, strings.NewReader(encoded))
	replayRequest.Host = "feature.preview.test"
	replayRequest.Header.Set("Content-Type", "text/plain")
	replayRequest.Header.Set("Origin", "http://feature.preview.test")
	gateway.ServeHTTP(replay, replayRequest)
	if replay.Code != http.StatusNotFound {
		t.Fatalf("replay status=%d", replay.Code)
	}
}

func TestBootstrapPageMovesFragmentIntoSameOriginBody(t *testing.T) {
	store := newMemoryGatewayStore(gatewayWorkspace("http://10.0.1.2:8080"))
	gateway := newTestGateway(t, store, nil, time.Now(), DefaultUpstreamPolicy())
	request := httptest.NewRequest(http.MethodGet, "http://feature.preview.test"+bootstrapPath, nil)
	request.Host = "feature.preview.test"
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "location.hash.slice(1)") ||
		!strings.Contains(body, `fetch("/__auth/exchange"`) ||
		!strings.Contains(body, `history.replaceState`) || response.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("unexpected bootstrap page: status=%d body=%s", response.Code, body)
	}
}

func TestProxyPreservesAppAuthAndStripsGatewayCredentials(t *testing.T) {
	var capturedHost string
	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		capturedHost = request.Host
		capturedHeaders = request.Header.Clone()
		http.SetCookie(writer, &http.Cookie{Name: defaultSessionCookie, Value: "overwrite", Path: "/", Secure: true})
		http.SetCookie(writer, &http.Cookie{Name: "app_response", Value: "keep", Path: "/"})
		_, _ = io.WriteString(writer, "proxied")
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	port := upstreamURL.Port()
	now := time.Now()
	workspace := gatewayWorkspace(upstream.URL)
	store := newMemoryGatewayStore(workspace)
	rawSession := bytes.Repeat([]byte{0x71}, 32)
	store.sessions[hashToken(rawSession)] = Session{
		TokenHash: hashToken(rawSession), WorkspaceID: workspace.ID, Host: workspace.Host,
		Subject: "owner", Principals: []string{"owner"}, ACLVersion: workspace.ACLVersion,
		ExpiresAt: now.Add(time.Hour),
	}
	policy := UpstreamPolicy{Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, Ports: []string{port}}
	gateway := newTestGateway(t, store, nil, now, policy)
	request := httptest.NewRequest(http.MethodGet, "http://feature.preview.test/a/data", nil)
	request.Host = workspace.Host
	request.RemoteAddr = "10.0.0.5:12345"
	request.Header.Set("Authorization", "Bearer app-token")
	request.Header.Set("Cookie", defaultSessionCookie+"="+base64.RawURLEncoding.EncodeToString(rawSession)+"; app_session=keep")
	request.Header.Set("X-Amzn-Oidc-Data", "spoof")
	request.Header.Set("X-Duranta-Preview-Principal", "spoof")
	request.Header.Set("Forwarded", "for=spoof")
	request.Header.Set("X-Forwarded-For", "192.0.2.1, 203.0.113.8")
	request.Header.Set("X-Real-IP", "192.0.2.2")
	response := httptest.NewRecorder()

	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "proxied" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if capturedHost != workspace.Host || capturedHeaders.Get("Authorization") != "Bearer app-token" ||
		capturedHeaders.Get("X-Amzn-Oidc-Data") != "" || capturedHeaders.Get("X-Duranta-Preview-Principal") != "" ||
		capturedHeaders.Get("Forwarded") != "" || capturedHeaders.Get("X-Real-IP") != "" ||
		capturedHeaders.Get("X-Forwarded-For") != "203.0.113.8" ||
		capturedHeaders.Get("X-Forwarded-Host") != workspace.Host ||
		capturedHeaders.Get("X-Forwarded-Proto") != "http" {
		t.Fatalf("unexpected proxy headers: host=%q headers=%v", capturedHost, capturedHeaders)
	}
	cookieHeader := capturedHeaders.Get("Cookie")
	if strings.Contains(cookieHeader, defaultSessionCookie) || !strings.Contains(cookieHeader, "app_session=keep") {
		t.Fatalf("unexpected upstream cookie: %q", cookieHeader)
	}
	setCookies := response.Header().Values("Set-Cookie")
	if len(setCookies) != 1 || !strings.HasPrefix(setCookies[0], "app_response=keep") {
		t.Fatalf("gateway cookie was not protected: %v", setCookies)
	}
}

func TestProxyCarriesWebSocketUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !isWebSocket(request) {
			http.Error(writer, "not an upgrade", http.StatusBadRequest)
			return
		}
		connection, buffer, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = buffer.Flush()
		payload := make([]byte, 4)
		if _, err = io.ReadFull(buffer, payload); err == nil {
			_, _ = connection.Write(payload)
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	now := time.Now()
	workspace := gatewayWorkspace(upstream.URL)
	store := newMemoryGatewayStore(workspace)
	rawSession := bytes.Repeat([]byte{0x72}, 32)
	store.sessions[hashToken(rawSession)] = Session{
		WorkspaceID: workspace.ID, Host: workspace.Host, ACLVersion: workspace.ACLVersion,
		ExpiresAt: now.Add(time.Hour),
	}
	policy := UpstreamPolicy{
		Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, Ports: []string{upstreamURL.Port()},
	}
	gatewayServer := httptest.NewServer(newTestGateway(t, store, nil, now, policy))
	defer gatewayServer.Close()
	gatewayURL, _ := url.Parse(gatewayServer.URL)
	connection, err := net.DialTimeout("tcp", gatewayURL.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(connection, "GET /@vite/client HTTP/1.1\r\nHost: "+workspace.Host+
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: http://"+workspace.Host+
		"\r\nCookie: "+defaultSessionCookie+"="+base64.RawURLEncoding.EncodeToString(rawSession)+"\r\n\r\n")
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if _, err = connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4)
	if _, err = io.ReadFull(reader, payload); err != nil || string(payload) != "ping" {
		t.Fatalf("websocket tunnel failed: %q %v", payload, err)
	}
}

func TestProxyPinsEphemeralWorkspaceCertificate(t *testing.T) {
	host := "feature.preview.test"
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "secure upstream")
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{certificateDER}, PrivateKey: privateKey,
	}}}
	upstream.StartTLS()
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	fingerprint := sha256.Sum256(certificateDER)
	workspace := gatewayWorkspace(upstream.URL)
	workspace.TLSCertSHA256 = hex.EncodeToString(fingerprint[:])
	store := newMemoryGatewayStore(workspace)
	rawSession := bytes.Repeat([]byte{0x73}, 32)
	store.sessions[hashToken(rawSession)] = Session{
		WorkspaceID: workspace.ID, Host: workspace.Host, ACLVersion: workspace.ACLVersion,
		ExpiresAt: now.Add(time.Hour),
	}
	policy := UpstreamPolicy{
		Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, Ports: []string{upstreamURL.Port()},
	}
	gateway := newTestGateway(t, store, nil, now, policy)
	request := httptest.NewRequest(http.MethodGet, "https://"+host+"/healthcheck", nil)
	request.Host = host
	request.AddCookie(&http.Cookie{
		Name: defaultSessionCookie, Value: base64.RawURLEncoding.EncodeToString(rawSession),
	})
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "secure upstream" {
		t.Fatalf("TLS proxy failed: status=%d body=%s", response.Code, response.Body.String())
	}
}
