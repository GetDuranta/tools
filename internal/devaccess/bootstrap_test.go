package devaccess

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"testing"
	"time"
)

type grantWriter struct {
	grants []BootstrapGrant
	errors []error
}

func (w *grantWriter) PutBootstrap(_ context.Context, grant BootstrapGrant) error {
	w.grants = append(w.grants, grant)
	if len(w.errors) == 0 {
		return nil
	}
	err := w.errors[0]
	w.errors = w.errors[1:]
	return err
}

func TestBootstrapIssuerStoresOnlyHashAndUsesFragment(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	raw := bytes.Repeat([]byte{0x42}, 32)
	writer := &grantWriter{}
	issuer := BootstrapIssuer{
		Store: writer, TTL: 45 * time.Second, Now: func() time.Time { return now }, Random: bytes.NewReader(raw),
	}

	link, err := issuer.Issue(context.Background(), IssueRequest{
		WorkspaceID: "env-1", Host: "Preview-1.Example.test", Subject: "owner-1",
		Principals: []string{"owner-1"}, ACLVersion: 3, ReturnPath: "/a/list?tab=one",
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "preview-1.example.test" ||
		parsed.Path != "/__auth/bootstrap" || parsed.RawQuery != "" {
		t.Fatalf("unexpected link: %s", link.URL)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parsed.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatal("fragment does not contain the generated code")
	}
	if len(writer.grants) != 1 {
		t.Fatalf("got %d grants", len(writer.grants))
	}
	grant := writer.grants[0]
	if grant.CodeHash != TokenHash(raw) || grant.CodeHash == string(raw) || grant.Host != parsed.Host ||
		grant.WorkspaceID != "env-1" || grant.Subject != "owner-1" || grant.ACLVersion != 3 ||
		grant.ReturnPath != "/a/list?tab=one" || !grant.ExpiresAt.Equal(now.Add(45*time.Second)) {
		t.Fatalf("unexpected grant: %#v", grant)
	}
	if !link.ExpiresAt.Equal(grant.ExpiresAt) {
		t.Fatal("link and grant expiry differ")
	}
}

func TestBootstrapIssuerRetriesHashCollision(t *testing.T) {
	first := bytes.Repeat([]byte{1}, 32)
	second := bytes.Repeat([]byte{2}, 32)
	writer := &grantWriter{errors: []error{ErrConflict, nil}}
	issuer := BootstrapIssuer{Store: writer, Random: bytes.NewReader(append(first, second...))}

	link, err := issuer.Issue(context.Background(), IssueRequest{
		WorkspaceID: "env-1", Host: "one.example.test", Subject: "owner-1",
		Principals: []string{"owner-1"}, ACLVersion: 1, ReturnPath: "/a/",
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link.URL)
	decoded, _ := base64.RawURLEncoding.DecodeString(parsed.Fragment)
	if !bytes.Equal(decoded, second) || len(writer.grants) != 2 {
		t.Fatalf("collision was not retried: %x, %d grants", decoded, len(writer.grants))
	}
}

func TestBootstrapIssuerRejectsUnsafeInput(t *testing.T) {
	writer := &grantWriter{}
	issuer := BootstrapIssuer{Store: writer}
	base := IssueRequest{
		WorkspaceID: "env-1", Host: "one.example.test", Subject: "owner-1",
		Principals: []string{"owner-1"}, ACLVersion: 1, ReturnPath: "/a/",
	}
	for name, mutate := range map[string]func(*IssueRequest){
		"host":    func(request *IssueRequest) { request.Host = "one.example.test:443" },
		"path":    func(request *IssueRequest) { request.ReturnPath = "//attacker.test" },
		"actor":   func(request *IssueRequest) { request.Subject = "" },
		"version": func(request *IssueRequest) { request.ACLVersion = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			mutate(&request)
			if _, err := issuer.Issue(context.Background(), request); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	writer.errors = []error{errors.New("write failed")}
	if _, err := issuer.Issue(context.Background(), base); err == nil || err.Error() != "write failed" {
		t.Fatalf("unexpected store error: %v", err)
	}
}
