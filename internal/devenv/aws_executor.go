package devenv

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
)

const (
	managedTag     = "duranta:managed-by"
	environmentTag = "duranta:environment-id"
	ownerTag       = "duranta:owner-key"
	expiresTag     = "duranta:expires-at"
	resourceTag    = "duranta:resource"
)

type EC2API interface {
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput,
		...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput,
		...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeVolumes(context.Context, *ec2.DescribeVolumesInput,
		...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DescribeSnapshots(context.Context, *ec2.DescribeSnapshotsInput,
		...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
	DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput,
		...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	CreateVolume(context.Context, *ec2.CreateVolumeInput,
		...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error)
	RunInstances(context.Context, *ec2.RunInstancesInput,
		...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	CreateTags(context.Context, *ec2.CreateTagsInput,
		...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	AttachVolume(context.Context, *ec2.AttachVolumeInput,
		...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error)
	StartInstances(context.Context, *ec2.StartInstancesInput,
		...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(context.Context, *ec2.StopInstancesInput,
		...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput,
		...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CreateSnapshot(context.Context, *ec2.CreateSnapshotInput,
		...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error)
	DeleteVolume(context.Context, *ec2.DeleteVolumeInput,
		...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	DeleteSnapshot(context.Context, *ec2.DeleteSnapshotInput,
		...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)
	DeleteNetworkInterface(context.Context, *ec2.DeleteNetworkInterfaceInput,
		...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error)
}

type SSMAPI interface {
	SendCommand(context.Context, *ssm.SendCommandInput,
		...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput,
		...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

type PreviewApps interface {
	Ensure(context.Context, Environment) (string, error)
	Delete(context.Context, string) error
}

type AWSExecutorConfig struct {
	CPULaunchTemplateID string
	GPULaunchTemplateID string
	SubnetIDs           []string
	VolumeSizeGiB       int32
	KMSKeyARN           string
	SecurityGroupID     string
	PreviewBaseDomain   string
	WorkspacePort       int
	InstanceRoleARN     string
	SharedCVMLEndpoint  string
	DeviceName          string
	CommandTimeout      time.Duration
}

func (c AWSExecutorConfig) Validate() error {
	if c.CPULaunchTemplateID == "" || c.GPULaunchTemplateID == "" || len(c.SubnetIDs) == 0 ||
		c.VolumeSizeGiB < 1 || c.KMSKeyARN == "" || c.SecurityGroupID == "" || c.PreviewBaseDomain == "" ||
		c.WorkspacePort < 1 ||
		c.InstanceRoleARN == "" {
		return errors.New("incomplete AWS executor configuration")
	}
	return nil
}

type AWSExecutor struct {
	ec2         EC2API
	ssm         SSMAPI
	previewApps PreviewApps
	sources     SourceDownloader
	config      AWSExecutorConfig
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

func NewAWSExecutor(ec2Client EC2API, ssmClient SSMAPI, previewApps PreviewApps, sources SourceDownloader,
	config AWSExecutorConfig) (*AWSExecutor, error) {
	if ec2Client == nil || ssmClient == nil || sources == nil {
		return nil, errors.New("EC2, SSM, and source downloader are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.DeviceName == "" {
		config.DeviceName = "/dev/sdf"
	}
	if config.CommandTimeout <= 0 {
		config.CommandTimeout = 12 * time.Minute
	}
	return &AWSExecutor{
		ec2: ec2Client, ssm: ssmClient, previewApps: previewApps, sources: sources, config: config,
		now: func() time.Time { return time.Now().UTC() }, sleep: sleepContext,
	}, nil
}

func (e *AWSExecutor) Execute(ctx context.Context, env Environment, operation Operation) (WorkflowResult, error) {
	switch operation.Action {
	case ActionCreate, ActionStart:
		return e.start(ctx, env, operation)
	case ActionStop:
		return e.stop(ctx, env)
	case ActionArchive:
		return e.archive(ctx, env, operation)
	case ActionDelete:
		return e.delete(ctx, env)
	default:
		return WorkflowResult{}, fmt.Errorf("unsupported lifecycle action %q", operation.Action)
	}
}

func (e *AWSExecutor) start(ctx context.Context, env Environment,
	operation Operation) (result WorkflowResult, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = &ExecutionError{Result: result, Err: resultErr}
		}
	}()
	result.Host = previewHost(env, e.config.PreviewBaseDomain)
	if e.previewApps != nil {
		previewEnv := env
		previewEnv.Host = result.Host
		result.LogtoAppID, resultErr = e.previewApps.Ensure(ctx, previewEnv)
		if resultErr != nil {
			return result, resultErr
		}
	} else {
		result.LogtoAppID = env.LogtoAppID
	}
	instance, err := e.findInstance(ctx, env.ID)
	if err != nil {
		return result, err
	}
	volume, err := e.findWorkspaceVolume(ctx, env.ID)
	if err != nil {
		return result, err
	}
	if instance != nil {
		result.InstanceID = aws.ToString(instance.InstanceId)
		result.WorkspaceVolumeID = env.WorkspaceVolumeID
		if volume != nil {
			result.WorkspaceVolumeID = aws.ToString(volume.VolumeId)
		}
	} else {
		result, err = e.provision(ctx, env, operation, result.Host, result.LogtoAppID, volume)
		if err != nil {
			return result, err
		}
	}
	if _, err = e.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{result.InstanceID},
		Tags:      resourceTags(env, operation.ID, "workspace-instance"),
	}); err != nil {
		return result, err
	}
	if instance != nil && instance.State != nil && instance.State.Name == ec2types.InstanceStateNameStopped {
		_, err = e.ec2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{result.InstanceID}})
		if err != nil {
			return result, err
		}
	}
	instance, err = e.waitInstance(ctx, result.InstanceID, ec2types.InstanceStateNameRunning)
	if err != nil {
		return result, err
	}
	volume, err = e.resolveVolume(ctx, Environment{ID: env.ID, WorkspaceVolumeID: result.WorkspaceVolumeID})
	if err != nil || volume == nil {
		return result, errors.Join(errors.New("workspace volume not found after launch"), err)
	}
	if err = e.ensureVolumeAttached(ctx, result.InstanceID, volume); err != nil {
		return result, err
	}
	result.InstanceRoleARN = e.config.InstanceRoleARN
	result.PrivateUpstream = "https://" + aws.ToString(instance.PrivateIpAddress) + ":" + strconv.Itoa(e.config.WorkspacePort)
	fingerprint, err := e.startRuntime(ctx, env, result)
	if err != nil {
		return result, err
	}
	result.TLSCertSHA256 = fingerprint
	return result, nil
}

func (e *AWSExecutor) provision(ctx context.Context, env Environment, operation Operation, host, logtoAppID string,
	existingVolume *ec2types.Volume) (WorkflowResult, error) {
	subnetID := e.config.SubnetIDs[stableIndex(env.ID, len(e.config.SubnetIDs))]
	subnets, err := e.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if err != nil || len(subnets.Subnets) != 1 {
		return WorkflowResult{}, errors.Join(errors.New("resolve workspace subnet"), err)
	}
	volume := existingVolume
	if volume == nil {
		input := &ec2.CreateVolumeInput{
			AvailabilityZone: subnets.Subnets[0].AvailabilityZone, Encrypted: aws.Bool(true),
			KmsKeyId: aws.String(e.config.KMSKeyARN), Size: aws.Int32(e.config.VolumeSizeGiB),
			VolumeType:  ec2types.VolumeTypeGp3,
			ClientToken: aws.String(operationToken(operation.ID, "volume")),
			TagSpecifications: []ec2types.TagSpecification{{
				ResourceType: ec2types.ResourceTypeVolume, Tags: resourceTags(env, operation.ID, "workspace-volume"),
			}},
		}
		if env.CurrentSnapshotID != "" {
			input.SnapshotId = aws.String(env.CurrentSnapshotID)
		}
		created, createErr := e.ec2.CreateVolume(ctx, input)
		if createErr != nil {
			return WorkflowResult{}, createErr
		}
		volumeID := aws.ToString(created.VolumeId)
		volume, err = e.waitVolume(ctx, volumeID, ec2types.VolumeStateAvailable)
		if err != nil {
			return WorkflowResult{WorkspaceVolumeID: volumeID, LogtoAppID: logtoAppID}, err
		}
	}
	volumeID := aws.ToString(volume.VolumeId)
	templateID := e.config.CPULaunchTemplateID
	if env.Profile == ProfileGPUCVML {
		templateID = e.config.GPULaunchTemplateID
	}
	launched, err := e.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(templateID), Version: aws.String("$Default"),
		},
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{
			primaryNetworkInterface(subnetID, e.config.SecurityGroupID),
		},
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		ClientToken:       aws.String(operationToken(operation.ID, "instance")),
		TagSpecifications: launchTagSpecifications(env, operation.ID),
	})
	if err != nil || len(launched.Instances) != 1 {
		return WorkflowResult{WorkspaceVolumeID: volumeID, LogtoAppID: logtoAppID},
			errors.Join(errors.New("launch workspace instance"), err)
	}
	instanceID := aws.ToString(launched.Instances[0].InstanceId)
	return WorkflowResult{
		InstanceID: instanceID, InstanceRoleARN: e.config.InstanceRoleARN,
		WorkspaceVolumeID: volumeID, Host: host, LogtoAppID: logtoAppID,
	}, nil
}

func (e *AWSExecutor) ensureVolumeAttached(ctx context.Context, instanceID string, volume *ec2types.Volume) error {
	for _, attachment := range volume.Attachments {
		if aws.ToString(attachment.InstanceId) == instanceID &&
			attachment.State == ec2types.VolumeAttachmentStateAttached {
			return nil
		}
	}
	if len(volume.Attachments) > 0 || volume.State == ec2types.VolumeStateInUse {
		return &RetryableError{Err: fmt.Errorf("volume %s is attached to another instance", aws.ToString(volume.VolumeId))}
	}
	if volume.State != ec2types.VolumeStateAvailable {
		var err error
		volume, err = e.waitVolume(ctx, aws.ToString(volume.VolumeId), ec2types.VolumeStateAvailable)
		if err != nil {
			return err
		}
	}
	_, err := e.ec2.AttachVolume(ctx, &ec2.AttachVolumeInput{
		Device: aws.String(e.config.DeviceName), InstanceId: aws.String(instanceID), VolumeId: volume.VolumeId,
	})
	if err != nil {
		return &RetryableError{Err: err}
	}
	deadline := e.now().Add(e.config.CommandTimeout)
	for e.now().Before(deadline) {
		output, describeErr := e.ec2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{aws.ToString(volume.VolumeId)}})
		if describeErr == nil && len(output.Volumes) == 1 {
			for _, attachment := range output.Volumes[0].Attachments {
				if aws.ToString(attachment.InstanceId) == instanceID &&
					attachment.State == ec2types.VolumeAttachmentStateAttached {
					return nil
				}
			}
		}
		if err = e.sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
	return &RetryableError{Err: fmt.Errorf("volume %s did not attach to %s", aws.ToString(volume.VolumeId), instanceID)}
}

func (e *AWSExecutor) stop(ctx context.Context, env Environment) (WorkflowResult, error) {
	result := runtimeResult(env)
	instance, err := e.resolveInstance(ctx, env)
	if err != nil || instance == nil {
		return result, err
	}
	if instance.State != nil && instance.State.Name == ec2types.InstanceStateNameRunning {
		if commandErr := e.runCommand(ctx, env.InstanceID, stopRuntimeCommands()); commandErr != nil {
			result.DirtyShutdown = true
		}
		_, err = e.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{env.InstanceID}})
		if err != nil {
			return result, err
		}
	}
	_, err = e.waitInstance(ctx, env.InstanceID, ec2types.InstanceStateNameStopped)
	return result, err
}

