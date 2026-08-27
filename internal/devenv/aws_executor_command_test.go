package devenv

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type commandSSM struct {
	sent       *ssm.SendCommandInput
	invocation *ssm.GetCommandInvocationOutput
}

func (s *commandSSM) SendCommand(_ context.Context, input *ssm.SendCommandInput,
	_ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	s.sent = input
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("command-1")}}, nil
}

func (s *commandSSM) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput,
	...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	return s.invocation, nil
}

func TestSSMExecutionTimeoutEndsBeforeLocalClaimRelease(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	client := &commandSSM{invocation: &ssm.GetCommandInvocationOutput{
		Status: ssmtypes.CommandInvocationStatusTimedOut,
	}}
	executor := &AWSExecutor{
		ssm: client, config: AWSExecutorConfig{CommandTimeout: 13 * time.Minute},
		now:   func() time.Time { return now },
		sleep: func(context.Context, time.Duration) error { return nil },
	}
	_, err := executor.runCommandOutput(context.Background(), "i-1", []string{"true"})
	var retryable *RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("SSM timeout is not retryable: %v", err)
	}
	want := strconv.Itoa(int((13*time.Minute - ssmTimeoutGrace).Seconds()))
	if got := client.sent.Parameters["executionTimeout"]; len(got) != 1 || got[0] != want {
		t.Fatalf("execution timeout = %#v, want %q", got, want)
	}
	if got := aws.ToInt32(client.sent.TimeoutSeconds); got != 30 {
		t.Fatalf("delivery timeout = %d, want 30", got)
	}
}
