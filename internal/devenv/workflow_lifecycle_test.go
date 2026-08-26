package devenv

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
)

type captureScheduler struct {
	SchedulerAPI
	creates []*scheduler.CreateScheduleInput
}

func (s *captureScheduler) CreateSchedule(_ context.Context, input *scheduler.CreateScheduleInput,
	_ ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error) {
	s.creates = append(s.creates, input)
	return &scheduler.CreateScheduleOutput{}, nil
}

type captureTagger struct {
	EC2TagAPI
	input *ec2.CreateTagsInput
}

func (t *captureTagger) CreateTags(_ context.Context, input *ec2.CreateTagsInput,
	_ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	t.input = input
	return &ec2.CreateTagsOutput{}, nil
}

func TestLeaseSchedulesAreVersionFenced(t *testing.T) {
	schedulerClient := &captureScheduler{}
	workflow := NewAWSWorkflow(nil, schedulerClient, nil, "", "lease-target", "scheduler-role", "leases", "")
	env := Environment{
		ID: strings.Repeat("x", 64), State: StateRunning,
		Lease: Lease{
			IdleExpiresAt: time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC),
			HardExpiresAt: time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC),
			Version:       3,
		},
	}
	if err := workflow.ScheduleLease(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	env.Lease.Version = 4
	if err := workflow.ScheduleLease(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(schedulerClient.creates) != 2 {
		t.Fatalf("unexpected schedule calls: %d", len(schedulerClient.creates))
	}
	first := aws.ToString(schedulerClient.creates[0].Name)
	second := aws.ToString(schedulerClient.creates[1].Name)
	if first == second || !strings.HasSuffix(first, "-v3") || !strings.HasSuffix(second, "-v4") {
		t.Fatalf("schedule names are not generation-fenced: %q %q", first, second)
	}
	if len(first) > 64 || len(second) > 64 {
		t.Fatalf("schedule name exceeds EventBridge limit: %q %q", first, second)
	}
}

func TestErrorRetryDoesNotExtendComputeDeadline(t *testing.T) {
	schedulerClient := &captureScheduler{}
	tagger := &captureTagger{}
	workflow := NewAWSWorkflow(nil, schedulerClient, tagger, "", "lease-target", "scheduler-role", "leases", "")
	cleanupAfter := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	retryAfter := time.Date(2026, 8, 27, 19, 0, 0, 0, time.UTC)
	env := Environment{
		ID: "env-1", InstanceID: "i-1", State: StateError, Lease: Lease{Version: 8},
		CleanupAfter: &cleanupAfter, RecoveryRetryAfter: &retryAfter,
	}
	if err := workflow.ScheduleLease(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(schedulerClient.creates[0].ScheduleExpression); got != "at(2026-08-27T19:00:00)" {
		t.Fatalf("unexpected retry schedule: %q", got)
	}
	if tagger.input == nil {
		t.Fatal("instance was not tagged with its cleanup deadline")
	}
	var expiresAt string
	for _, tag := range tagger.input.Tags {
		if aws.ToString(tag.Key) == expiresTag {
			expiresAt = aws.ToString(tag.Value)
		}
	}
	if expiresAt != cleanupAfter.Format(time.RFC3339) {
		t.Fatalf("recovery retry extended compute deadline: %q", expiresAt)
	}
}