func (e *AWSExecutor) archive(ctx context.Context, env Environment, operation Operation) (WorkflowResult, error) {
	result, err := e.stop(ctx, env)
	if err != nil {
		return result, &ExecutionError{Result: result, Err: err}
	}
	var snapshot *ec2types.Snapshot
	if snapshotID := reusableArchiveSnapshot(env); snapshotID != "" {
		snapshot = &ec2types.Snapshot{SnapshotId: aws.String(snapshotID)}
	} else {
		volume, resolveErr := e.resolveVolume(ctx, env)
		if resolveErr != nil || volume == nil {
			return result, errors.Join(errors.New("workspace volume not found"), resolveErr)
		}
		snapshot, err = e.findSnapshot(ctx, env.ID, operation.ID)
		if err != nil {
			return result, err
		}
		if snapshot == nil {
			checkpointName := checkpointForArchive(operation, env, "", operation.RequestedAt).Name
			created, createErr := e.ec2.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
				VolumeId:    volume.VolumeId,
				Description: aws.String(snapshotDescription(env, checkpointName)),
				TagSpecifications: []ec2types.TagSpecification{{
					ResourceType: ec2types.ResourceTypeSnapshot,
					Tags:         snapshotTags(env, operation, checkpointName),
				}},
			})
			if createErr != nil {
				return result, createErr
			}
			snapshot = &ec2types.Snapshot{SnapshotId: created.SnapshotId, State: created.State}
		}
	}
	result.SnapshotID = aws.ToString(snapshot.SnapshotId)
	if _, err = e.waitSnapshot(ctx, result.SnapshotID, ec2types.SnapshotStateCompleted); err != nil {
		return result, &ExecutionError{Result: result, Err: err}
	}
	if err = e.terminateAndDeleteVolume(ctx, env); err != nil {
		return result, &ExecutionError{Result: result, Err: err}
	}
	if e.previewApps != nil && env.LogtoAppID != "" {
		if err = e.previewApps.Delete(ctx, env.LogtoAppID); err != nil {
			return result, &ExecutionError{Result: result, Err: err}
		}
	}
	return result, nil
}

