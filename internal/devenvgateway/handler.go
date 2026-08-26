package devenvgateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/GetDuranta/tools/internal/devaccess"
)

const (
	defaultSessionCookie = "__Host-duranta-preview"
	bootstrapPath        = "/__auth/bootstrap"
	exchangePath         = "/__auth/exchange"
	openPath             = "/__auth/open"
)

var previewLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type HandlerConfig struct {
	Store          Store
	Verifier       IdentityVerifier
	ControlPlane   ControlPlane
	ControlHost    string
	PreviewSuffix  string
	SessionCookie  string
	SessionTTL     time.Duration
	CodeTTL        time.Duration
	ExternalScheme string
	Now            func() time.Time
	Random         io.Reader
	Logger         *slog.Logger
	Transport      http.RoundTripper
	Upstreams      UpstreamPolicy
}

type Gateway struct {
	store          Store
	verifier       IdentityVerifier
	controlPlane   ControlPlane
	controlHost    string
	previewSuffix  string
	sessionCookie  string
	sessionTTL     time.Duration
	codeTTL        time.Duration
	externalScheme string
	now            func() time.Time
	random         io.Reader
	logger         *slog.Logger
	transport      http.RoundTripper
	transportPool  *workspaceTransportPool
	upstreams      UpstreamPolicy
	issuer         devaccess.BootstrapIssuer
	activityMu     sync.Mutex
	activityLast   map[string]time.Time
}

func NewHandler(config HandlerConfig) (*Gateway, error) {
	controlHost, err := normalizeHost(config.ControlHost)
	if err != nil || controlHost == "" {
		return nil, errors.New("invalid control host")
	}
	previewSuffix := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(config.PreviewSuffix)), ".")
	if previewSuffix == "" || strings.Contains(previewSuffix, ":") {
		return nil, errors.New("invalid preview suffix")
	}
	if config.Store == nil || config.Verifier == nil {
		return nil, errors.New("store and identity verifier are required")
	}
	if config.SessionCookie == "" {
		config.SessionCookie = defaultSessionCookie
	}
	if !strings.HasPrefix(config.SessionCookie, "__Host-") {
		return nil, errors.New("session cookie must use the __Host- prefix")
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = 8 * time.Hour
	}
	if config.CodeTTL <= 0 || config.CodeTTL > time.Minute {
		config.CodeTTL = time.Minute
	}
	if config.ExternalScheme == "" {
		config.ExternalScheme = "https"
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if len(config.Upstreams.Prefixes) == 0 || len(config.Upstreams.Ports) == 0 {
		config.Upstreams = DefaultUpstreamPolicy()
	}
	return &Gateway{
		store: config.Store, verifier: config.Verifier, controlPlane: config.ControlPlane,
		controlHost: controlHost, previewSuffix: previewSuffix, sessionCookie: config.SessionCookie,
		sessionTTL: config.SessionTTL, codeTTL: config.CodeTTL, externalScheme: config.ExternalScheme,
		now: config.Now, random: config.Random, logger: config.Logger, transport: config.Transport,
		transportPool: newWorkspaceTransportPool(), upstreams: config.Upstreams,
		activityLast: make(map[string]time.Time),
		issuer: devaccess.BootstrapIssuer{
			Store: config.Store, Scheme: config.ExternalScheme, TTL: config.CodeTTL,
			Now: config.Now, Random: config.Random,
		},
	}, nil
}

func (g *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/health" || request.URL.Path == "/healthz" {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, "ok\n")
		return
	}

	host, err := normalizeHost(request.Host)
	if err != nil {
		writeNotFound(writer)
		return
	}
	if host == g.controlHost {
		g.serveControl(writer, request)
		return
	}
	if !g.validPreviewHost(host) {
		writeNotFound(writer)
		return
	}
	g.servePreview(writer, request, host)
}

func (g *Gateway) serveControl(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == openPath && request.Method == http.MethodGet {
		g.openPreview(writer, request)
		return
	}
	if g.controlPlane != nil && g.serveControlUI(writer, request) {
		return
	}
	writeNotFound(writer)
}

