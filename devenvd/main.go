package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GetDuranta/tools/internal/devaccess"
	"github.com/GetDuranta/tools/internal/devenv"
	"github.com/GetDuranta/tools/internal/devenvgateway"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

func main() {
	ctx := context.Background()
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatal(err)
	}
	store := devenv.NewDynamoStore(dynamodb.NewFromConfig(awsConfig), required("STATE_TABLE_NAME"),
		required("OWNER_INDEX_NAME"), required("DUE_INDEX_NAME"))
	ec2Client := ec2.NewFromConfig(awsConfig)
	workflow := devenv.NewAWSWorkflow(
		awslambda.NewFromConfig(awsConfig), scheduler.NewFromConfig(awsConfig), ec2Client,
		required("RECONCILER_FUNCTION_ARN"), required("LEASE_TARGET_ARN"),
		required("SCHEDULER_ROLE_ARN"), required("SCHEDULER_GROUP_NAME"),
		required("LIFECYCLE_DLQ_ARN"),
	)
	sourceBundles := devenv.S3SourceUploader{
		Client: s3.NewPresignClient(s3.NewFromConfig(awsConfig)), Bucket: required("SOURCE_STAGING_BUCKET"),
	}
	cloud := devenv.BootstrapCloud{Issuer: devaccess.BootstrapIssuer{Store: store}, Uploader: sourceBundles}
	service := devenv.NewService(store, workflow, cloud, nil)
	if err = service.SetQuotaLimits(devenv.QuotaLimits{
		MaxOwnerWorkspaces:  integer("MAX_OWNER_WORKSPACES", devenv.DefaultMaxOwnerSlots),
		MaxWorkspaces:       integer("MAX_WORKSPACES", devenv.DefaultMaxWorkspaces),
		MaxRunning:          integer("MAX_RUNNING", devenv.DefaultMaxRunning),
		MaxGPURunning:       nonNegativeInteger("MAX_GPU_RUNNING", devenv.DefaultMaxGPURunning),
		MaxOwnerCheckpoints: integer("MAX_OWNER_CHECKPOINTS", devenv.DefaultMaxOwnerCheckpoints),
		MaxOwnerPinnedCheckpoints: integer("MAX_OWNER_PINNED_CHECKPOINTS",
			devenv.DefaultMaxOwnerPinnedCheckpoints),
		MaxCheckpoints:       integer("MAX_CHECKPOINTS", devenv.DefaultMaxCheckpoints),
		MaxPinnedCheckpoints: integer("MAX_PINNED_CHECKPOINTS", devenv.DefaultMaxPinnedCheckpoints),
	}); err != nil {
		log.Fatal(err)
	}

	switch handlerMode() {
	case "api":
		gatewayVerifier := devenvgateway.NewALBOIDCVerifier(awsConfig.Region, required("ALB_SIGNER_ARN"),
			required("ALB_CLIENT_ID"), required("OWNER_NAMESPACE"))
		gatewayVerifier.TrustEmailClaim = boolean("ALB_TRUST_EMAIL_CLAIM", false)
		handler := &devenv.HTTPHandler{
			Service: service,
			Identity: devenv.IAMIdentityResolver{
				Namespace:               required("OWNER_NAMESPACE"),
				TrustedAutomationRoles:  values("TRUSTED_AUTOMATION_ROLE_ARNS"),
				InteractiveRolePrefixes: csv("INTERACTIVE_ROLE_ARN_PREFIXES"),
				GatewayRoleARN:          required("GATEWAY_ROLE_ARN"),
			},
			VerifyGatewayOIDC: func(ctx context.Context, token string) (string, string, error) {
				identity, verifyErr := gatewayVerifier.Verify(ctx, token)
				return identity.Email, identity.Subject, verifyErr
			},
		}
		lambda.Start(handler.Handle)
	case "reconcile":
		ssmClient := ssm.NewFromConfig(awsConfig)
		previewApps := &devenv.LogtoPreviewApps{
			Endpoint:           required("LOGTO_ADMIN_BASE_URL"),
			ManagementResource: envOr("LOGTO_MANAGEMENT_RESOURCE", "https://default.logto.app/api"),
			ClientID:           secretParameter(ctx, ssmClient, required("LOGTO_M2M_CLIENT_ID_PARAMETER")),
			ClientSecret:       secretParameter(ctx, ssmClient, required("LOGTO_M2M_CLIENT_SECRET_PARAMETER")),
		}
		executor, executorErr := devenv.NewAWSExecutor(ec2Client, ssmClient, previewApps, sourceBundles,
			devenv.AWSExecutorConfig{
				CPULaunchTemplateID: required("CPU_LAUNCH_TEMPLATE_ID"),
				GPULaunchTemplateID: required("GPU_LAUNCH_TEMPLATE_ID"),
				SubnetIDs:           csv("PRIVATE_SUBNET_IDS"),
				VolumeSizeGiB:       int32(integer("WORKSPACE_VOLUME_SIZE_GIB", 250)),
				KMSKeyARN:           required("WORKSPACE_KMS_KEY_ARN"),
				SecurityGroupID:     required("WORKSPACE_SECURITY_GROUP_ID"),
				PreviewBaseDomain:   required("PREVIEW_BASE_DOMAIN"),
				WorkspacePort:       integer("WORKSPACE_PORT", 8443),
				InstanceRoleARN:     required("INSTANCE_ROLE_ARN"),
				SharedCVMLEndpoint:  required("SHARED_CVML_ENDPOINT"),
				CommandTimeout:      13 * time.Minute,
			})
		if executorErr != nil {
			log.Fatal(executorErr)
		}
		worker := &devenv.Worker{Service: service, Store: store, Executor: executor}
		lambda.Start(func(ctx context.Context, raw json.RawMessage) (any, error) {
			return handleReconciler(ctx, worker, raw)
		})
	default:
		log.Fatalf("unsupported Lambda handler %q", os.Getenv("_HANDLER"))
	}
}