func (e *AWSExecutor) delete(ctx context.Context, env Environment) (WorkflowResult, error) {
	result := runtimeResult(env)
	err := e.terminateAndDeleteVolume(ctx, env)
	if err == nil && e.previewApps != nil && env.LogtoAppID != "" {
		err = e.previewApps.Delete(ctx, env.LogtoAppID)
	}
	return result, err
}

func (e *AWSExecutor) DeleteCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	if checkpoint.SnapshotID == "" {
		return nil
	}
	_, err := e.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(checkpoint.SnapshotID)})
	if isAWSNotFound(err, "InvalidSnapshot.NotFound") {
		return nil
	}
	return err
}

func (e *AWSExecutor) ReconcileOrphans(ctx context.Context, environments []Environment,
	checkpoints []Checkpoint) error {
	environmentByID := make(map[string]Environment, len(environments))
	for _, env := range environments {
		environmentByID[env.ID] = env
	}
	checkpointSnapshots := make(map[string]struct{}, len(checkpoints))
	for _, checkpoint := range checkpoints {
		if checkpoint.State != CheckpointDeleted && checkpoint.SnapshotID != "" {
			checkpointSnapshots[checkpoint.SnapshotID] = struct{}{}
		}
	}
	for _, env := range environments {
		if env.CurrentSnapshotID != "" {
			checkpointSnapshots[env.CurrentSnapshotID] = struct{}{}
		}
	}
	now := e.now()
	var failures []error
	instances, err := e.managedInstances(ctx)
	if err != nil {
		return err
	}
	instanceGroups := groupInstances(instances)
	for environmentID, group := range instanceGroups {
		env, found := environmentByID[environmentID]
		canonical := canonicalInstance(env, group)
		for _, instance := range group {
			if !olderThan(aws.ToTime(instance.LaunchTime), now, DefaultErrorCleanupTTL) {
				continue
			}
			instanceID := aws.ToString(instance.InstanceId)
			if !found || environmentGarbageCollectable(env, now) || instanceID != canonical {
				_, terminateErr := e.ec2.TerminateInstances(ctx,
					&ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}})
				if terminateErr != nil && !isAWSNotFound(terminateErr, "InvalidInstanceID.NotFound") {
					failures = append(failures, fmt.Errorf("terminate orphan instance %s: %w", instanceID, terminateErr))
				}
				continue
			}
			if instance.State != nil && instance.State.Name == ec2types.InstanceStateNameRunning &&
				now.After(environmentSafetyDeadline(env).Add(DefaultErrorCleanupTTL)) {
				_, stopErr := e.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{instanceID}})
				if stopErr != nil {
					failures = append(failures, fmt.Errorf("stop expired instance %s: %w", instanceID, stopErr))
				}
			}
		}
	}
	volumes, err := e.managedWorkspaceVolumes(ctx)
	if err != nil {
		failures = append(failures, err)
	} else {
		for environmentID, group := range groupVolumes(volumes) {
			env, found := environmentByID[environmentID]
			canonical := canonicalVolume(env, group)
			for _, volume := range group {
				volumeID := aws.ToString(volume.VolumeId)
				if (!found || environmentGarbageCollectable(env, now) || volumeID != canonical) &&
					volume.State == ec2types.VolumeStateAvailable &&
					olderThan(aws.ToTime(volume.CreateTime), now, DefaultErrorCleanupTTL) {
					_, deleteErr := e.ec2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: volume.VolumeId})
					if deleteErr != nil && !isAWSNotFound(deleteErr, "InvalidVolume.NotFound") {
						failures = append(failures, fmt.Errorf("delete orphan volume %s: %w", volumeID, deleteErr))
					}
				}
			}
		}
	}
	snapshots, err := e.managedSnapshots(ctx)
	if err != nil {
		failures = append(failures, err)
	} else {
		activeSnapshots := canonicalActiveSnapshots(snapshots, environmentByID, now)
		for _, snapshot := range snapshots {
			if _, keep := checkpointSnapshots[aws.ToString(snapshot.SnapshotId)]; keep {
				continue
			}
			if _, keep := activeSnapshots[aws.ToString(snapshot.SnapshotId)]; keep {
				continue
			}
			if olderThan(aws.ToTime(snapshot.StartTime), now, DefaultErrorCleanupTTL) {
				_, deleteErr := e.ec2.DeleteSnapshot(ctx,
					&ec2.DeleteSnapshotInput{SnapshotId: snapshot.SnapshotId})
				if deleteErr != nil && !isAWSNotFound(deleteErr, "InvalidSnapshot.NotFound") {
					failures = append(failures, fmt.Errorf("delete orphan snapshot %s: %w",
						aws.ToString(snapshot.SnapshotId), deleteErr))
				}
			}
		}
	}
	networkInterfaces, err := e.managedNetworkInterfaces(ctx)
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, networkInterface := range networkInterfaces {
			if networkInterface.Status != ec2types.NetworkInterfaceStatusAvailable ||
				!taggedResourceOld(networkInterface.TagSet, now, DefaultErrorCleanupTTL) {
				continue
			}
			interfaceID := aws.ToString(networkInterface.NetworkInterfaceId)
			_, deleteErr := e.ec2.DeleteNetworkInterface(ctx,
				&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: networkInterface.NetworkInterfaceId})
			if deleteErr != nil && !isAWSNotFound(deleteErr, "InvalidNetworkInterfaceID.NotFound") {
				failures = append(failures, fmt.Errorf("delete orphan network interface %s: %w",
					interfaceID, deleteErr))
			}
		}
	}
	return errors.Join(failures...)
}

func groupInstances(instances []ec2types.Instance) map[string][]ec2types.Instance {
	groups := map[string][]ec2types.Instance{}
	for _, instance := range instances {
		environmentID := tagMap(instance.Tags)[environmentTag]
		groups[environmentID] = append(groups[environmentID], instance)
	}
	return groups
}

func groupVolumes(volumes []ec2types.Volume) map[string][]ec2types.Volume {
	groups := map[string][]ec2types.Volume{}
	for _, volume := range volumes {
		environmentID := tagMap(volume.Tags)[environmentTag]
		groups[environmentID] = append(groups[environmentID], volume)
	}
	return groups
}

