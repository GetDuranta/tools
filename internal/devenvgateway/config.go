package devenvgateway

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

type RuntimeConfig struct {
	ListenAddress   string
	TableName       string
	Region          string
	ControlHost     string
	PreviewSuffix   string
	ALBSignerARN    string
	ALBClientID     string
	OwnerNamespace  string
	ControlAPIURL   string
	TrustEmailClaim bool
	SessionTTL      time.Duration
	Upstreams       UpstreamPolicy
}

func ParseRuntimeConfig(getenv func(string) string) (RuntimeConfig, error) {
	config := RuntimeConfig{
		ListenAddress:  strings.TrimSpace(getenv("DEVENV_GATEWAY_LISTEN_ADDR")),
		TableName:      strings.TrimSpace(getenv("DEVENV_GATEWAY_TABLE")),
		Region:         strings.TrimSpace(getenv("AWS_REGION")),
		ControlHost:    strings.TrimSpace(getenv("DEVENV_GATEWAY_CONTROL_HOST")),
		PreviewSuffix:  strings.TrimSpace(getenv("DEVENV_GATEWAY_PREVIEW_SUFFIX")),
		ALBSignerARN:   strings.TrimSpace(getenv("DEVENV_GATEWAY_ALB_ARN")),
		ALBClientID:    strings.TrimSpace(getenv("DEVENV_GATEWAY_ALB_CLIENT_ID")),
		OwnerNamespace: strings.TrimSpace(getenv("DEVENV_OWNER_NAMESPACE")),
		ControlAPIURL:  strings.TrimSpace(getenv("DEVENV_CONTROL_API_URL")),
	}
	if config.ListenAddress == "" {
		config.ListenAddress = ":8080"
	}
	if _, _, err := net.SplitHostPort(config.ListenAddress); err != nil {
		return RuntimeConfig{}, fmt.Errorf("DEVENV_GATEWAY_LISTEN_ADDR: %w", err)
	}
	required := map[string]string{
		"DEVENV_GATEWAY_TABLE": config.TableName, "AWS_REGION": config.Region,
		"DEVENV_GATEWAY_CONTROL_HOST":   config.ControlHost,
		"DEVENV_GATEWAY_PREVIEW_SUFFIX": config.PreviewSuffix,
		"DEVENV_GATEWAY_ALB_ARN":        config.ALBSignerARN,
		"DEVENV_GATEWAY_ALB_CLIENT_ID":  config.ALBClientID,
		"DEVENV_OWNER_NAMESPACE":        config.OwnerNamespace,
		"DEVENV_CONTROL_API_URL":        config.ControlAPIURL,
	}
	for name, value := range required {
		if value == "" {
			return RuntimeConfig{}, errors.New(name + " is required")
		}
	}
	var err error
	if raw := strings.TrimSpace(getenv("DEVENV_GATEWAY_TRUST_EMAIL_CLAIM")); raw != "" {
		config.TrustEmailClaim, err = strconv.ParseBool(raw)
		if err != nil {
			return RuntimeConfig{}, errors.New("DEVENV_GATEWAY_TRUST_EMAIL_CLAIM must be true or false")
		}
	}
	config.SessionTTL = 8 * time.Hour
	if raw := strings.TrimSpace(getenv("DEVENV_GATEWAY_SESSION_TTL")); raw != "" {
		config.SessionTTL, err = time.ParseDuration(raw)
		if err != nil || config.SessionTTL < time.Minute || config.SessionTTL > 24*time.Hour {
			return RuntimeConfig{}, errors.New("DEVENV_GATEWAY_SESSION_TTL must be between 1m and 24h")
		}
	}
	config.Upstreams, err = parseUpstreamPolicy(
		getenv("DEVENV_GATEWAY_UPSTREAM_CIDRS"), getenv("DEVENV_GATEWAY_UPSTREAM_PORTS"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	return config, nil
}

func parseUpstreamPolicy(rawCIDRs, rawPorts string) (UpstreamPolicy, error) {
	policy := UpstreamPolicy{}
	for _, raw := range strings.Split(rawCIDRs, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || !prefix.IsValid() || prefix != prefix.Masked() {
			return UpstreamPolicy{}, fmt.Errorf("invalid DEVENV_GATEWAY_UPSTREAM_CIDRS entry %q", raw)
		}
		policy.Prefixes = append(policy.Prefixes, prefix)
	}
	for _, raw := range strings.Split(rawPorts, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		port, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || port == 0 {
			return UpstreamPolicy{}, fmt.Errorf("invalid DEVENV_GATEWAY_UPSTREAM_PORTS entry %q", raw)
		}
		policy.Ports = append(policy.Ports, strconv.FormatUint(port, 10))
	}
	if len(policy.Prefixes) == 0 || len(policy.Ports) == 0 {
		return UpstreamPolicy{}, errors.New("DEVENV_GATEWAY_UPSTREAM_CIDRS and DEVENV_GATEWAY_UPSTREAM_PORTS are required")
	}
	return policy, nil
}
