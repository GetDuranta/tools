package devenvgateway

import (
	"net/netip"
	"testing"
)

func TestUpstreamPolicyRejectsSSRFTargets(t *testing.T) {
	policy := UpstreamPolicy{
		Prefixes: []netip.Prefix{netip.MustParsePrefix("10.24.16.0/20")},
		Ports:    []string{"8443"},
	}
	for _, target := range []string{
		"http://metadata.google.internal:8443", "http://169.254.169.254:8443",
		"http://10.24.16.10:8080", "http://user@10.24.16.10:8443",
		"http://10.24.16.10:8443/path", "file:///etc/passwd",
	} {
		if _, err := policy.Validate(target); err == nil {
			t.Fatalf("accepted unsafe target %q", target)
		}
	}
	if _, err := policy.Validate("https://10.24.16.10:8443"); err != nil {
		t.Fatalf("rejected workspace target: %v", err)
	}
}