func canonicalInstance(env Environment, instances []ec2types.Instance) string {
	if env.InstanceID != "" {
		return env.InstanceID
	}
	ids := make([]string, 0, len(instances))
	for _, instance := range instances {
		if env.ActiveOperationID == "" || tagMap(instance.Tags)["duranta:operation-id"] == env.ActiveOperationID {
			ids = append(ids, aws.ToString(instance.InstanceId))
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func canonicalVolume(env Environment, volumes []ec2types.Volume) string {
	if env.WorkspaceVolumeID != "" {
		return env.WorkspaceVolumeID
	}
	ids := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		if env.ActiveOperationID == "" || tagMap(volume.Tags)["duranta:operation-id"] == env.ActiveOperationID {
			ids = append(ids, aws.ToString(volume.VolumeId))
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func canonicalActiveSnapshots(snapshots []ec2types.Snapshot, environments map[string]Environment,
	now time.Time) map[string]struct{} {
	byOperation := map[string][]ec2types.Snapshot{}
	for _, snapshot := range snapshots {
		tags := tagMap(snapshot.Tags)
		env, found := environments[tags[environmentTag]]
		if !found || env.State != StateArchiving || env.ActiveOperationID == "" ||
			tags["duranta:operation-id"] != env.ActiveOperationID || environmentGarbageCollectable(env, now) {
			continue
		}
		byOperation[env.ActiveOperationID] = append(byOperation[env.ActiveOperationID], snapshot)
	}
	keep := map[string]struct{}{}
	for _, group := range byOperation {
		sort.Slice(group, func(i, j int) bool {
			return aws.ToTime(group[i].StartTime).Before(aws.ToTime(group[j].StartTime))
		})
		if len(group) > 0 {
			keep[aws.ToString(group[0].SnapshotId)] = struct{}{}
		}
	}
	return keep
}

func environmentGarbageCollectable(env Environment, now time.Time) bool {
	switch env.State {
	case StateArchived, StateDeleted:
		return true
	case StateError:
		return env.FailedAction == ActionDelete && env.CleanupAfter != nil &&
			!now.Before(env.CleanupAfter.Add(DefaultErrorCleanupTTL))
	case StateProvisioning, StateStarting, StateStopping, StateArchiving, StateDeleting:
		return !now.Before(env.UpdatedAt.Add(DefaultOperationMaxAge + DefaultErrorCleanupTTL))
	default:
		return false
	}
}

func (e *AWSExecutor) managedInstances(ctx context.Context) ([]ec2types.Instance, error) {
	var instances []ec2types.Instance
	var token *string
	for {
		output, err := e.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			NextToken: token, Filters: []ec2types.Filter{
				{Name: aws.String("tag:" + managedTag), Values: []string{"dev-environments"}},
				{Name: aws.String("tag:" + resourceTag), Values: []string{"workspace-instance"}},
				{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
			},
		})
		if err != nil {
			return nil, err
		}
		for _, reservation := range output.Reservations {
			instances = append(instances, reservation.Instances...)
		}
		if output.NextToken == nil || aws.ToString(output.NextToken) == "" {
			return instances, nil
		}
		token = output.NextToken
	}
}

func (e *AWSExecutor) managedWorkspaceVolumes(ctx context.Context) ([]ec2types.Volume, error) {
	var volumes []ec2types.Volume
	var token *string
	for {
		output, err := e.ec2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			NextToken: token, Filters: []ec2types.Filter{
				{Name: aws.String("tag:" + managedTag), Values: []string{"dev-environments"}},
				{Name: aws.String("tag:" + resourceTag), Values: []string{"workspace-volume"}},
			},
		})
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, output.Volumes...)
		if output.NextToken == nil || aws.ToString(output.NextToken) == "" {
			return volumes, nil
		}
		token = output.NextToken
	}
}

func (e *AWSExecutor) managedSnapshots(ctx context.Context) ([]ec2types.Snapshot, error) {
	var snapshots []ec2types.Snapshot
	var token *string
	for {
		output, err := e.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
			NextToken: token, OwnerIds: []string{"self"}, Filters: []ec2types.Filter{
				{Name: aws.String("tag:" + managedTag), Values: []string{"dev-environments"}},
				{Name: aws.String("tag:" + resourceTag), Values: []string{"checkpoint"}},
			},
		})
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, output.Snapshots...)
		if output.NextToken == nil || aws.ToString(output.NextToken) == "" {
			return snapshots, nil
		}
		token = output.NextToken
	}
}

func (e *AWSExecutor) managedNetworkInterfaces(ctx context.Context) ([]ec2types.NetworkInterface, error) {
	var networkInterfaces []ec2types.NetworkInterface
	var token *string
	for {
		output, err := e.ec2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			NextToken: token, Filters: []ec2types.Filter{
				{Name: aws.String("tag:" + managedTag), Values: []string{"dev-environments"}},
				{Name: aws.String("tag:" + resourceTag), Values: []string{"workspace-eni"}},
			},
		})
		if err != nil {
			return nil, err
		}
		networkInterfaces = append(networkInterfaces, output.NetworkInterfaces...)
		if output.NextToken == nil || aws.ToString(output.NextToken) == "" {
			return networkInterfaces, nil
		}
		token = output.NextToken
	}
}

func tagMap(tags []ec2types.Tag) map[string]string {
	values := make(map[string]string, len(tags))
	for _, tag := range tags {
		values[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return values
}

func olderThan(createdAt, now time.Time, grace time.Duration) bool {
	return !createdAt.IsZero() && !now.Before(createdAt.Add(grace))
}

func taggedResourceOld(tags []ec2types.Tag, now time.Time, grace time.Duration) bool {
	values := tagMap(tags)
	for _, key := range []string{"duranta:created-at", expiresTag} {
		value, err := time.Parse(time.RFC3339, values[key])
		if err == nil && olderThan(value, now, grace) {
			return true
		}
	}
	return false
}

func environmentSafetyDeadline(env Environment) time.Time {
	if env.State == StateStopped && env.ArchiveAfter != nil {
		return *env.ArchiveAfter
	}
	if env.State == StateError && env.CleanupAfter != nil {
		return *env.CleanupAfter
	}
	return env.Lease.DueAt()
}

func (e *AWSExecutor) terminateAndDeleteVolume(ctx context.Context, env Environment) error {
	instance, err := e.resolveInstance(ctx, env)
	if err != nil {
		return err
	}
	if instance != nil && instance.State != nil && instance.State.Name != ec2types.InstanceStateNameTerminated {
		instanceID := aws.ToString(instance.InstanceId)
		_, err = e.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{instanceID}})
		if err != nil {
			return err
		}
		if _, err = e.waitInstance(ctx, instanceID, ec2types.InstanceStateNameTerminated); err != nil {
			return err
		}
	}
	volume, err := e.resolveVolume(ctx, env)
	if err != nil || volume == nil {
		return err
	}
	_, err = e.ec2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: volume.VolumeId})
	if isAWSNotFound(err, "InvalidVolume.NotFound") {
		return nil
	}
	return err
}