func (g *Gateway) openPreview(writer http.ResponseWriter, request *http.Request) {
	identity, err := g.controlIdentity(request)
	if err != nil {
		g.logFailure("control identity", err)
		writeNotFound(writer)
		return
	}
	host, err := normalizeHost(request.URL.Query().Get("host"))
	if err != nil || !g.validPreviewHost(host) {
		writeNotFound(writer)
		return
	}
	returnPath, err := cleanReturnPath(request.URL.Query().Get("return"))
	if err != nil {
		writeNotFound(writer)
		return
	}
	workspace, err := g.store.WorkspaceForPrincipals(request.Context(), host, identity.Principals)
	if err != nil {
		g.logFailure("authorize workspace", err)
		writeNotFound(writer)
		return
	}

	link, err := g.issuer.Issue(request.Context(), devaccess.IssueRequest{
		WorkspaceID: workspace.ID, Host: host, Subject: identity.OwnerID,
		Principals: identity.Principals, ACLVersion: workspace.ACLVersion, ReturnPath: returnPath,
	})
	if err != nil {
		g.logFailure("issue bootstrap", err)
		writeNotFound(writer)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Location", link.URL)
	writer.WriteHeader(http.StatusFound)
}

func (g *Gateway) servePreview(writer http.ResponseWriter, request *http.Request, host string) {
	switch {
	case request.URL.Path == bootstrapPath && request.Method == http.MethodGet:
		g.bootstrapPage(writer)
		return
	case request.URL.Path == exchangePath && request.Method == http.MethodPost:
		g.exchangeBootstrap(writer, request, host)
		return
	}

	cookie, err := request.Cookie(g.sessionCookie)
	if err != nil {
		if isTopNavigation(request) {
			g.redirectToControl(writer, request, host)
		} else {
			writeNotFound(writer)
		}
		return
	}
	token, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(token) != 32 {
		if isTopNavigation(request) {
			g.redirectToControl(writer, request, host)
		} else {
			writeNotFound(writer)
		}
		return
	}
	workspace, err := g.store.AuthorizedWorkspace(request.Context(), hashToken(token), host, g.now())
	if err != nil {
		g.logFailure("authorize session", err)
		if isTopNavigation(request) {
			g.redirectToControl(writer, request, host)
		} else {
			writeNotFound(writer)
		}
		return
	}
	if isWebSocket(request) && !validWebSocketOrigin(request.Header.Get("Origin"), g.externalScheme, host) {
		writeNotFound(writer)
		return
	}
	target, err := g.upstreams.Validate(workspace.Upstream)
	if err != nil {
		g.logFailure("validate upstream", err)
		writeNotFound(writer)
		return
	}
	transport := g.transport
	if transport == nil {
		transport, err = g.transportPool.For(target, host, workspace.TLSCertSHA256)
		if err != nil {
			g.logFailure("configure upstream transport", err)
			writeNotFound(writer)
			return
		}
	}
	g.recordPreviewActivity(workspace.ID)
	proxy := newWorkspaceProxy(target, host, g.sessionCookie, g.externalScheme, transport,
		func(response http.ResponseWriter, _ *http.Request, err error) {
			g.logFailure("proxy workspace", err)
			response.Header().Set("Cache-Control", "no-store")
			http.Error(response, "Bad gateway", http.StatusBadGateway)
		})
	proxy.ServeHTTP(writer, request)
}

func (g *Gateway) recordPreviewActivity(workspaceID string) {
	if g.controlPlane == nil || workspaceID == "" {
		return
	}
	now := g.now()
	g.activityMu.Lock()
	if last := g.activityLast[workspaceID]; !last.IsZero() && now.Sub(last) < 5*time.Minute {
		g.activityMu.Unlock()
		return
	}
	g.activityLast[workspaceID] = now
	g.activityMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := g.controlPlane.Activity(ctx, workspaceID); err != nil {
			g.logFailure("record preview activity", err)
		}
	}()
}

func (g *Gateway) bootstrapPage(writer http.ResponseWriter) {
	nonceBytes, err := randomBytes(g.random, 18)
	if err != nil {
		writeNotFound(writer)
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Frame-Options", "DENY")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; script-src 'nonce-"+nonce+"'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	_, _ = fmt.Fprintf(writer, `<!doctype html><meta charset="utf-8"><title>Opening preview</title><body>Opening preview…<script nonce="%s">(async()=>{const c=location.hash.slice(1);history.replaceState(null,"",location.pathname);if(!c)throw 0;const r=await fetch(%q,{method:"POST",credentials:"same-origin",headers:{"content-type":"text/plain"},body:c});if(!r.ok)throw 0;const v=await r.json();location.replace(v.path)})().catch(()=>{document.body.textContent="Not found"})</script>`,
		html.EscapeString(nonce), exchangePath)
}

