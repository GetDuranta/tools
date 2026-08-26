package devenvgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

type UpstreamPolicy struct {
	Prefixes []netip.Prefix
	Ports    []string
}

type workspaceTransportPool struct {
	http *http.Transport
	mu   sync.Mutex
	tls  map[string]*http.Transport
}

func newWorkspaceTransportPool() *workspaceTransportPool {
	return &workspaceTransportPool{http: newWorkspaceTransport(nil), tls: make(map[string]*http.Transport)}
}

func (p *workspaceTransportPool) For(target *url.URL, host, fingerprint string) (http.RoundTripper, error) {
	if target.Scheme == "http" {
		return p.http, nil
	}
	pinned, err := hex.DecodeString(fingerprint)
	if err != nil || len(pinned) != sha256.Size || fingerprint != strings.ToLower(fingerprint) {
		return nil, errors.New("invalid upstream TLS certificate pin")
	}
	key := target.String() + "\x00" + host + "\x00" + fingerprint
	p.mu.Lock()
	defer p.mu.Unlock()
	if transport := p.tls[key]; transport != nil {
		return transport, nil
	}
	transport := newWorkspaceTransport(&tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: host,
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("upstream did not present a certificate")
			}
			certificate := state.PeerCertificates[0]
			digest := sha256.Sum256(certificate.Raw)
			if subtle.ConstantTimeCompare(digest[:], pinned) != 1 {
				return errors.New("upstream TLS certificate pin mismatch")
			}
			if err := certificate.VerifyHostname(host); err != nil {
				return err
			}
			now := time.Now()
			if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
				return errors.New("upstream TLS certificate is not valid now")
			}
			return nil
		},
	})
	p.tls[key] = transport
	return transport, nil
}

func newWorkspaceTransport(tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2: true, MaxIdleConns: 256, MaxIdleConnsPerHost: 64,
		TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 0,
	}
}

func DefaultUpstreamPolicy() UpstreamPolicy {
	return UpstreamPolicy{
		Prefixes: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("172.16.0.0/12"),
			netip.MustParsePrefix("192.168.0.0/16"),
			netip.MustParsePrefix("fc00::/7"),
		},
		Ports: []string{"80", "443", "8080", "8443"},
	}
}

func (p UpstreamPolicy) Validate(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil ||
		target.Path != "" || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("invalid upstream URL")
	}
	address, err := netip.ParseAddr(target.Hostname())
	if err != nil {
		return nil, errors.New("upstream must use a literal IP address")
	}
	allowed := false
	for _, prefix := range p.Prefixes {
		if prefix.Contains(address) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errors.New("upstream is outside the workspace network")
	}
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if !slices.Contains(p.Ports, port) {
		return nil, errors.New("upstream port is not allowed")
	}
	return target, nil
}

func newWorkspaceProxy(target *url.URL, originalHost, sessionCookie, externalScheme string,
	transport http.RoundTripper, onError func(http.ResponseWriter, *http.Request, error)) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			clientIP := forwardedClientIP(request.In)
			stripGatewayHeaders(request.Out.Header)
			removeCookie(request.Out.Header, sessionCookie)
			request.SetURL(target)
			request.Out.Host = originalHost
			request.Out.Header.Set("X-Forwarded-Host", originalHost)
			request.Out.Header.Set("X-Forwarded-Proto", externalScheme)
			request.Out.Header.Set("X-Forwarded-Port", "443")
			if clientIP != "" {
				request.Out.Header.Set("X-Forwarded-For", clientIP)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			stripResponseCookie(response.Header, sessionCookie)
			return nil
		},
		Transport: transport, FlushInterval: -1, ErrorHandler: onError,
	}
	return proxy
}

func stripGatewayHeaders(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amzn-oidc-") || strings.HasPrefix(lower, "x-duranta-preview-") ||
			strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" || lower == "proxy-authorization" ||
			lower == "x-real-ip" {
			header.Del(name)
		}
	}
}

func removeCookie(header http.Header, name string) {
	request := &http.Request{Header: header}
	cookies := request.Cookies()
	header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != name {
			header.Add("Cookie", cookie.String())
		}
	}
}

func stripResponseCookie(header http.Header, name string) {
	values := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, value := range values {
		cookie, err := http.ParseSetCookie(value)
		if err != nil || cookie.Name != name {
			header.Add("Set-Cookie", value)
		}
	}
}

func forwardedClientIP(request *http.Request) string {
	values := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
	for i := len(values) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(values[i])
		if address, err := netip.ParseAddr(candidate); err == nil {
			return address.String()
		}
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		if address, parseErr := netip.ParseAddr(host); parseErr == nil {
			return address.String()
		}
	}
	return ""
}

func isWebSocket(request *http.Request) bool {
	return strings.EqualFold(request.Header.Get("Upgrade"), "websocket") &&
		headerContainsToken(request.Header.Get("Connection"), "upgrade")
}

func headerContainsToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