func (e *AWSExecutor) startRuntime(ctx context.Context, env Environment, result WorkflowResult) (string, error) {
	sourceURL, err := e.sources.DownloadURL(ctx, Identity{PrincipalID: env.OwnerID}, env.Source.BundleKey)
	if err != nil {
		return "", err
	}
	commands, err := startRuntimeCommands(e.config, env, result, sourceURL)
	if err != nil {
		return "", err
	}
	output, err := e.runCommandOutput(ctx, result.InstanceID, commands)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "TLS_CERT_SHA256="); found &&
			len(value) == 64 && isLowerHex(value) {
			return value, nil
		}
	}
	return "", errors.New("workspace readiness did not return a TLS certificate fingerprint")
}

func (e *AWSExecutor) runCommand(ctx context.Context, instanceID string, commands []string) error {
	_, err := e.runCommandOutput(ctx, instanceID, commands)
	return err
}

func (e *AWSExecutor) runCommandOutput(ctx context.Context, instanceID string, commands []string) (string, error) {
	deadline := e.now().Add(e.config.CommandTimeout)
	var sent *ssm.SendCommandOutput
	var err error
	for e.now().Before(deadline) {
		sent, err = e.ssm.SendCommand(ctx, &ssm.SendCommandInput{
			DocumentName: aws.String("AWS-RunShellScript"), InstanceIds: []string{instanceID},
			TimeoutSeconds: aws.Int32(int32(e.config.CommandTimeout.Seconds())),
			Parameters:     map[string][]string{"commands": commands},
		})
		if err == nil && sent.Command != nil && sent.Command.CommandId != nil {
			break
		}
		if err = e.sleep(ctx, 5*time.Second); err != nil {
			return "", err
		}
	}
	if err != nil || sent == nil || sent.Command == nil || sent.Command.CommandId == nil {
		return "", errors.Join(errors.New("send SSM command"), err)
	}
	commandID := aws.ToString(sent.Command.CommandId)
	for e.now().Before(deadline) {
		invocation, getErr := e.ssm.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId: aws.String(commandID), InstanceId: aws.String(instanceID),
		})
		if getErr == nil {
			switch invocation.Status {
			case ssmtypes.CommandInvocationStatusSuccess:
				return aws.ToString(invocation.StandardOutputContent), nil
			case ssmtypes.CommandInvocationStatusCancelled, ssmtypes.CommandInvocationStatusTimedOut,
				ssmtypes.CommandInvocationStatusFailed, ssmtypes.CommandInvocationStatusCancelling:
				return "", fmt.Errorf("SSM command %s: %s", invocation.Status,
					strings.TrimSpace(aws.ToString(invocation.StandardErrorContent)))
			}
		}
		if err = e.sleep(ctx, 5*time.Second); err != nil {
			return "", err
		}
	}
	return "", &RetryableError{Err: errors.New("SSM command timed out")}
}

func (e *AWSExecutor) resolveInstance(ctx context.Context, env Environment) (*ec2types.Instance, error) {
	if env.InstanceID != "" {
		output, err := e.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{env.InstanceID}})
		if err == nil {
			return firstInstance(output), nil
		}
	}
	return e.findInstance(ctx, env.ID)
}

func (e *AWSExecutor) findInstance(ctx context.Context, environmentID string) (*ec2types.Instance, error) {
	output, err := e.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + managedTag), Values: []string{"dev-environments"}},
		{Name: aws.String("tag:" + environmentTag), Values: []string{environmentID}},
		{Name: aws.String("tag:" + resourceTag), Values: []string{"workspace-instance"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	if err != nil {
		return nil, err
	}
	return firstInstance(output), nil
}

func firstInstance(output *ec2.DescribeInstancesOutput) *ec2types.Instance {
	if output == nil {
		return nil
	}
	for _, reservation := range output.Reservations {
		if len(reservation.Instances) > 0 {
			instance := reservation.Instances[0]
			return &instance
		}
	}
	return nil
}

func (e *AWSExecutor) resolveVolume(ctx context.Context, env Environment) (*ec2types.Volume, error) {
	if env.WorkspaceVolumeID != "" {
		output, err := e.ec2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{env.WorkspaceVolumeID}})
		if err == nil && len(output.Volumes) > 0 {
			return &output.Volumes[0], nil
		}
	}
	return e.findWorkspaceVolume(ctx, env.ID)
}

func (e *AWSExecutor) findWorkspaceVolume(ctx context.Context, environmentID string) (*ec2types.Volume, error) {
	output, err := e.ec2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + managedTag), Values: []string{"dev-environments"}},
		{Name: aws.String("tag:" + environmentTag), Values: []string{environmentID}},
		{Name: aws.String("tag:" + resourceTag), Values: []string{"workspace-volume"}},
	}})
	if err != nil || len(output.Volumes) == 0 {
		return nil, err
	}
	return &output.Volumes[0], nil
}

func (e *AWSExecutor) findSnapshot(ctx context.Context, environmentID, operationID string) (*ec2types.Snapshot, error) {
	output, err := e.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"}, Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + managedTag), Values: []string{"dev-environments"}},
			{Name: aws.String("tag:" + environmentTag), Values: []string{environmentID}},
			{Name: aws.String("tag:duranta:operation-id"), Values: []string{operationID}},
			{Name: aws.String("tag:" + resourceTag), Values: []string{"checkpoint"}},
		},
	})
	if err != nil || len(output.Snapshots) == 0 {
		return nil, err
	}
	sort.Slice(output.Snapshots, func(i, j int) bool {
		return aws.ToTime(output.Snapshots[i].StartTime).After(aws.ToTime(output.Snapshots[j].StartTime))
	})
	return &output.Snapshots[0], nil
}

func (e *AWSExecutor) waitInstance(ctx context.Context, id string,
	want ec2types.InstanceStateName) (*ec2types.Instance, error) {
	deadline := e.now().Add(e.config.CommandTimeout)
	for e.now().Before(deadline) {
		output, err := e.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err == nil {
			instance := firstInstance(output)
			if instance != nil && instance.State != nil && instance.State.Name == want {
				return instance, nil
			}
		}
		if err = e.sleep(ctx, 5*time.Second); err != nil {
			return nil, err
		}
	}
	return nil, &RetryableError{Err: fmt.Errorf("instance %s did not reach %s", id, want)}
}