func handleReconciler(ctx context.Context, worker *devenv.Worker, raw json.RawMessage) (any, error) {
	var envelope struct {
		Kind          string        `json:"kind"`
		EnvironmentID string        `json:"environmentId"`
		LeaseVersion  int64         `json:"leaseVersion"`
		ID            string        `json:"id"`
		Action        devenv.Action `json:"action"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Kind == "lease_expired" {
		return nil, worker.HandleLease(ctx, devenv.LeaseExpiry{
			EnvironmentID: envelope.EnvironmentID, LeaseVersion: envelope.LeaseVersion,
		})
	}
	if envelope.ID != "" && envelope.Action != "" {
		var operation devenv.Operation
		if err := json.Unmarshal(raw, &operation); err != nil {
			return nil, err
		}
		return nil, worker.HandleOperation(ctx, operation)
	}
	return worker.HandlePeriodic(ctx)
}

func handlerMode() string {
	handler := strings.TrimSpace(os.Getenv("_HANDLER"))
	if strings.HasSuffix(handler, ".reconcile") || handler == "reconcile" {
		return "reconcile"
	}
	if strings.HasSuffix(handler, ".api") || handler == "api" || handler == "" {
		return "api"
	}
	return handler
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func integer(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	result, err := strconv.Atoi(raw)
	if err != nil || result < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return result
}

func nonNegativeInteger(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	result, err := strconv.Atoi(raw)
	if err != nil || result < 0 {
		log.Fatalf("%s must be a non-negative integer", name)
	}
	return result
}

func boolean(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		log.Fatalf("%s must be a boolean", name)
	}
	return value
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func secretParameter(ctx context.Context, client *ssm.Client, name string) string {
	output, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil || output.Parameter == nil || strings.TrimSpace(aws.ToString(output.Parameter.Value)) == "" {
		log.Fatalf("load required SSM parameter %s: %v", name, err)
	}
	return aws.ToString(output.Parameter.Value)
}

func values(name string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, item := range csv(name) {
		result[item] = struct{}{}
	}
	return result
}

func csv(name string) []string {
	result := []string{}
	for _, item := range strings.Split(os.Getenv(name), ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
