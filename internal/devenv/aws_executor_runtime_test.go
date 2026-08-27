package devenv

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestRemoteRuntimeUsesCloudDependenciesAndPreservesSafetyControls(t *testing.T) {
	environment := Environment{ID: "env-1234567890", Name: "feature", OwnerEmail: "alice@example.com", Profile: ProfileGPUCVML}
	result := WorkflowResult{Host: "alice-feature-12345678.preview.example.test", WorkspaceVolumeID: "vol-123", LogtoAppID: "app-1"}
	config := AWSExecutorConfig{WorkspacePort: 8443, SharedCVMLEndpoint: "https://shared-cvml.example.test"}
	compose := remoteCompose(config, environment, result)
	for _, expected := range []string{
		"--configs=local,dev-stack", "DRNT_DATA_S3_ENDPOINT: \"http://blobs:9000\"",
		"DRNT_SERVICES_CVML_HTTPENDPOINT: \"http://cvml:8082\"", "gpus: all",
	} {
		if !strings.Contains(compose, expected) {
			t.Fatalf("remote compose is missing %q:\n%s", expected, compose)
		}
	}
	if strings.Contains(compose, "with-logto") {
		t.Fatal("remote compose points at laptop-local Logto")
	}
	commands, err := startRuntimeCommands(config, environment, result, "https://signed.example.test/source")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	for index, command := range commands {
		if len(command) > 20<<10 {
			t.Fatalf("SSM command %d is too large: %d bytes", index, len(command))
		}
	}
	for _, expected := range []string{
		"curl --fail", "source.tgz", "duranta-bootstrap-watchdog.service", "VITE_LOGTO_APP_ID='app-1'",
		"source-extract.py extract", "source-extract.py apply", "/workspace/repo.next",
		"GIT_LFS_SKIP_SMUDGE=1 git clone",
		"chown -R ssm-user:ssm-user /workspace/repo", "d[\"data-root\"]=\"/workspace/docker\"",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("runtime commands are missing %q", expected)
		}
	}
	if strings.Contains(joined, "aws s3") {
		t.Fatal("workspace bootstrap must not use instance S3 credentials")
	}
	if strings.Contains(joined, "tar -x") || strings.Contains(joined, "cp -a /workspace/runtime/source") {
		t.Fatal("workspace bootstrap bypasses the safe source extractor")
	}
	watchdog := watchdogScript()
	for _, expected := range []string{"last-valid-deadline", "missing-deadline", "hard-deadline", "docker compose"} {
		if !strings.Contains(watchdog, expected) {
			t.Fatalf("watchdog is missing %q", expected)
		}
	}
}

func TestAWSResourceTagsAreHumanReadableAndCoverLaunchENI(t *testing.T) {
	environment := Environment{
		ID: "env-1", Name: "classification-ui", OwnerID: "owner:v1:abc",
		OwnerEmail: "alice@example.com", Lease: NewLease(time.Unix(1_700_000_000, 0)),
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	specifications := launchTagSpecifications(environment, "op-1")
	resources := map[ec2types.ResourceType]map[string]string{}
	for _, specification := range specifications {
		resources[specification.ResourceType] = tagMap(specification.Tags)
	}
	for _, resource := range []ec2types.ResourceType{
		ec2types.ResourceTypeInstance, ec2types.ResourceTypeVolume, ec2types.ResourceTypeNetworkInterface,
	} {
		if resources[resource][environmentTag] != environment.ID ||
			resources[resource]["duranta:owner-email"] != environment.OwnerEmail ||
			resources[resource]["duranta:display-name"] != environment.Name ||
			resources[resource]["duranta:created-at"] == "" {
			t.Fatalf("incomplete %s tags: %#v", resource, resources[resource])
		}
	}
	operation := Operation{ID: "op-1", CheckpointPinned: true}
	snapshot := tagMap(snapshotTags(environment, operation, "alice-classification-ui-20260827-120000"))
	if snapshot["duranta:checkpoint-name"] == "" || snapshot["duranta:checkpoint-pinned"] != "true" ||
		!strings.Contains(snapshotDescription(environment, snapshot["duranta:checkpoint-name"]), snapshot["duranta:checkpoint-name"]) {
		t.Fatalf("incomplete snapshot metadata: %#v", snapshot)
	}
}

func TestTaggedResourceAgeUsesCreatedAtOrExpiredLease(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if !taggedResourceOld([]ec2types.Tag{{
		Key: aws.String("duranta:created-at"), Value: aws.String(now.Add(-time.Hour).Format(time.RFC3339)),
	}}, now, 15*time.Minute) {
		t.Fatal("old tagged resource was not eligible for cleanup")
	}
	if taggedResourceOld([]ec2types.Tag{{
		Key: aws.String(expiresTag), Value: aws.String(now.Add(-time.Minute).Format(time.RFC3339)),
	}}, now, 15*time.Minute) {
		t.Fatal("resource inside cleanup grace was eligible")
	}
}

func TestArchiveDoesNotReuseRestoredCheckpointSnapshot(t *testing.T) {
	if got := reusableArchiveSnapshot(Environment{
		CurrentCheckpoint: "checkpoint-source", CurrentSnapshotID: "snap-source",
	}); got != "" {
		t.Fatalf("restored source snapshot would be reused: %q", got)
	}
	if got := reusableArchiveSnapshot(Environment{CurrentSnapshotID: "snap-partial-archive"}); got != "snap-partial-archive" {
		t.Fatalf("partial archive snapshot was not reused: %q", got)
	}
}

func TestPrimaryNetworkInterfaceIsPrivateAndEphemeral(t *testing.T) {
	configuration := primaryNetworkInterface("subnet-private", "sg-workspace")
	if aws.ToInt32(configuration.DeviceIndex) != 0 || aws.ToString(configuration.SubnetId) != "subnet-private" ||
		len(configuration.Groups) != 1 || configuration.Groups[0] != "sg-workspace" ||
		configuration.AssociatePublicIpAddress == nil || aws.ToBool(configuration.AssociatePublicIpAddress) ||
		configuration.Ipv6AddressCount == nil || aws.ToInt32(configuration.Ipv6AddressCount) != 0 ||
		!aws.ToBool(configuration.DeleteOnTermination) {
		t.Fatalf("unsafe primary network interface: %#v", configuration)
	}
	if err := (AWSExecutorConfig{
		CPULaunchTemplateID: "lt-cpu", GPULaunchTemplateID: "lt-gpu", SubnetIDs: []string{"subnet-private"},
		VolumeSizeGiB: 250, KMSKeyARN: "kms", SecurityGroupID: "", PreviewBaseDomain: "preview.test",
		WorkspacePort: 8443, InstanceRoleARN: "arn:aws:iam::123:role/workspace",
	}).Validate(); err == nil {
		t.Fatal("executor accepted a missing workspace security group")
	}
}

func TestPreviewHostAlwaysKeepsRandomSuffix(t *testing.T) {
	environment := Environment{
		ID: "env-0123456789abcdef", Name: strings.Repeat("long-name-", 8),
		OwnerEmail: strings.Repeat("owner", 10) + "@example.com",
	}
	host := previewHost(environment, "preview.example.test")
	label := strings.Split(host, ".")[0]
	if len(label) > 63 || !strings.HasSuffix(label, "-89abcdef") {
		t.Fatalf("suffix was truncated from %q", host)
	}
}