func (e *AWSExecutor) waitVolume(ctx context.Context, id string,
	want ec2types.VolumeState) (*ec2types.Volume, error) {
	deadline := e.now().Add(e.config.CommandTimeout)
	for e.now().Before(deadline) {
		output, err := e.ec2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
		if err == nil && len(output.Volumes) == 1 && output.Volumes[0].State == want {
			return &output.Volumes[0], nil
		}
		if err = e.sleep(ctx, 5*time.Second); err != nil {
			return nil, err
		}
	}
	return nil, &RetryableError{Err: fmt.Errorf("volume %s did not reach %s", id, want)}
}

func (e *AWSExecutor) waitSnapshot(ctx context.Context, id string,
	want ec2types.SnapshotState) (*ec2types.Snapshot, error) {
	deadline := e.now().Add(e.config.CommandTimeout)
	for e.now().Before(deadline) {
		output, err := e.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{id}})
		if err == nil && len(output.Snapshots) == 1 && output.Snapshots[0].State == want {
			return &output.Snapshots[0], nil
		}
		if err = e.sleep(ctx, 10*time.Second); err != nil {
			return nil, err
		}
	}
	return nil, &RetryableError{Err: fmt.Errorf("snapshot %s did not reach %s", id, want)}
}

func startRuntimeCommands(config AWSExecutorConfig, env Environment, result WorkflowResult, sourceURL string) ([]string, error) {
	if !strings.HasPrefix(sourceURL, "https://") {
		return nil, errors.New("source download URL must use HTTPS")
	}
	compose := remoteCompose(config, env, result)
	composeB64 := base64.StdEncoding.EncodeToString([]byte(compose))
	dynamicB64 := base64.StdEncoding.EncodeToString([]byte(previewProxyConfig(result.Host)))
	watchdogB64 := base64.StdEncoding.EncodeToString([]byte(watchdogScript()))
	unitB64 := base64.StdEncoding.EncodeToString([]byte(watchdogUnit()))
	extractorB64 := base64.StdEncoding.EncodeToString([]byte(sourceExtractorScript))
	ref := env.Source.Ref
	if ref == "" {
		ref = "HEAD"
	}
	setOrigin := ":"
	if !strings.HasPrefix(env.Source.Repository, "local:") {
		setOrigin = "git -C /workspace/repo.next remote set-url origin " + shellQuote(env.Source.Repository)
	}
	return []string{
		"set -euo pipefail",
		"install -d -m 0755 /workspace /workspace/runtime /workspace/docker",
		"volume_id=" + shellQuote(strings.ReplaceAll(result.WorkspaceVolumeID, "-", "")),
		"device=''",
		"for attempt in $(seq 1 60); do candidate=/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${volume_id}; [ -e \"$candidate\" ] && device=$(readlink -f \"$candidate\") && break; [ -e /dev/xvdf ] && device=/dev/xvdf && break; sleep 2; done",
		"[ -n \"$device\" ]",
		"blkid \"$device\" >/dev/null 2>&1 || mkfs.ext4 -F \"$device\"",
		"uuid=$(blkid -s UUID -o value \"$device\"); grep -q \"UUID=$uuid /workspace \" /etc/fstab || printf 'UUID=%s /workspace ext4 defaults,nofail 0 2\\n' \"$uuid\" >> /etc/fstab",
		"mountpoint -q /workspace || mount \"$device\" /workspace",
		"install -d -m 0755 /workspace/runtime /workspace/runtime/tls /workspace/docker",
		"printf '%s' '" + composeB64 + "' | base64 -d > /workspace/runtime/compose.remote.yml",
		"printf '%s' '" + dynamicB64 + "' | base64 -d > /workspace/runtime/traefik.dynamic.yml",
		"if [ ! -s /workspace/runtime/tls/cert.pem ] || [ ! -s /workspace/runtime/tls/key.pem ] || ! openssl x509 -in /workspace/runtime/tls/cert.pem -checkend 86400 -noout >/dev/null 2>&1 || ! openssl x509 -in /workspace/runtime/tls/cert.pem -checkhost " + shellQuote(result.Host) + " -noout >/dev/null 2>&1; then openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 30 -subj /CN=" + shellQuote(result.Host) + " -addext subjectAltName=DNS:" + shellQuote(result.Host) + " -keyout /workspace/runtime/tls/key.pem -out /workspace/runtime/tls/cert.pem; chmod 0600 /workspace/runtime/tls/key.pem; fi",
		"systemctl stop docker",
		"if [ ! -e /workspace/docker/.golden-seeded ]; then cp -a /var/lib/docker/. /workspace/docker/; touch /workspace/docker/.golden-seeded; fi",
		"python3 -c 'import json,pathlib; p=pathlib.Path(\"/etc/docker/daemon.json\"); d=json.loads(p.read_text()) if p.exists() and p.read_text().strip() else {}; d[\"data-root\"]=\"/workspace/docker\"; p.write_text(json.dumps(d))'",
		"install -d -m 0755 /etc/systemd/system/docker.service.d; printf '%s\n' '[Unit]' 'RequiresMountsFor=/workspace' > /etc/systemd/system/docker.service.d/workspace.conf",
		"printf '%s' '" + watchdogB64 + "' | base64 -d > /usr/local/sbin/duranta-dev-agent; chmod 0755 /usr/local/sbin/duranta-dev-agent",
		"printf '%s' '" + unitB64 + "' | base64 -d > /etc/systemd/system/duranta-dev-agent.service",
		"printf '%s' '" + extractorB64 + "' | base64 -d > /workspace/runtime/source-extract.py; chmod 0700 /workspace/runtime/source-extract.py",
		"systemctl disable --now duranta-bootstrap-watchdog.service 2>/dev/null || true; systemctl daemon-reload; systemctl enable --now docker duranta-dev-agent.service",
		"if [ ! -d /workspace/repo/.git ]; then rm -rf /workspace/repo.next /workspace/runtime/source; curl --fail --silent --show-error --location --output /workspace/runtime/source.tgz " +
			shellQuote(sourceURL) +
			"; python3 /workspace/runtime/source-extract.py extract /workspace/runtime/source.tgz /workspace/runtime/source 17179869184; GIT_LFS_SKIP_SMUDGE=1 git clone /workspace/runtime/source/source.bundle /workspace/repo.next; GIT_LFS_SKIP_SMUDGE=1 git -C /workspace/repo.next checkout --detach " + shellQuote(ref) + "; python3 /workspace/runtime/source-extract.py apply /workspace/runtime/source/worktree /workspace/runtime/source/deleted /workspace/repo.next; " + setOrigin + "; mv /workspace/repo.next /workspace/repo; fi",
		"id ssm-user >/dev/null 2>&1 || useradd --create-home --shell /bin/bash ssm-user; usermod -aG docker ssm-user; chown -R ssm-user:ssm-user /workspace/repo; chmod 0775 /workspace/runtime",
		"cd /workspace/repo",
		composeUpCommand(env),
		"if [ -f /workspace/runtime/frontend.pid ] && kill -0 $(cat /workspace/runtime/frontend.pid) 2>/dev/null; then :; else cd /workspace/repo/frontend; npm ci --include=dev; cd website; nohup env PORT=5173 FRONTEND_PUBLIC_ORIGIN=" + shellQuote("https://"+result.Host) +
			" VITE_PUBLIC_API_URL=" + shellQuote("https://"+result.Host) +
			" VITE_LOGTO_ENDPOINT=https://logto.be-dev.getduranta.com VITE_LOGTO_APP_ID=" + shellQuote(result.LogtoAppID) +
			" VITE_LOGTO_API_RESOURCE=https://api.getduranta.com" +
			" ../node_modules/.bin/react-router dev --mode live --host 0.0.0.0 --port 5173 >/workspace/runtime/frontend.log 2>&1 </dev/null & echo $! >/workspace/runtime/frontend.pid; fi",
		"for attempt in $(seq 1 120); do curl -ksS --resolve " + shellQuote(result.Host+":"+strconv.Itoa(config.WorkspacePort)+":127.0.0.1") +
			" " + shellQuote("https://"+result.Host+":"+strconv.Itoa(config.WorkspacePort)+"/healthcheck") +
			" >/dev/null && break; [ \"$attempt\" -eq 120 ] && exit 1; sleep 2; done",
		"cert=$(mktemp); openssl s_client -connect 127.0.0.1:" + strconv.Itoa(config.WorkspacePort) + " -servername " +
			shellQuote(result.Host) + " -showcerts </dev/null 2>/dev/null | openssl x509 -out \"$cert\"",
		"openssl x509 -in \"$cert\" -checkhost " + shellQuote(result.Host) + " -noout",
		"openssl x509 -in \"$cert\" -checkend 60 -noout",
		"printf 'TLS_CERT_SHA256='; openssl x509 -in \"$cert\" -outform der | sha256sum | awk '{print $1}'",
	}, nil
}

