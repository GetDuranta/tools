package devaccess

import (
	"strings"
	"testing"
)

func TestCanonicalOwnerIDNormalizesVerifiedEmail(t *testing.T) {
	first, err := CanonicalOwnerID("aws:123456789012:be-dev", " Vitalii@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalOwnerID("aws:123456789012:be-dev", "vitalii@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "owner:v1:") || strings.Contains(first, "vitalii") {
		t.Fatalf("unexpected owner id %q", first)
	}
	other, _ := CanonicalOwnerID("aws:999999999999:be-dev", "vitalii@example.com")
	if other == first {
		t.Fatal("namespace must separate identities")
	}
}

func TestNormalizeEmailRejectsDisplayNamesAndWhitespace(t *testing.T) {
	for _, value := range []string{"", "Vitalii <v@example.com>", "v @example.com", "a@", "@example.com"} {
		if _, err := NormalizeEmail(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
