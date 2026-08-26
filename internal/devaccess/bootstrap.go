package devaccess

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

var ErrConflict = errors.New("conflict")

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type BootstrapGrant struct {
	CodeHash    string
	WorkspaceID string
	Host        string
	Subject     string
	Principals  []string
	ACLVersion  int64
	ReturnPath  string
	ExpiresAt   time.Time
}

type GrantWriter interface {
	PutBootstrap(context.Context, BootstrapGrant) error
}

type IssueRequest struct {
	WorkspaceID string
	Host        string
	Subject     string
	Principals  []string
	ACLVersion  int64
	ReturnPath  string
}

type BootstrapLink struct {
	URL       string
	ExpiresAt time.Time
}

type BootstrapIssuer struct {
	Store  GrantWriter
	Scheme string
	TTL    time.Duration
	Now    func() time.Time
	Random io.Reader
}

func (i BootstrapIssuer) Issue(ctx context.Context, request IssueRequest) (BootstrapLink, error) {
	if i.Store == nil || request.WorkspaceID == "" || request.Host == "" || request.Subject == "" ||
		len(request.Principals) == 0 || request.ACLVersion <= 0 {
		return BootstrapLink{}, errors.New("invalid bootstrap request")
	}
	principals := make([]string, 0, len(request.Principals))
	for _, principal := range request.Principals {
		if principal = strings.TrimSpace(principal); principal == "" {
			return BootstrapLink{}, errors.New("invalid bootstrap principal")
		}
		principals = append(principals, principal)
	}
	slices.Sort(principals)
	principals = slices.Compact(principals)
	returnPath, err := validateReturnPath(request.ReturnPath)
	if err != nil {
		return BootstrapLink{}, err
	}
	scheme := i.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if scheme != "https" && scheme != "http" {
		return BootstrapLink{}, errors.New("invalid bootstrap scheme")
	}
	host, err := normalizeDNSHost(request.Host)
	if err != nil {
		return BootstrapLink{}, errors.New("invalid bootstrap host")
	}
	ttl := i.TTL
	if ttl <= 0 || ttl > time.Minute {
		ttl = time.Minute
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	random := i.Random
	if random == nil {
		random = rand.Reader
	}

	for attempt := 0; attempt < 3; attempt++ {
		raw := make([]byte, 32)
		if _, err := io.ReadFull(random, raw); err != nil {
			return BootstrapLink{}, err
		}
		expiresAt := now().Add(ttl)
		grant := BootstrapGrant{
			CodeHash: TokenHash(raw), WorkspaceID: request.WorkspaceID, Host: host,
			Subject: request.Subject, Principals: principals, ACLVersion: request.ACLVersion,
			ReturnPath: returnPath, ExpiresAt: expiresAt,
		}
		if err := i.Store.PutBootstrap(ctx, grant); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return BootstrapLink{}, err
		}
		location := url.URL{Scheme: scheme, Host: host, Path: "/__auth/bootstrap",
			Fragment: base64.RawURLEncoding.EncodeToString(raw)}
		return BootstrapLink{URL: location.String(), ExpiresAt: expiresAt}, nil
	}
	return BootstrapLink{}, ErrConflict
}

func normalizeDNSHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" || len(host) > 253 || strings.HasSuffix(host, ".") || strings.Contains(host, ":") {
		return "", errors.New("invalid host")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("invalid host")
	}
	for _, label := range labels {
		if !dnsLabelPattern.MatchString(label) {
			return "", errors.New("invalid host")
		}
	}
	return host, nil
}

func TokenHash(token []byte) string {
	hash := sha256.Sum256(token)
	return hex.EncodeToString(hash[:])
}

func validateReturnPath(raw string) (string, error) {
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