func stopRuntimeCommands() []string {
	return []string{
		"set -euo pipefail",
		"if [ -f /workspace/runtime/frontend.pid ]; then pid=$(cat /workspace/runtime/frontend.pid); kill -TERM \"$pid\" 2>/dev/null || true; rm -f /workspace/runtime/frontend.pid; fi",
		"if [ -d /workspace/repo ]; then cd /workspace/repo; docker compose -p duranta-dev -f compose.yml -f /workspace/runtime/compose.remote.yml --profile backend --profile cvml stop preview_proxy cvml backend db_init redis db blobs || true; fi",
		"sync",
	}
}

func remoteCompose(config AWSExecutorConfig, env Environment, result WorkflowResult) string {
	cvmlEndpoint := config.SharedCVMLEndpoint
	if env.Profile == ProfileGPUCVML {
		cvmlEndpoint = "http://cvml:8082"
	}
	return fmt.Sprintf(`services:
  backend:
    environment:
      BACKEND_EXTRA_ARGS: "--configs=local,dev-stack --otel=false"
      DRNT_WEB_PUBLICURL: "https://%s/a/"
      DRNT_WEB_EXTERNALLYVISIBLESERVERNAME: "%s"
      DRNT_WEB_PROXYFROMBACKENDTO: "http://host.docker.internal:5173/"
      DRNT_SERVICES_CVML_HTTPENDPOINT: "%s"
      DRNT_DATA_S3_CUSTOMSETTINGS: "true"
      DRNT_DATA_S3_ENDPOINT: "http://blobs:9000"
      DRNT_WEB_CDNURLS: "https://%s"
    extra_hosts:
      - "host.docker.internal:host-gateway"
  preview_proxy:
    image: "traefik:v3.7.10"
    command:
      - "--entrypoints.preview.address=:8443"
      - "--providers.file.filename=/runtime/traefik.dynamic.yml"
    ports:
      - "0.0.0.0:%d:8443"
    volumes:
      - "/workspace/runtime:/runtime:ro"
    networks: [duranta]
    depends_on: [backend]
  cvml:
    gpus: all
`, result.Host, result.Host, cvmlEndpoint, result.Host, config.WorkspacePort)
}

func previewProxyConfig(host string) string {
	return fmt.Sprintf(`tls:
  certificates:
    - certFile: /runtime/tls/cert.pem
      keyFile: /runtime/tls/key.pem
http:
  routers:
    workspace:
      rule: "PathPrefix(\"/\")"
      entryPoints: [preview]
      tls: {}
      service: backend
  services:
    backend:
      loadBalancer:
        servers:
          - url: "https://backend:8443"
        serversTransport: backend
  serversTransports:
    backend:
      serverName: "%s"
      insecureSkipVerify: true
`, host)
}

func composeUpCommand(env Environment) string {
	services := "db redis blobs db_init backend preview_proxy"
	profiles := "--profile backend"
	if env.Profile == ProfileGPUCVML {
		profiles += " --profile cvml"
		services += " cvml"
	}
	return "docker compose -p duranta-dev -f compose.yml -f /workspace/runtime/compose.remote.yml " +
		profiles + " up -d --wait " + services
}

func watchdogScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
state_dir=/workspace/runtime/watchdog
install -d -m 0700 "$state_dir"
boot_id=$(cat /proc/sys/kernel/random/boot_id)
if [ ! -s "$state_dir/boot-id" ] || [ "$(cat "$state_dir/boot-id" 2>/dev/null || true)" != "$boot_id" ]; then
  now=$(date -u +%s)
  printf '%s\n' "$boot_id" > "$state_dir/boot-id"
  printf '%s\n' "$((now + 14400))" > "$state_dir/missing-deadline"
  printf '%s\n' "$((now + 86400))" > "$state_dir/hard-deadline"
  rm -f "$state_dir/last-valid-deadline"
