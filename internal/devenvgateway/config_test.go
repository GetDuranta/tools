package devenvgateway

import (
	"testing"
	"time"
)

func TestParseRuntimeConfigRequiresExplicitWorkspaceNetwork(t *testing.T) {
	values := map[string]string{
		"DEVENV_GATEWAY_TABLE": "dev-envs", "AWS_REGION": "us-west-2",
		"DEVENV_GATEWAY_CONTROL_HOST":   "dev.example.test",
		"DEVENV_GATEWAY_PREVIEW_SUFFIX": "preview.example.test",
		"DEVENV_GATEWAY_ALB_ARN":        "arn:aws:elasticloadbalancing:us-west-2:123:loadbalancer/app/dev/1",
		"DEVENV_GATEWAY_ALB_CLIENT_ID":  "client-1",
		"DEVENV_OWNER_NAMESPACE":        "aws:123:be-dev",
		"DEVENV_CONTROL_API_URL":        "https://api.example.test",
	}
	getenv := func(name string) string { return values[name] }
	if _, err := ParseRuntimeConfig(getenv); err == nil {
		t.Fatal("workspace network should be required")
	}
	values["DEVENV_GATEWAY_UPSTREAM_CIDRS"] = "10.24.16.0/20, 10.24.32.0/20"
	values["DEVENV_GATEWAY_UPSTREAM_PORTS"] = "80,8080"
	values["DEVENV_GATEWAY_SESSION_TTL"] = "6h"
	config, err := ParseRuntimeConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenAddress != ":8080" || config.SessionTTL != 6*time.Hour ||
		len(config.Upstreams.Prefixes) != 2 || len(config.Upstreams.Ports) != 2 {
		t.Fatalf("unexpected config: %#v", config)
	}
}

func TestParseRuntimeConfigRejectsNonCanonicalCIDR(t *testing.T) {
	values := map[string]string{
		"DEVENV_GATEWAY_TABLE": "dev-envs", "AWS_REGION": "us-west-2",
		"DEVENV_GATEWAY_CONTROL_HOST":   "dev.example.test",
		"DEVENV_GATEWAY_PREVIEW_SUFFIX": "preview.example.test",
		"DEVENV_GATEWAY_ALB_ARN":        "arn:aws:elasticloadbalancing:us-west-2:123:loadbalancer/app/dev/1",
		"DEVENV_GATEWAY_ALB_CLIENT_ID":  "client-1",
		"DEVENV_OWNER_NAMESPACE":        "aws:123:be-dev", "DEVENV_GATEWAY_UPSTREAM_CIDRS": "10.24.16.1/20",
		"DEVENV_CONTROL_API_URL":        "https://api.example.test",
		"DEVENV_GATEWAY_UPSTREAM_PORTS": "8080",
	}
	if _, err := ParseRuntimeConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("non-canonical CIDR was accepted")
	}
}
