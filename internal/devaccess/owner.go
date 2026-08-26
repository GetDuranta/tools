package devaccess

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

var emailLocalPattern = regexp.MustCompile("^[a-z0-9.!#$%&'*+/=?^_`{|}~-]+$")

func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > 254 || strings.ContainsAny(email, "\r\n\t ") {
		return "", errors.New("invalid email")
	}
	separator := strings.LastIndexByte(email, '@')
	if separator <= 0 || separator != strings.IndexByte(email, '@') || separator == len(email)-1 {
		return "", errors.New("invalid email")
	}
	local, domain := email[:separator], email[separator+1:]
	if len(local) > 64 || !emailLocalPattern.MatchString(local) || strings.HasPrefix(local, ".") ||
		strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", errors.New("invalid email")
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", errors.New("invalid email")
	}
	for _, label := range labels {
		if !dnsLabelPattern.MatchString(label) {
			return "", errors.New("invalid email")
		}
	}
	return email, nil
}

func CanonicalOwnerID(namespace, trustedEmail string) (string, error) {
	if namespace = strings.TrimSpace(namespace); namespace == "" || len(namespace) > 256 {
		return "", errors.New("invalid owner namespace")
	}
	email, err := NormalizeEmail(trustedEmail)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte("v1\x00" + namespace + "\x00" + email))
	return "owner:v1:" + hex.EncodeToString(digest[:]), nil
}