fi
while true; do
  now=$(date -u +%s)

  missing_deadline=$(cat "$state_dir/missing-deadline" 2>/dev/null || printf 0)
  if ! [[ "$missing_deadline" =~ ^[0-9]+$ ]] || [ "$missing_deadline" -le 0 ]; then
    missing_deadline=$((now + 3600))
    printf '%s\n' "$missing_deadline" > "$state_dir/missing-deadline"
  fi
  hard_deadline=$(cat "$state_dir/hard-deadline" 2>/dev/null || printf 0)
  if ! [[ "$hard_deadline" =~ ^[0-9]+$ ]] || [ "$hard_deadline" -le 0 ]; then
    hard_deadline=$((now + 14400))
    printf '%s\n' "$hard_deadline" > "$state_dir/hard-deadline"
  fi
  token=$(curl -fsS -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' http://169.254.169.254/latest/api/token || true)
  expires=''
  if [ -n "$token" ]; then
    expires=$(curl -fsS -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/tags/instance/duranta:expires-at || true)
  fi
  deadline=0
  if [ -n "$expires" ]; then
    deadline=$(date -u -d "$expires" +%s 2>/dev/null || printf 0)
    if [ "$deadline" -gt 0 ]; then printf '%s\n' "$deadline" > "$state_dir/last-valid-deadline"; fi
  fi
  if [ "$deadline" -le 0 ] && [ -s "$state_dir/last-valid-deadline" ]; then
    deadline=$(cat "$state_dir/last-valid-deadline" 2>/dev/null || printf 0)
  fi
  if ! [[ "$deadline" =~ ^[0-9]+$ ]] || [ "$deadline" -le 0 ]; then
    deadline=$missing_deadline
  fi
  if [ "$deadline" -gt "$hard_deadline" ]; then deadline=$hard_deadline; fi
  if [ "$now" -ge "$deadline" ]; then
    if [ -f /workspace/runtime/frontend.pid ]; then kill -TERM "$(cat /workspace/runtime/frontend.pid)" 2>/dev/null || true; fi
    if [ -d /workspace/repo ]; then cd /workspace/repo; docker compose -p duranta-dev -f compose.yml -f /workspace/runtime/compose.remote.yml --profile backend --profile cvml stop preview_proxy cvml backend db_init redis db blobs || true; fi
    sync
    systemctl poweroff
    exit 0
  fi
  sleep 60
done
`
}

func watchdogUnit() string {
	return `[Unit]
Description=Duranta development workspace lease watchdog
After=network-online.target
Wants=network-online.target
RequiresMountsFor=/workspace

[Service]
Type=simple
ExecStart=/usr/local/sbin/duranta-dev-agent
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`
}

func resourceTags(env Environment, operationID, resource string) []ec2types.Tag {
	tags := []ec2types.Tag{
		{Key: aws.String(managedTag), Value: aws.String("dev-environments")},
		{Key: aws.String(environmentTag), Value: aws.String(env.ID)},
		{Key: aws.String(ownerTag), Value: aws.String(env.OwnerID)},
		{Key: aws.String(expiresTag), Value: aws.String(env.Lease.DueAt().UTC().Format(time.RFC3339))},
		{Key: aws.String(resourceTag), Value: aws.String(resource)},
		{Key: aws.String("duranta:operation-id"), Value: aws.String(operationID)},
		{Key: aws.String("duranta:display-name"), Value: aws.String(env.Name)},
		{Key: aws.String("Name"), Value: aws.String("dev-" + env.Name)},
	}
	if env.OwnerEmail != "" {
		tags = append(tags, ec2types.Tag{Key: aws.String("duranta:owner-email"), Value: aws.String(env.OwnerEmail)})
	}
	if !env.CreatedAt.IsZero() {
		tags = append(tags, ec2types.Tag{
			Key: aws.String("duranta:created-at"), Value: aws.String(env.CreatedAt.UTC().Format(time.RFC3339)),
		})
	}
	return tags
}

func snapshotTags(env Environment, operation Operation, checkpointName string) []ec2types.Tag {
	tags := resourceTags(env, operation.ID, "checkpoint")
	return append(tags,
		ec2types.Tag{Key: aws.String("duranta:checkpoint-name"), Value: aws.String(checkpointName)},
		ec2types.Tag{Key: aws.String("duranta:checkpoint-pinned"), Value: aws.String(strconv.FormatBool(operation.CheckpointPinned))},
	)
}

func launchTagSpecifications(env Environment, operationID string) []ec2types.TagSpecification {
	return []ec2types.TagSpecification{
		{ResourceType: ec2types.ResourceTypeInstance, Tags: resourceTags(env, operationID, "workspace-instance")},
		{ResourceType: ec2types.ResourceTypeVolume, Tags: resourceTags(env, operationID, "instance-root")},
		{ResourceType: ec2types.ResourceTypeNetworkInterface, Tags: resourceTags(env, operationID, "workspace-eni")},
	}
}

func primaryNetworkInterface(subnetID, securityGroupID string) ec2types.InstanceNetworkInterfaceSpecification {
	return ec2types.InstanceNetworkInterfaceSpecification{
		DeviceIndex: aws.Int32(0), SubnetId: aws.String(subnetID), Groups: []string{securityGroupID},
		AssociatePublicIpAddress: aws.Bool(false), Ipv6AddressCount: aws.Int32(0), DeleteOnTermination: aws.Bool(true),
	}
}

func snapshotDescription(env Environment, checkpointName string) string {
	return "Duranta dev checkpoint " + checkpointName + " (" + env.ID + ")"
}

func reusableArchiveSnapshot(env Environment) string {
	if env.CurrentCheckpoint == "" {
		return env.CurrentSnapshotID
	}
	return ""
}

func runtimeResult(env Environment) WorkflowResult {
	return WorkflowResult{
		InstanceID: env.InstanceID, InstanceRoleARN: env.InstanceRoleARN,
		WorkspaceVolumeID: env.WorkspaceVolumeID, Host: env.Host,
		PrivateUpstream: env.PrivateUpstream, TLSCertSHA256: env.TLSCertSHA256, LogtoAppID: env.LogtoAppID,
	}
}

func stableIndex(value string, size int) int {
	var total uint64
	for _, character := range []byte(value) {
		total = total*131 + uint64(character)
	}
	return int(total % uint64(size))
}

func previewHost(env Environment, domain string) string {
	owner := sanitizeDNSPart(strings.Split(env.OwnerEmail, "@")[0])
	if owner == "" {
		owner = "owner"
	}
	name := sanitizeDNSPart(env.Name)
	if name == "" {
		name = "workspace"
	}
	suffix := strings.TrimPrefix(env.ID, "env-")
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	prefix := owner + "-" + name
	maxPrefix := 63 - len(suffix) - 1
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-")
	}
	label := prefix + "-" + suffix
	return label + "." + strings.TrimPrefix(domain, ".")
}

func sanitizeDNSPart(value string) string {
	var result strings.Builder
	lastHyphen := false
	for _, character := range strings.ToLower(value) {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
		if valid {
			result.WriteRune(character)
			lastHyphen = false
		} else if result.Len() > 0 && !lastHyphen {
			result.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func operationToken(environmentID, resource string) string {
	value := strings.ReplaceAll(environmentID+"-"+resource, "_", "-")
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func isLowerHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isAWSNotFound(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}
