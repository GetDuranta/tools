package devenvgateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GetDuranta/tools/internal/devaccess"
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

type ALBOIDCVerifier struct {
	SignerARN       string
	ClientID        string
	OwnerNamespace  string
	TrustEmailClaim bool
	KeyBaseURL      string
	HTTPClient      *http.Client
	Now             func() time.Time
	CacheTTL        time.Duration

	mu   sync.Mutex
	keys map[string]cachedPublicKey
}

type cachedPublicKey struct {
	key       *ecdsa.PublicKey
	expiresAt time.Time
}

type albJWTHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Signer    string `json:"signer"`
	Client    string `json:"client"`
	ExpiresAt int64  `json:"exp"`
}

func NewALBOIDCVerifier(region, signerARN, clientID, ownerNamespace string) *ALBOIDCVerifier {
	return &ALBOIDCVerifier{
		SignerARN: signerARN, ClientID: clientID, OwnerNamespace: ownerNamespace,
		KeyBaseURL: "https://public-keys.auth.elb." + region + ".amazonaws.com",
		HTTPClient: &http.Client{Timeout: 5 * time.Second}, Now: time.Now, CacheTTL: time.Hour,
		keys: make(map[string]cachedPublicKey),
	}
}

func (v *ALBOIDCVerifier) Verify(ctx context.Context, token string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, ErrNotFound
	}

	headerBytes, err := decodeJWTPart(parts[0])
	if err != nil {
		return Identity{}, ErrNotFound
	}
	var header albJWTHeader
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "ES256" ||
		header.Signer != v.SignerARN || !keyIDPattern.MatchString(header.KeyID) {
		return Identity{}, ErrNotFound
	}
	if v.ClientID != "" && header.Client != v.ClientID {
		return Identity{}, ErrNotFound
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if header.ExpiresAt <= now.Unix() {
		return Identity{}, ErrNotFound
	}

	key, err := v.publicKey(ctx, header.KeyID, now)
	if err != nil {
		return Identity{}, fmt.Errorf("load ALB signing key: %w", err)
	}
	signature, err := decodeJWTPart(parts[2])
	if err != nil || !verifyES256(key, []byte(parts[0]+"."+parts[1]), signature) {
		return Identity{}, ErrNotFound
	}

	claimsBytes, err := decodeJWTPart(parts[1])
	if err != nil {
		return Identity{}, ErrNotFound
	}
	var claims map[string]any
	if json.Unmarshal(claimsBytes, &claims) != nil {
		return Identity{}, ErrNotFound
	}
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return Identity{}, ErrNotFound
	}
	email, _ := claims["email"].(string)
	if !v.TrustEmailClaim && !claimTrue(claims["email_verified"]) {
		return Identity{}, ErrNotFound
	}
	email, err = devaccess.NormalizeEmail(email)
	if err != nil {
		return Identity{}, ErrNotFound
	}
	ownerID, err := devaccess.CanonicalOwnerID(v.OwnerNamespace, email)
	if err != nil {
		return Identity{}, ErrNotFound
	}
	principals := []string{"user:" + subject, ownerID}
	for _, name := range []string{"cognito:groups", "groups"} {
		for _, group := range claimStrings(claims[name]) {
			principals = append(principals, "group:"+group)
		}
	}
	slices.Sort(principals)
	principals = slices.Compact(principals)
	return Identity{Subject: subject, Email: email, OwnerID: ownerID, Principals: principals}, nil
}

func (v *ALBOIDCVerifier) publicKey(ctx context.Context, keyID string, now time.Time) (*ecdsa.PublicKey, error) {
	v.mu.Lock()
	entry, ok := v.keys[keyID]
	v.mu.Unlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.key, nil
	}

	base, err := url.Parse(strings.TrimRight(v.KeyBaseURL, "/") + "/")
	if err != nil {
		return nil, err
	}
	keyURL, err := base.Parse(url.PathEscape(keyID))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, keyURL.String(), nil)
	if err != nil {
		return nil, err
	}
	client := v.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key endpoint returned %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<10))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("invalid public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve.Params().Name != "P-256" {
		return nil, errors.New("ALB key is not ECDSA P-256")
	}
	ttl := v.CacheTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	v.mu.Lock()
	if v.keys == nil {
		v.keys = make(map[string]cachedPublicKey)
	}
	v.keys[keyID] = cachedPublicKey{key: key, expiresAt: now.Add(ttl)}
	v.mu.Unlock()
	return key, nil
}

func decodeJWTPart(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

func verifyES256(key *ecdsa.PublicKey, message, signature []byte) bool {
	hash := sha256.Sum256(message)
	if len(signature) == 64 {
		r := new(big.Int).SetBytes(signature[:32])
		s := new(big.Int).SetBytes(signature[32:])
		return ecdsa.Verify(key, hash[:], r, s)
	}
	return ecdsa.VerifyASN1(key, hash[:], signature)
}

func claimStrings(value any) []string {
	switch value := value.(type) {
	case string:
		if value == "" {
			return nil
		}
		return strings.Fields(value)
	case []any:
		values := make([]string, 0, len(value))
		for _, item := range value {
			if item, ok := item.(string); ok && item != "" {
				values = append(values, item)
			}
		}
		return values
	default:
		return nil
	}
}

func claimTrue(value any) bool {
	switch value := value.(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}