func (g *Gateway) exchangeBootstrap(writer http.ResponseWriter, request *http.Request, host string) {
	mediaType := strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])
	if mediaType != "text/plain" || request.Header.Get("Origin") != g.externalScheme+"://"+host {
		writeNotFound(writer)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1024)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		writeNotFound(writer)
		return
	}
	rawCode, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil || len(rawCode) != 32 {
		writeNotFound(writer)
		return
	}
	rawSession, err := randomBytes(g.random, 32)
	if err != nil {
		writeNotFound(writer)
		return
	}
	expiresAt := g.now().Add(g.sessionTTL)
	grant, err := g.store.ConsumeBootstrap(request.Context(), hashToken(rawCode), host,
		Session{TokenHash: hashToken(rawSession), ExpiresAt: expiresAt}, g.now())
	if err != nil {
		g.logFailure("consume bootstrap", err)
		writeNotFound(writer)
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name: g.sessionCookie, Value: base64.RawURLEncoding.EncodeToString(rawSession),
		Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: expiresAt, MaxAge: int(g.sessionTTL.Seconds()),
	})
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(writer).Encode(map[string]string{"path": grant.ReturnPath})
}

func (g *Gateway) redirectToControl(writer http.ResponseWriter, request *http.Request, host string) {
	returnPath, err := cleanReturnPath(request.URL.RequestURI())
	if err != nil {
		writeNotFound(writer)
		return
	}
	target := url.URL{Scheme: g.externalScheme, Host: g.controlHost, Path: openPath}
	query := target.Query()
	query.Set("host", host)
	query.Set("return", returnPath)
	target.RawQuery = query.Encode()
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

func (g *Gateway) controlIdentity(request *http.Request) (Identity, error) {
	data := request.Header.Get("X-Amzn-Oidc-Data")
	if data == "" {
		return Identity{}, ErrNotFound
	}
	identity, err := g.verifier.Verify(request.Context(), data)
	if err != nil || identity.Subject == "" || identity.OwnerID == "" || identity.Email == "" ||
		len(identity.Principals) == 0 {
		return Identity{}, ErrNotFound
	}
	identity.OIDCData = data
	return identity, nil
}

func (g *Gateway) validPreviewHost(host string) bool {
	suffix := "." + g.previewSuffix
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := strings.TrimSuffix(host, suffix)
	return previewLabelPattern.MatchString(label)
}

func (g *Gateway) logFailure(operation string, err error) {
	if err != nil && !errors.Is(err, ErrNotFound) {
		g.logger.Error(operation, "error", err)
	}
}

func normalizeHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if strings.Contains(host, "/") || strings.HasSuffix(host, ".") {
		return "", errors.New("invalid host")
	}
	if parsed, port, err := net.SplitHostPort(host); err == nil {
		if port == "" {
			return "", errors.New("invalid host port")
		}
		host = parsed
	} else if strings.Contains(host, ":") {
		return "", errors.New("invalid host")
	}
	if host == "" {
		return "", errors.New("empty host")
	}
	return host, nil
}

func cleanReturnPath(raw string) (string, error) {
	if raw == "" {
		return "/a/", nil
	}
	if len(raw) > 4096 || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") ||
		strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("invalid return path")
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "", errors.New("invalid return path")
	}
	return parsed.RequestURI(), nil
}

func randomBytes(reader io.Reader, size int) ([]byte, error) {
	value := make([]byte, size)
	_, err := io.ReadFull(reader, value)
	return value, err
}

func hashToken(token []byte) string {
	return devaccess.TokenHash(token)
}

func validWebSocketOrigin(origin, scheme, host string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == scheme && parsed.Host == host && parsed.Path == "" &&
		parsed.RawQuery == "" && parsed.Fragment == ""
}

func isTopNavigation(request *http.Request) bool {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return false
	}
	if strings.EqualFold(request.Header.Get("Sec-Fetch-Dest"), "document") {
		return true
	}
	return strings.Contains(request.Header.Get("Accept"), "text/html")
}

func secureEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func writeNotFound(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(writer, "Not found\n")
}
