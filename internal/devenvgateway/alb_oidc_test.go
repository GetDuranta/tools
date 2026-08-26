package devenvgateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GetDuranta/tools/internal/devaccess"
)

func TestALBOIDCVerifierChecksSignatureAndVerifiedEmail(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	var keyRequests atomic.Int32
	keyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		keyRequests.Add(1)
		if request.URL.Path != "/key-1" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(publicPEM)
	}))
	defer keyServer.Close()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	verifier := NewALBOIDCVerifier("us-west-2", "arn:aws:elasticloadbalancing:us-west-2:123:loadbalancer/app/gateway/1", "client-1", "aws:123:be-dev")
	verifier.KeyBaseURL = keyServer.URL
	verifier.Now = func() time.Time { return now }
	token := signALBToken(t, privateKey, albJWTHeader{
		Algorithm: "ES256", KeyID: "key-1", Signer: verifier.SignerARN,
		Client: verifier.ClientID, ExpiresAt: now.Add(time.Minute).Unix(),
	}, map[string]any{
		"sub": "subject-1", "email": "Vitalii@Example.com", "email_verified": true,
		"groups": []string{"developers", "admins"},
	})

	identity, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	wantOwner, _ := devaccess.CanonicalOwnerID("aws:123:be-dev", "vitalii@example.com")
	if identity.Subject != "subject-1" || identity.Email != "vitalii@example.com" ||
		identity.OwnerID != wantOwner || !slices.Contains(identity.Principals, wantOwner) ||
		!slices.Contains(identity.Principals, "group:developers") {
		t.Fatalf("unexpected identity: %#v", identity)
	}
	if _, err := verifier.Verify(context.Background(), token); err != nil || keyRequests.Load() != 1 {
		t.Fatalf("key was not cached: err=%v requests=%d", err, keyRequests.Load())
	}
}

func TestALBOIDCVerifierRejectsUntrustedClaims(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, _ := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = pem.Encode(writer, &pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	}))
	defer server.Close()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	verifier := NewALBOIDCVerifier("us-west-2", "signer", "client", "namespace")
	verifier.KeyBaseURL = server.URL
	verifier.Now = func() time.Time { return now }
	baseHeader := albJWTHeader{
		Algorithm: "ES256", KeyID: "key-1", Signer: "signer", Client: "client",
		ExpiresAt: now.Add(time.Minute).Unix(),
	}
	for name, alter := range map[string]func(*albJWTHeader, map[string]any){
		"expired": func(header *albJWTHeader, _ map[string]any) { header.ExpiresAt = now.Unix() },
		"signer":  func(header *albJWTHeader, _ map[string]any) { header.Signer = "other" },
		"client":  func(header *albJWTHeader, _ map[string]any) { header.Client = "other" },
		"email":   func(_ *albJWTHeader, claims map[string]any) { claims["email"] = "attacker" },
		"verified": func(_ *albJWTHeader, claims map[string]any) {
			claims["email_verified"] = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			header := baseHeader
			claims := map[string]any{"sub": "subject", "email": "user@example.com", "email_verified": true}
			alter(&header, claims)
			token := signALBToken(t, privateKey, header, claims)
			if _, err := verifier.Verify(context.Background(), token); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func signALBToken(t *testing.T, key *ecdsa.PrivateKey, header albJWTHeader, claims map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256Bytes([]byte(encodedHeader + "." + encodedClaims))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest)
	if err != nil {
		t.Fatal(err)
	}
	signature := append(paddedInt(r), paddedInt(s)...)
	return encodedHeader + "." + encodedClaims + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func paddedInt(value *big.Int) []byte {
	result := make([]byte, 32)
	value.FillBytes(result)
	return result
}
