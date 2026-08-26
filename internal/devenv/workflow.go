package devenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

type LambdaInvokeAPI interface {
	Invoke(context.Context, *awslambda.InvokeInput,
		...func(*awslambda.Options)) (*awslambda.InvokeOutput, error)
}

type SchedulerAPI interface {
	CreateSchedule(context.Context, *scheduler.CreateScheduleInput,
		...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error)
	UpdateSchedule(context.Context, *scheduler.UpdateScheduleInput,
		...func(*scheduler.Options)) (*scheduler.UpdateScheduleOutput, error)
}

type EC2TagAPI interface {
	CreateTags(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

type AWSWorkflow struct {
	invoker           LambdaInvokeAPI
	scheduler         SchedulerAPI
	tagger            EC2TagAPI
	reconcilerARN     string
	leaseTargetARN    string
	schedulerRoleARN  string
	scheduleGroupName string
	deadLetterARN     string
}

func NewAWSWorkflow(invoker LambdaInvokeAPI, schedulerClient SchedulerAPI, tagger EC2TagAPI, reconcilerARN,
	leaseTargetARN, schedulerRoleARN, scheduleGroupName, deadLetterARN string) *AWSWorkflow {
	return &AWSWorkflow{
		invoker: invoker, scheduler: schedulerClient, tagger: tagger, reconcilerARN: reconcilerARN,
		leaseTargetARN: leaseTargetARN, schedulerRoleARN: schedulerRoleARN, scheduleGroupName: scheduleGroupName,
		deadLetterARN: deadLetterARN,
	}
}

func (w *AWSWorkflow) Start(ctx context.Context, operation Operation) (string, error) {
	if w.invoker == nil || w.reconcilerARN == "" {
		return "", errors.New("reconciler Lambda is not configured")
	}
	payload, err := json.Marshal(operation)
	if err != nil {
		return "", err
	}
	output, err := w.invoker.Invoke(ctx, &awslambda.InvokeInput{
		FunctionName: aws.String(w.reconcilerARN), InvocationType: lambdatypes.InvocationTypeEvent,
		Payload: payload,
	})
	if err != nil {
		return "", err
	}
	if output.StatusCode != 202 {
		return "", fmt.Errorf("reconciler invocation returned HTTP %d", output.StatusCode)
	}
	return w.reconcilerARN + "#" + operation.ID, nil
}

func (w *AWSWorkflow) ScheduleLease(ctx context.Context, env Environment) error {
	if w.scheduler == nil || w.leaseTargetARN == "" || w.schedulerRoleARN == "" || w.scheduleGroupName == "" {
		return errors.New("lease scheduler is not configured")
	}
	dueAt := env.ScheduledActionAt()
	tagDeadline := dueAt
	if env.State == StateError && env.CleanupAfter != nil {
		tagDeadline = *env.CleanupAfter
	}
	payload, err := json.Marshal(struct {
		Kind          string `json:"kind"`
		EnvironmentID string `json:"environmentId"`
		LeaseVersion  int64  `json:"leaseVersion"`
	}{Kind: "lease_expired", EnvironmentID: env.ID, LeaseVersion: env.Lease.Version})
	if err != nil {
		return err
	}
	name := scheduleName(env.ID, env.Lease.Version)
	expression := "at(" + dueAt.UTC().Format("2006-01-02T15:04:05") + ")"
	target := &schedulertypes.Target{
		Arn: aws.String(w.leaseTargetARN), RoleArn: aws.String(w.schedulerRoleARN),
		Input: aws.String(string(payload)), RetryPolicy: &schedulertypes.RetryPolicy{
			MaximumEventAgeInSeconds: aws.Int32(3600), MaximumRetryAttempts: aws.Int32(10),
		},
	}
	if w.deadLetterARN != "" {
		target.DeadLetterConfig = &schedulertypes.DeadLetterConfig{Arn: aws.String(w.deadLetterARN)}
	}
	flexible := &schedulertypes.FlexibleTimeWindow{Mode: schedulertypes.FlexibleTimeWindowModeOff}
	_, err = w.scheduler.CreateSchedule(ctx, &scheduler.CreateScheduleInput{
		Name: aws.String(name), GroupName: aws.String(w.scheduleGroupName),
		ScheduleExpression: aws.String(expression), ScheduleExpressionTimezone: aws.String("UTC"),
		FlexibleTimeWindow: flexible, Target: target, ActionAfterCompletion: schedulertypes.ActionAfterCompletionDelete,
		ClientToken: aws.String(scheduleToken(env.ID, env.Lease.Version)),
	})
	var conflict *schedulertypes.ConflictException
	if err == nil {
		return w.tagLease(ctx, env, tagDeadline)
	}
	if !errors.As(err, &conflict) {
		return err
	}
	_, err = w.scheduler.UpdateSchedule(ctx, &scheduler.UpdateScheduleInput{
		Name: aws.String(name), GroupName: aws.String(w.scheduleGroupName),
		ScheduleExpression: aws.String(expression), ScheduleExpressionTimezone: aws.String("UTC"),
		FlexibleTimeWindow: flexible, Target: target, ActionAfterCompletion: schedulertypes.ActionAfterCompletionDelete,
	})
	if err != nil {
		return err
	}
	return w.tagLease(ctx, env, tagDeadline)
}

func (w *AWSWorkflow) tagLease(ctx context.Context, env Environment, dueAt time.Time) error {
	if env.InstanceID == "" {
		return nil
	}
	if w.tagger == nil {
		return errors.New("EC2 lease tagger is not configured")
	}
	_, err := w.tagger.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{env.InstanceID}, Tags: []ec2types.Tag{
			{Key: aws.String(expiresTag), Value: aws.String(dueAt.UTC().Format(time.RFC3339))},
			{Key: aws.String("duranta:lease-version"), Value: aws.String(fmt.Sprintf("%d", env.Lease.Version))},
		},
	})
	return err
}

func scheduleName(environmentID string, version int64) string {
	suffix := fmt.Sprintf("-v%d", version)
	prefix := "duranta-dev-" + environmentID
	if len(prefix)+len(suffix) > 64 {
		prefix = strings.TrimRight(prefix[:64-len(suffix)], "-")
	}
	return prefix + suffix
}

func scheduleToken(environmentID string, version int64) string {
	return fmt.Sprintf("%s-%d", strings.TrimPrefix(environmentID, "env-"), version)
}
