package devenv

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GetDuranta/tools/internal/devaccess"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	recordEnvironment = "environment"
	recordOperation   = "operation"
	recordCheckpoint  = "checkpoint"
	recordIdempotency = "idempotency"
	recordLeaseOutbox = "lease_outbox"
	recordQuota       = "quota"
)

type DynamoDBAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput,
		...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

func (s *DynamoStore) PutBootstrap(ctx context.Context, grant devaccess.BootstrapGrant) error {
	item := map[string]types.AttributeValue{
		"pk": avs("CODE#" + grant.CodeHash), "sk": avs("BOOTSTRAP"),
		"workspace_id": avs(grant.WorkspaceID), "host": avs(grant.Host), "subject": avs(grant.Subject),
		"principals":  &types.AttributeValueMemberSS{Value: grant.Principals},
		"acl_version": avn(grant.ACLVersion), "return_path": avs(grant.ReturnPath),
		"expires_at": avn(grant.ExpiresAt.Unix()), "ttl": avn(grant.ExpiresAt.Unix()),
	}
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table), Item: item,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		return devaccess.ErrConflict
	}
	return err
}

type DynamoStore struct {
	client     DynamoDBAPI
	table      string
	ownerIndex string
	dueIndex   string
	now        func() time.Time
}

func NewDynamoStore(client DynamoDBAPI, table, ownerIndex, dueIndex string) *DynamoStore {
	return &DynamoStore{
		client: client, table: table, ownerIndex: ownerIndex, dueIndex: dueIndex,
		now: func() time.Time { return time.Now().UTC() },
	}
}

type environmentItem struct {
	PK          string      `dynamodbav:"pk"`
	SK          string      `dynamodbav:"sk"`
	Kind        string      `dynamodbav:"kind"`
	Environment Environment `dynamodbav:"environment"`
	OwnerPK     string      `dynamodbav:"owner_pk"`
	OwnerSK     string      `dynamodbav:"owner_sk"`
	DuePK       string      `dynamodbav:"due_pk,omitempty"`
	DueAt       string      `dynamodbav:"due_at,omitempty"`
}

type operationItem struct {
	PK        string    `dynamodbav:"pk"`
	SK        string    `dynamodbav:"sk"`
	Kind      string    `dynamodbav:"kind"`
	Operation Operation `dynamodbav:"operation"`
}

type checkpointItem struct {
	PK         string     `dynamodbav:"pk"`
	SK         string     `dynamodbav:"sk"`
	Kind       string     `dynamodbav:"kind"`
	Checkpoint Checkpoint `dynamodbav:"checkpoint"`
	OwnerPK    string     `dynamodbav:"owner_pk"`
	OwnerSK    string     `dynamodbav:"owner_sk"`
	DuePK      string     `dynamodbav:"due_pk,omitempty"`
	DueAt      string     `dynamodbav:"due_at,omitempty"`
}

type idempotencyItem struct {
	PK            string `dynamodbav:"pk"`
	SK            string `dynamodbav:"sk"`
	Kind          string `dynamodbav:"kind"`
	RequestHash   string `dynamodbav:"request_hash"`
	EnvironmentID string `dynamodbav:"environment_id,omitempty"`
	OperationID   string `dynamodbav:"operation_id"`
	CheckpointID  string `dynamodbav:"checkpoint_id,omitempty"`
	ExpiresAt     int64  `dynamodbav:"expires_at"`
	TTL           int64  `dynamodbav:"ttl"`
}

type leaseOutboxItem struct {
	PK           string      `dynamodbav:"pk"`
	SK           string      `dynamodbav:"sk"`
	Kind         string      `dynamodbav:"kind"`
	Environment  Environment `dynamodbav:"environment"`
	LeaseVersion int64       `dynamodbav:"lease_version"`
}

type quotaItem struct {
	PK                string `dynamodbav:"pk"`
	SK                string `dynamodbav:"sk"`
	Kind              string `dynamodbav:"kind"`
	Workspaces        int    `dynamodbav:"workspaces"`
	Running           int    `dynamodbav:"running"`
	GPURunning        int    `dynamodbav:"gpu_running"`
	Checkpoints       int    `dynamodbav:"checkpoints"`
	PinnedCheckpoints int    `dynamodbav:"pinned_checkpoints"`
	Version           int64  `dynamodbav:"version"`
}

func (s *DynamoStore) ReplayMutation(ctx context.Context, idem Idempotency) (MutationResult, bool, error) {
	item, found, err := s.getIdempotency(ctx, idem)
	if err != nil || !found {
		return MutationResult{}, false, err
	}
	if item.RequestHash != idem.RequestHash {
		return MutationResult{}, false, ErrIdempotency
	}
	op, err := s.GetOperation(ctx, item.OperationID)
	if err != nil {
		return MutationResult{}, false, err
	}
	result := MutationResult{Operation: op, Replayed: true}
	if item.EnvironmentID != "" {
		result.Environment, err = s.GetEnvironment(ctx, item.EnvironmentID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return MutationResult{}, false, err
		}
	}
	return result, true, nil
}

func (s *DynamoStore) GetEnvironment(ctx context.Context, id string) (Environment, error) {
	output, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: key("ENV#"+id, "META"), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Environment{}, err
	}
	if len(output.Item) == 0 {
		return Environment{}, ErrNotFound
	}
	var item environmentItem
	if err = attributevalue.UnmarshalMap(output.Item, &item); err != nil {
		return Environment{}, err
	}
	return item.Environment, nil
}

func (s *DynamoStore) ListEnvironments(ctx context.Context, ownerID string) ([]Environment, error) {
	output, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName: aws.String(s.table), IndexName: aws.String(s.ownerIndex),
		KeyConditionExpression: aws.String("owner_pk = :owner AND begins_with(owner_sk, :kind)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner": avs("OWNER#" + ownerID), ":kind": avs("ENV#"),
		},
	})
	if err != nil {
		return nil, err
	}
	environments := make([]Environment, 0, len(output.Items))
	for _, raw := range output.Items {
		var item environmentItem
		if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
			return nil, err
		}
		environments = append(environments, item.Environment)
	}
	return environments, nil
}

func (s *DynamoStore) ListAllEnvironments(ctx context.Context) ([]Environment, error) {
	var environments []Environment
	var start map[string]types.AttributeValue
	for {
		output, err := s.client.Scan(ctx, &dynamodb.ScanInput{
			TableName: aws.String(s.table), ExclusiveStartKey: start, ConsistentRead: aws.Bool(true),
			FilterExpression:          aws.String("#kind = :kind"),
			ExpressionAttributeNames:  map[string]string{"#kind": "kind"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":kind": avs(recordEnvironment)},
		})
		if err != nil {
			return nil, err
		}
		for _, raw := range output.Items {
			var item environmentItem
			if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
				return nil, err
			}
			environments = append(environments, item.Environment)
		}
		if len(output.LastEvaluatedKey) == 0 {
			return environments, nil
		}
		start = output.LastEvaluatedKey
	}
}

func (s *DynamoStore) GetOperation(ctx context.Context, id string) (Operation, error) {
	output, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: key("OP#"+id, "META"), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Operation{}, err
	}
	if len(output.Item) == 0 {
		return Operation{}, ErrNotFound
	}
	var item operationItem
	if err = attributevalue.UnmarshalMap(output.Item, &item); err != nil {
		return Operation{}, err
	}
	return item.Operation, nil
}

func (s *DynamoStore) ClaimOperation(ctx context.Context, id, token string, now time.Time,
	claimFor time.Duration, maxAttempts int, maxAge time.Duration) (Operation, bool, bool, error) {
	op, err := s.GetOperation(ctx, id)
	if err != nil {
		return Operation{}, false, false, err
	}
	if op.Status == OperationSucceeded || op.Status == OperationFailed {
		return op, false, false, nil
	}
	if op.Status == OperationRunning && op.ClaimUntil != nil && now.Before(*op.ClaimUntil) {
		return op, false, false, nil
	}
	exhausted := op.AttemptCount >= maxAttempts || !now.Before(op.RequestedAt.Add(maxAge))
	expectedAttempts := op.AttemptCount
	op.Status = OperationRunning
	op.ClaimToken = token
	claimUntil := now.Add(claimFor)
	op.ClaimUntil = &claimUntil
	op.UpdatedAt = now
	if op.FirstAttemptAt == nil {
		first := now
		op.FirstAttemptAt = &first
	}
	op.AttemptCount++
	raw, err := attributevalue.MarshalMap(makeOperationItem(op))
	if err != nil {
		return Operation{}, false, false, err
	}
	condition := "attribute_exists(pk) AND (operation.#status = :queued OR " +
		"(operation.#status = :running AND (attribute_not_exists(operation.#claim_until) OR operation.#claim_until <= :now))) AND " +
		"(attribute_not_exists(operation.#attempts) OR operation.#attempts = :attempts)"
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table), Item: raw, ConditionExpression: aws.String(condition),
		ExpressionAttributeNames: map[string]string{
			"#status": "Status", "#claim_until": "ClaimUntil", "#attempts": "AttemptCount",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":queued": avs(string(OperationQueued)), ":running": avs(string(OperationRunning)),
			":now": avs(now.UTC().Format(time.RFC3339Nano)), ":attempts": avn(int64(expectedAttempts)),
		},
	})
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		return op, false, false, nil
	}
	return op, err == nil, exhausted, err
}

func (s *DynamoStore) ReleaseOperationClaim(ctx context.Context, id, token string, now time.Time) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table), Key: key("OP#"+id, "META"),
		UpdateExpression: aws.String("SET operation.#status = :queued, operation.#updated = :updated " +
			"REMOVE operation.#claim, operation.#claim_until"),
		ConditionExpression: aws.String("operation.#status = :running AND operation.#claim = :claim"),
		ExpressionAttributeNames: map[string]string{
			"#status": "Status", "#updated": "UpdatedAt", "#claim": "ClaimToken", "#claim_until": "ClaimUntil",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":queued": avs(string(OperationQueued)), ":running": avs(string(OperationRunning)),
			":claim": avs(token), ":updated": avs(now.UTC().Format(time.RFC3339Nano)),
		},
	})
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		return ErrConflict
	}
	return err
}

func (s *DynamoStore) GetCheckpoint(ctx context.Context, id string) (Checkpoint, error) {
	output, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: key("CHECKPOINT#"+id, "META"), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Checkpoint{}, err
	}
	if len(output.Item) == 0 {
		return Checkpoint{}, ErrNotFound
	}
	var item checkpointItem
	if err = attributevalue.UnmarshalMap(output.Item, &item); err != nil {
		return Checkpoint{}, err
	}
	return item.Checkpoint, nil
}

func (s *DynamoStore) ListCheckpoints(ctx context.Context, ownerID string) ([]Checkpoint, error) {
	output, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName: aws.String(s.table), IndexName: aws.String(s.ownerIndex),
		KeyConditionExpression: aws.String("owner_pk = :owner AND begins_with(owner_sk, :kind)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner": avs("OWNER#" + ownerID), ":kind": avs("CHECKPOINT#"),
		},
	})
	if err != nil {
		return nil, err
	}
	checkpoints := make([]Checkpoint, 0, len(output.Items))
	for _, raw := range output.Items {
		var item checkpointItem
		if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
			return nil, err
		}
		checkpoints = append(checkpoints, item.Checkpoint)
	}
	return checkpoints, nil
}

func (s *DynamoStore) ListAllCheckpoints(ctx context.Context) ([]Checkpoint, error) {
	var checkpoints []Checkpoint
	var start map[string]types.AttributeValue
	for {
		output, err := s.client.Scan(ctx, &dynamodb.ScanInput{
			TableName: aws.String(s.table), ExclusiveStartKey: start, ConsistentRead: aws.Bool(true),
			FilterExpression:          aws.String("#kind = :kind"),
			ExpressionAttributeNames:  map[string]string{"#kind": "kind"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":kind": avs(recordCheckpoint)},
		})
		if err != nil {
			return nil, err
		}
		for _, raw := range output.Items {
			var item checkpointItem
			if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
				return nil, err
			}
			checkpoints = append(checkpoints, item.Checkpoint)
		}
		if len(output.LastEvaluatedKey) == 0 {
			return checkpoints, nil
		}
		start = output.LastEvaluatedKey
	}
}

func (s *DynamoStore) Create(ctx context.Context, mutation CreateMutation,
	limits QuotaLimits) (MutationResult, error) {
	if replayed, found, err := s.ReplayMutation(ctx, mutation.Idempotency); err != nil || found {
		return replayed, err
	}
	envRaw, err := attributevalue.MarshalMap(makeEnvironmentItem(mutation.Environment))
	if err != nil {
		return MutationResult{}, err
	}
	opRaw, err := attributevalue.MarshalMap(makeOperationItem(mutation.Operation))
	if err != nil {
		return MutationResult{}, err
	}
	idemRaw, err := attributevalue.MarshalMap(makeIdempotencyItem(mutation.Idempotency, mutation.Operation))
	if err != nil {
		return MutationResult{}, err
	}
	outboxRaw, err := attributevalue.MarshalMap(makeLeaseOutboxItem(mutation.Environment))
	if err != nil {
		return MutationResult{}, err
	}
	items := []types.TransactWriteItem{
		putTransaction(envRaw, "attribute_not_exists(pk)", nil),
		putTransaction(opRaw, "attribute_not_exists(pk)", nil),
		putIdempotencyTransaction(idemRaw, s.now()),
		putTransaction(outboxRaw, "", nil),
	}
	if mutation.SourceCheckpoint != nil {
		items = append(items, checkpointReferenceTransaction(*mutation.SourceCheckpoint, 1))
	}
	quotaIndexes := []int{len(items), len(items) + 1}
	items = append(items,
		ownerQuotaTransaction(mutation.Environment.OwnerID, 1, 0, 0, limits),
		globalQuotaTransaction(1, 1, boolInt(mutation.Environment.Profile == ProfileGPUCVML), 0, 0, limits))
	err = s.transact(ctx, mutation.Operation.ID, items)
	if err != nil {
		if replayed, found, replayErr := s.ReplayMutation(ctx, mutation.Idempotency); replayErr != nil || found {
			return replayed, replayErr
		}
		return MutationResult{}, classifyTransactionError(err, quotaIndexes...)
	}
	return MutationResult{Environment: mutation.Environment, Operation: mutation.Operation}, nil
}

func (s *DynamoStore) Begin(ctx context.Context, mutation BeginMutation,
	limits QuotaLimits) (MutationResult, error) {
	if replayed, found, err := s.ReplayMutation(ctx, mutation.Idempotency); err != nil || found {
		return replayed, err
	}
	env, err := s.GetEnvironment(ctx, mutation.EnvironmentID)
	if err != nil {
		return MutationResult{}, err
	}
	if env.OwnerID != mutation.OwnerID || env.State != mutation.ExpectedState || env.Version != mutation.ExpectedVersion {
		return MutationResult{}, ErrConflict
	}
	profile := mutation.CurrentProfile
	if mutation.Profile != nil {
		profile = *mutation.Profile
	}
	workspaceDelta, runningDelta, gpuDelta := 0, 0, 0
	checkpointDelta, pinnedCheckpointDelta := 0, 0
	quotaLimits := limits
	if mutation.NextState == StateStarting {
		if !env.WorkspaceSlot {
			workspaceDelta = 1
			env.WorkspaceSlot = true
		}
		if !env.RunningSlot {
			runningDelta = 1
			env.RunningSlot = true
		}
		if profile == ProfileGPUCVML && !env.GPURunningSlot {
			gpuDelta = 1
			env.GPURunningSlot = true
		}
	}
	if mutation.Operation.Action == ActionArchive {
		if env.CheckpointQuotaReserved {
			mutation.Operation.CheckpointName = env.CheckpointQuotaName
			mutation.Operation.CheckpointPinned = env.PinnedCheckpointQuotaReserved
		} else {
			checkpointDelta = 1
			pinnedCheckpointDelta = boolInt(mutation.Operation.CheckpointPinned)
			env.CheckpointQuotaReserved = true
			env.PinnedCheckpointQuotaReserved = mutation.Operation.CheckpointPinned
			env.CheckpointQuotaName = checkpointForArchive(
				mutation.Operation, env, "", mutation.Operation.RequestedAt).Name
			mutation.Operation.CheckpointName = env.CheckpointQuotaName
			if mutation.Operation.ActorPrincipal == "system:lease" {
				quotaLimits.MaxOwnerCheckpoints += limits.MaxOwnerWorkspaces
				quotaLimits.MaxCheckpoints += limits.MaxWorkspaces
			}
		}
	}
	env.State = mutation.NextState
	env.Profile = profile
	env.ActiveOperationID = mutation.Operation.ID
	env.Version++
	env.UpdatedAt = mutation.Operation.RequestedAt
	if mutation.Lease != nil {
		env.Lease = *mutation.Lease
	}
	envRaw, err := attributevalue.MarshalMap(makeEnvironmentItem(env))
	if err != nil {
		return MutationResult{}, err
	}
	opRaw, err := attributevalue.MarshalMap(makeOperationItem(mutation.Operation))
	if err != nil {
		return MutationResult{}, err
	}
	idemRaw, err := attributevalue.MarshalMap(makeIdempotencyItem(mutation.Idempotency, mutation.Operation))
	if err != nil {
		return MutationResult{}, err
	}
	items := []types.TransactWriteItem{
		putTransaction(envRaw,
			"environment.#owner = :owner AND environment.#state = :state AND environment.#version = :version AND "+
				"(attribute_not_exists(environment.#active) OR environment.#active = :empty)",
			&transactionCondition{
				Names: map[string]string{"#owner": "OwnerID", "#state": "State", "#version": "Version", "#active": "ActiveOperationID"},
				Values: map[string]types.AttributeValue{
					":owner": avs(mutation.OwnerID), ":state": avs(string(mutation.ExpectedState)),
					":version": avn(mutation.ExpectedVersion), ":empty": avs(""),
				},
			}),
		putTransaction(opRaw, "attribute_not_exists(pk)", nil),
		putIdempotencyTransaction(idemRaw, s.now()),
	}
	if env.Host != "" && (mutation.NextState == StateStopping || mutation.NextState == StateArchiving ||
		mutation.NextState == StateDeleting) {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{
			Key: key("HOST#"+env.Host, "WORKSPACE"),
		}})
	}
	if mutation.Lease != nil {
		outboxRaw, marshalErr := attributevalue.MarshalMap(makeLeaseOutboxItem(env))
		if marshalErr != nil {
			return MutationResult{}, marshalErr
		}
		items = append(items, putTransaction(outboxRaw, "", nil))
	}
	quotaIndexes := []int{}
	if workspaceDelta != 0 || checkpointDelta != 0 || pinnedCheckpointDelta != 0 {
		quotaIndexes = append(quotaIndexes, len(items))
		items = append(items, ownerQuotaTransaction(env.OwnerID, workspaceDelta, checkpointDelta,
			pinnedCheckpointDelta, quotaLimits))
	}
	if workspaceDelta != 0 || runningDelta != 0 || gpuDelta != 0 || checkpointDelta != 0 ||
		pinnedCheckpointDelta != 0 {
		quotaIndexes = append(quotaIndexes, len(items))
		items = append(items, globalQuotaTransaction(workspaceDelta, runningDelta, gpuDelta,
			checkpointDelta, pinnedCheckpointDelta, quotaLimits))
	}
	err = s.transact(ctx, mutation.Operation.ID, items)
	if err != nil {
		if replayed, found, replayErr := s.ReplayMutation(ctx, mutation.Idempotency); replayErr != nil || found {
			return replayed, replayErr
		}
		return MutationResult{}, classifyTransactionError(err, quotaIndexes...)
	}
	return MutationResult{Environment: env, Operation: mutation.Operation}, nil
}

func (s *DynamoStore) Extend(ctx context.Context, mutation ExtendMutation) (MutationResult, error) {
	if replayed, found, err := s.ReplayMutation(ctx, mutation.Idempotency); err != nil || found {
		return replayed, err
	}
	env, err := s.GetEnvironment(ctx, mutation.EnvironmentID)
	if err != nil {
		return MutationResult{}, err
	}
	if env.OwnerID != mutation.OwnerID || env.State != mutation.ExpectedState ||
		env.Lease.Version != mutation.ExpectedLease {
		return MutationResult{}, ErrConflict
	}
	env.Lease = mutation.Lease
	env.Version++
	env.UpdatedAt = mutation.Operation.RequestedAt
	envRaw, err := attributevalue.MarshalMap(makeEnvironmentItem(env))
	if err != nil {
		return MutationResult{}, err
	}
	opRaw, err := attributevalue.MarshalMap(makeOperationItem(mutation.Operation))
	if err != nil {
		return MutationResult{}, err
	}
	idemRaw, err := attributevalue.MarshalMap(makeIdempotencyItem(mutation.Idempotency, mutation.Operation))
	if err != nil {
		return MutationResult{}, err
	}
	outboxRaw, err := attributevalue.MarshalMap(makeLeaseOutboxItem(env))
	if err != nil {
		return MutationResult{}, err
	}
	err = s.transact(ctx, mutation.Operation.ID, []types.TransactWriteItem{
		putTransaction(envRaw,
			"environment.#owner = :owner AND environment.#state = :state AND environment.#lease.#version = :lease "+
				"AND environment.#version = :version",
			&transactionCondition{
				Names: map[string]string{"#owner": "OwnerID", "#state": "State", "#lease": "Lease", "#version": "Version"},
				Values: map[string]types.AttributeValue{
					":owner": avs(mutation.OwnerID), ":state": avs(string(mutation.ExpectedState)),
					":lease": avn(mutation.ExpectedLease), ":version": avn(mutation.ExpectedVersion),
				},
			}),
		putTransaction(opRaw, "attribute_not_exists(pk)", nil),
		putIdempotencyTransaction(idemRaw, s.now()),
		putTransaction(outboxRaw, "", nil),
	})
	if err != nil {
		if replayed, found, replayErr := s.ReplayMutation(ctx, mutation.Idempotency); replayErr != nil || found {
			return replayed, replayErr
		}
		return MutationResult{}, classifyTransactionError(err)
	}
	return MutationResult{Environment: env, Operation: mutation.Operation}, nil
}

func (s *DynamoStore) RecordActivity(ctx context.Context, id string, expectedVersion int64, lease Lease, occurredAt time.Time,
	kind string) (Environment, error) {
	env, err := s.GetEnvironment(ctx, id)
	if err != nil {
		return Environment{}, err
	}
	env.Lease = lease
	env.Version++
	env.UpdatedAt = occurredAt
	env.LastMeaningfulAt = &occurredAt
	env.LastMeaningfulKind = kind
	envRaw, err := attributevalue.MarshalMap(makeEnvironmentItem(env))
	if err != nil {
		return Environment{}, err
	}
	outboxRaw, err := attributevalue.MarshalMap(makeLeaseOutboxItem(env))
	if err != nil {
		return Environment{}, err
	}
	err = s.transact(ctx, "activity-"+id+"-"+strconv.FormatInt(lease.Version, 10), []types.TransactWriteItem{
		putTransaction(envRaw,
			"environment.#state = :running AND environment.#lease.#version = :previous "+
				"AND environment.#version = :version",
			&transactionCondition{
				Names: map[string]string{"#state": "State", "#lease": "Lease", "#version": "Version"},
				Values: map[string]types.AttributeValue{
					":running": avs(string(StateRunning)), ":previous": avn(lease.Version - 1),
					":version": avn(expectedVersion),
				},
			}),
		putTransaction(outboxRaw, "", nil),
	})
	if err != nil {
		return Environment{}, classifyTransactionError(err)
	}
	return env, nil
}

func (s *DynamoStore) BeginCheckpointDelete(ctx context.Context,
	mutation CheckpointMutation) (Operation, bool, error) {
	if replayed, found, err := s.ReplayMutation(ctx, mutation.Idempotency); err != nil || found {
		return replayed.Operation, found, err
	}
	checkpoint := mutation.Checkpoint
	checkpoint.State = CheckpointDeleting
	checkpointRaw, err := attributevalue.MarshalMap(makeCheckpointItem(checkpoint))
	if err != nil {
		return Operation{}, false, err
	}
	opRaw, err := attributevalue.MarshalMap(makeOperationItem(mutation.Operation))
	if err != nil {
		return Operation{}, false, err
	}
	idemRaw, err := attributevalue.MarshalMap(makeIdempotencyItem(mutation.Idempotency, mutation.Operation))
	if err != nil {
		return Operation{}, false, err
	}
	err = s.transact(ctx, mutation.Operation.ID, []types.TransactWriteItem{
		putTransaction(checkpointRaw,
			"checkpoint.#state = :available AND checkpoint.#references = :zero", &transactionCondition{
				Names: map[string]string{"#state": "State", "#references": "ReferenceCount"},
				Values: map[string]types.AttributeValue{
					":available": avs(string(CheckpointAvailable)), ":zero": avn(0),
				},
			}),
		putTransaction(opRaw, "attribute_not_exists(pk)", nil),
		putIdempotencyTransaction(idemRaw, s.now()),
	})
	if err != nil {
		if replayed, found, replayErr := s.ReplayMutation(ctx, mutation.Idempotency); replayErr != nil || found {
			return replayed.Operation, found, replayErr
		}
		return Operation{}, false, classifyTransactionError(err)
	}
	return mutation.Operation, false, nil
}

func (s *DynamoStore) Complete(ctx context.Context, mutation CompletionMutation) (MutationResult, error) {
	envRaw, err := attributevalue.MarshalMap(makeEnvironmentItem(mutation.Environment))
	if err != nil {
		return MutationResult{}, err
	}
	opRaw, err := attributevalue.MarshalMap(makeOperationItem(mutation.Operation))
	if err != nil {
		return MutationResult{}, err
	}
	items := []types.TransactWriteItem{
		putTransaction(envRaw,
			"environment.#state = :state AND environment.#version = :version AND environment.#active = :operation",
			&transactionCondition{
				Names: map[string]string{"#state": "State", "#version": "Version", "#active": "ActiveOperationID"},
				Values: map[string]types.AttributeValue{
					":state":   avs(string(mutation.ExpectedEnvironmentState)),
					":version": avn(mutation.ExpectedEnvironmentVersion), ":operation": avs(mutation.Operation.ID),
				},
			}),
		putTransaction(opRaw, "operation.#status = :status AND operation.#claim = :claim", &transactionCondition{
			Names: map[string]string{"#status": "Status", "#claim": "ClaimToken"},
			Values: map[string]types.AttributeValue{
				":status": avs(string(mutation.ExpectedOperationStatus)), ":claim": avs(mutation.ExpectedOperationClaim),
			},
		}),
	}
	if mutation.ReleaseCheckpoint != nil {
		items = append(items, checkpointReferenceTransaction(*mutation.ReleaseCheckpoint, -1))
	}
	if mutation.Checkpoint != nil {
		raw, marshalErr := attributevalue.MarshalMap(makeCheckpointItem(*mutation.Checkpoint))
		if marshalErr != nil {
			return MutationResult{}, marshalErr
		}
		items = append(items, putTransaction(raw, "attribute_not_exists(pk)", nil))
	}
	if mutation.Route != nil {
		items = append(items, putRouteTransaction(*mutation.Route))
	} else if mutation.DeleteRouteHost != "" {
		items = append(items, types.TransactWriteItem{Delete: &types.Delete{
			Key: key("HOST#"+mutation.DeleteRouteHost, "WORKSPACE"),
		}})
	}
	if mutation.ScheduleExpiry {
		raw, marshalErr := attributevalue.MarshalMap(makeLeaseOutboxItem(mutation.Environment))
		if marshalErr != nil {
			return MutationResult{}, marshalErr
		}
		items = append(items, putTransaction(raw, "", nil))
	}
	quotaIndexes := []int{}
	if mutation.OwnerWorkspaceDelta != 0 || mutation.CheckpointDelta != 0 || mutation.PinnedCheckpointDelta != 0 {
		quotaIndexes = append(quotaIndexes, len(items))
		items = append(items, ownerQuotaTransaction(mutation.Environment.OwnerID,
			mutation.OwnerWorkspaceDelta, mutation.CheckpointDelta, mutation.PinnedCheckpointDelta,
			mutation.QuotaLimits))
	}
	if mutation.GlobalWorkspaceDelta != 0 || mutation.RunningDelta != 0 || mutation.GPURunningDelta != 0 ||
		mutation.CheckpointDelta != 0 || mutation.PinnedCheckpointDelta != 0 {
		quotaIndexes = append(quotaIndexes, len(items))
		items = append(items, globalQuotaTransaction(mutation.GlobalWorkspaceDelta, mutation.RunningDelta,
			mutation.GPURunningDelta, mutation.CheckpointDelta, mutation.PinnedCheckpointDelta,
			mutation.QuotaLimits))
	}
	if err = s.transact(ctx, "complete-"+mutation.Operation.ID, items); err != nil {
		return MutationResult{}, classifyTransactionError(err, quotaIndexes...)
	}
	return MutationResult{Environment: mutation.Environment, Operation: mutation.Operation}, nil
}

func (s *DynamoStore) CompleteCheckpointDelete(ctx context.Context, operation Operation,
	checkpoint Checkpoint, completedAt time.Time, workflowErr error) (Operation, error) {
	expectedOperationStatus := operation.Status
	expectedCheckpointState := checkpoint.State
	operation.UpdatedAt = completedAt
	if workflowErr == nil {
		operation.Status = OperationSucceeded
		checkpoint.State = CheckpointDeleted
	} else {
		operation.Status = OperationFailed
		operation.Error = workflowErr.Error()
		checkpoint.State = CheckpointAvailable
	}
	opRaw, err := attributevalue.MarshalMap(makeOperationItem(operation))
	if err != nil {
		return Operation{}, err
	}
	checkpointRaw, err := attributevalue.MarshalMap(makeCheckpointItem(checkpoint))
	if err != nil {
		return Operation{}, err
	}
	items := []types.TransactWriteItem{
		putTransaction(opRaw, "operation.#status = :status AND operation.#claim = :claim", &transactionCondition{
			Names: map[string]string{"#status": "Status", "#claim": "ClaimToken"},
			Values: map[string]types.AttributeValue{
				":status": avs(string(expectedOperationStatus)), ":claim": avs(operation.ClaimToken),
			},
		}),
		putTransaction(checkpointRaw, "checkpoint.#state = :state", &transactionCondition{
			Names:  map[string]string{"#state": "State"},
			Values: map[string]types.AttributeValue{":state": avs(string(expectedCheckpointState))},
		}),
	}
	if workflowErr == nil {
		pinnedDelta := -boolInt(checkpoint.Pinned)
		items = append(items,
			ownerQuotaTransaction(checkpoint.OwnerID, 0, -1, pinnedDelta, DefaultQuotaLimits()),
			globalQuotaTransaction(0, 0, 0, -1, pinnedDelta, DefaultQuotaLimits()),
		)
	}
	err = s.transact(ctx, "complete-"+operation.ID, items)
	if err != nil {
		if current, getErr := s.GetOperation(ctx, operation.ID); getErr == nil &&
			(current.Status == OperationSucceeded || current.Status == OperationFailed) {
			return current, nil
		}
		return Operation{}, classifyTransactionError(err)
	}
	return operation, nil
}

func (s *DynamoStore) ListPendingOperations(ctx context.Context, limit int32) ([]Operation, error) {
	operations := make([]Operation, 0, limit)
	var start map[string]types.AttributeValue
	for int32(len(operations)) < limit {
		output, err := s.client.Scan(ctx, &dynamodb.ScanInput{
			TableName: aws.String(s.table), ExclusiveStartKey: start,
			FilterExpression: aws.String("#kind = :operation AND (operation.#status = :queued OR " +
				"(operation.#status = :running AND (attribute_not_exists(operation.#claim_until) OR operation.#claim_until <= :now)))"),
			ExpressionAttributeNames: map[string]string{
				"#kind": "kind", "#status": "Status", "#claim_until": "ClaimUntil",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":operation": avs(recordOperation), ":queued": avs(string(OperationQueued)),
				":running": avs(string(OperationRunning)), ":now": avs(s.now().Format(time.RFC3339Nano)),
			},
		})
		if err != nil {
			return nil, err
		}
		for _, raw := range output.Items {
			var item operationItem
			if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
				return nil, err
			}
			operations = append(operations, item.Operation)
			if int32(len(operations)) == limit {
				break
			}
		}
		if len(output.LastEvaluatedKey) == 0 {
			break
		}
		start = output.LastEvaluatedKey
	}
	return operations, nil
}

func (s *DynamoStore) ListPendingLeaseSchedules(ctx context.Context, limit int32) ([]Environment, error) {
	output, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName: aws.String(s.table), KeyConditionExpression: aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": avs("OUTBOX#LEASE")},
		Limit:                     aws.Int32(limit),
	})
	if err != nil {
		return nil, err
	}
	environments := make([]Environment, 0, len(output.Items))
	for _, raw := range output.Items {
		var item leaseOutboxItem
		if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
			return nil, err
		}
		environments = append(environments, item.Environment)
	}
	return environments, nil
}

func (s *DynamoStore) AckLeaseSchedule(ctx context.Context, id string, version int64) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table), Key: key("OUTBOX#LEASE", "ENV#"+id),
		ConditionExpression:       aws.String("lease_version = :version"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":version": avn(version)},
	})
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		return ErrConflict
	}
	return err
}

func (s *DynamoStore) ListDue(ctx context.Context, now time.Time, limit int32) ([]DueItem, error) {
	items := []DueItem{}
	for _, partition := range []string{"DUE#ENV", "DUE#CHECKPOINT"} {
		remaining := limit - int32(len(items))
		if remaining <= 0 {
			break
		}
		output, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName: aws.String(s.table), IndexName: aws.String(s.dueIndex),
			KeyConditionExpression: aws.String("due_pk = :partition AND due_at <= :now"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":partition": avs(partition), ":now": avs(sortableTime(now)),
			},
			Limit: aws.Int32(remaining),
		})
		if err != nil {
			return nil, err
		}
		for _, raw := range output.Items {
			if partition == "DUE#ENV" {
				var item environmentItem
				if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
					return nil, err
				}
				items = append(items, DueItem{Environment: &item.Environment})
			} else {
				var item checkpointItem
				if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
					return nil, err
				}
				items = append(items, DueItem{Checkpoint: &item.Checkpoint})
			}
		}
	}
	return items, nil
}

func (s *DynamoStore) SetOperationExecution(ctx context.Context, id, arn string) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table), Key: key("OP#"+id, "META"),
		UpdateExpression:         aws.String("SET operation.#arn = :arn"),
		ConditionExpression:      aws.String("attribute_exists(pk) AND (operation.#status = :queued OR operation.#status = :running)"),
		ExpressionAttributeNames: map[string]string{"#arn": "ExecutionARN", "#status": "Status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":arn": avs(arn), ":running": avs(string(OperationRunning)), ":queued": avs(string(OperationQueued)),
		},
	})
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		operation, getErr := s.GetOperation(ctx, id)
		if getErr == nil && (operation.Status == OperationSucceeded || operation.Status == OperationFailed) {
			return nil
		}
	}
	return err
}

func (s *DynamoStore) FailDispatch(ctx context.Context, operation Operation, dispatchErr error) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table), Key: key("OP#"+operation.ID, "META"),
		UpdateExpression:         aws.String("SET operation.#error = :error"),
		ConditionExpression:      aws.String("attribute_exists(pk) AND operation.#status = :queued"),
		ExpressionAttributeNames: map[string]string{"#status": "Status", "#error": "Error"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":queued": avs(string(OperationQueued)), ":error": avs(dispatchErr.Error()),
		},
	})
	return err
}

func (s *DynamoStore) ReconcileQuotas(ctx context.Context) error {
	environments := []Environment{}
	checkpoints := []Checkpoint{}
	current := map[string]quotaItem{}
	var start map[string]types.AttributeValue
	for {
		output, err := s.client.Scan(ctx, &dynamodb.ScanInput{
			TableName: aws.String(s.table), ExclusiveStartKey: start, ConsistentRead: aws.Bool(true),
			FilterExpression:         aws.String("#kind = :environment OR #kind = :checkpoint OR #kind = :quota"),
			ExpressionAttributeNames: map[string]string{"#kind": "kind"},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":environment": avs(recordEnvironment), ":checkpoint": avs(recordCheckpoint),
				":quota": avs(recordQuota),
			},
		})
		if err != nil {
			return err
		}
		for _, raw := range output.Items {
			kind, ok := raw["kind"].(*types.AttributeValueMemberS)
			if !ok {
				continue
			}
			switch kind.Value {
			case recordEnvironment:
				var item environmentItem
				if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
					return err
				}
				environments = append(environments, item.Environment)
			case recordCheckpoint:
				var item checkpointItem
				if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
					return err
				}
				checkpoints = append(checkpoints, item.Checkpoint)
			case recordQuota:
				var item quotaItem
				if err = attributevalue.UnmarshalMap(raw, &item); err != nil {
					return err
				}
				current[item.PK] = item
			}
		}
		if len(output.LastEvaluatedKey) == 0 {
			break
		}
		start = output.LastEvaluatedKey
	}
	expected := observedQuotas(environments, checkpoints)
	for pk := range current {
		if strings.HasPrefix(pk, "QUOTA#OWNER#") {
			if _, ok := expected[pk]; !ok {
				expected[pk] = quotaItem{PK: pk, SK: "COUNTERS", Kind: recordQuota}
			}
		}
	}
	for pk, want := range expected {
		old, found := current[pk]
		want = raiseOnlyQuota(old, want, found)
		if found && old.Workspaces == want.Workspaces && old.Running == want.Running &&
			old.GPURunning == want.GPURunning && old.Checkpoints == want.Checkpoints &&
			old.PinnedCheckpoints == want.PinnedCheckpoints {
			continue
		}
		if err := s.repairQuota(ctx, old, want, found); err != nil {
			return err
		}
	}
	return nil
}

func observedQuotas(environments []Environment, checkpoints []Checkpoint) map[string]quotaItem {
	expected := map[string]quotaItem{
		"QUOTA#GLOBAL": {PK: "QUOTA#GLOBAL", SK: "COUNTERS", Kind: recordQuota},
	}
	ownerQuota := func(ownerID string) (string, quotaItem) {
		key := "QUOTA#OWNER#" + ownerID
		quota := expected[key]
		quota.PK, quota.SK, quota.Kind = key, "COUNTERS", recordQuota
		return key, quota
	}
	for _, env := range environments {
		global := expected["QUOTA#GLOBAL"]
		if env.WorkspaceSlot {
			global.Workspaces++
			ownerKey, owner := ownerQuota(env.OwnerID)
			owner.Workspaces++
			expected[ownerKey] = owner
		}
		if env.RunningSlot {
			global.Running++
		}
		if env.GPURunningSlot {
			global.GPURunning++
		}
		if env.CheckpointQuotaReserved {
			global.Checkpoints++
			ownerKey, owner := ownerQuota(env.OwnerID)
			owner.Checkpoints++
			if env.PinnedCheckpointQuotaReserved {
				global.PinnedCheckpoints++
				owner.PinnedCheckpoints++
			}
			expected[ownerKey] = owner
		}
		expected["QUOTA#GLOBAL"] = global
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.State == CheckpointDeleted {
			continue
		}
		global := expected["QUOTA#GLOBAL"]
		global.Checkpoints++
		ownerKey, owner := ownerQuota(checkpoint.OwnerID)
		owner.Checkpoints++
		if checkpoint.Pinned {
			global.PinnedCheckpoints++
			owner.PinnedCheckpoints++
		}
		expected["QUOTA#GLOBAL"] = global
		expected[ownerKey] = owner
	}
	return expected
}

func raiseOnlyQuota(current, observed quotaItem, found bool) quotaItem {
	if !found {
		return observed
	}
	if current.Workspaces > observed.Workspaces {
		observed.Workspaces = current.Workspaces
	}
	if current.Running > observed.Running {
		observed.Running = current.Running
	}
	if current.GPURunning > observed.GPURunning {
		observed.GPURunning = current.GPURunning
	}
	if current.Checkpoints > observed.Checkpoints {
		observed.Checkpoints = current.Checkpoints
	}
	if current.PinnedCheckpoints > observed.PinnedCheckpoints {
		observed.PinnedCheckpoints = current.PinnedCheckpoints
	}
	return observed
}

func (s *DynamoStore) repairQuota(ctx context.Context, old, want quotaItem, found bool) error {
	want.Version = old.Version + 1
	condition := "attribute_not_exists(pk)"
	values := map[string]types.AttributeValue{
		":workspaces": avn(int64(want.Workspaces)), ":running": avn(int64(want.Running)),
		":gpu": avn(int64(want.GPURunning)), ":checkpoints": avn(int64(want.Checkpoints)),
		":pinned": avn(int64(want.PinnedCheckpoints)), ":kind": avs(recordQuota),
		":next": avn(want.Version),
	}
	if found {
		condition = "#version = :previous"
		values[":previous"] = avn(old.Version)
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table), Key: key(want.PK, "COUNTERS"),
		UpdateExpression: aws.String("SET #kind = :kind, workspaces = :workspaces, running = :running, " +
			"gpu_running = :gpu, checkpoints = :checkpoints, pinned_checkpoints = :pinned, #version = :next"),
		ConditionExpression:       aws.String(condition),
		ExpressionAttributeNames:  map[string]string{"#kind": "kind", "#version": "version"},
		ExpressionAttributeValues: values,
	})
	var conditional *types.ConditionalCheckFailedException
	if errors.As(err, &conditional) {
		return ErrConflict
	}
	return err
}

func (s *DynamoStore) getIdempotency(ctx context.Context,
	idem Idempotency) (idempotencyItem, bool, error) {
	if idem.Key == "" {
		return idempotencyItem{}, false, nil
	}
	output, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: key("IDEMPOTENCY#"+idem.OwnerID, idem.Key),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return idempotencyItem{}, false, err
	}
	if len(output.Item) == 0 {
		return idempotencyItem{}, false, nil
	}
	var item idempotencyItem
	if err = attributevalue.UnmarshalMap(output.Item, &item); err != nil {
		return idempotencyItem{}, false, err
	}
	if item.ExpiresAt <= s.now().Unix() {
		return idempotencyItem{}, false, nil
	}
	return item, true, nil
}

func makeEnvironmentItem(env Environment) environmentItem {
	item := environmentItem{
		PK: "ENV#" + env.ID, SK: "META", Kind: recordEnvironment, Environment: env,
		OwnerPK: "OWNER#" + env.OwnerID,
		OwnerSK: fmt.Sprintf("ENV#%020d#%s", env.CreatedAt.Unix(), env.ID),
	}
	switch env.State {
	case StateProvisioning, StateStarting, StateRunning:
		item.DuePK, item.DueAt = "DUE#ENV", sortableTime(env.Lease.DueAt())
	case StateStopped:
		if env.ArchiveAfter != nil {
			item.DuePK, item.DueAt = "DUE#ENV", sortableTime(*env.ArchiveAfter)
		}
	case StateError:
		if dueAt := env.ScheduledActionAt(); !dueAt.IsZero() {
			item.DuePK, item.DueAt = "DUE#ENV", sortableTime(dueAt)
		}
	}
	return item
}

func makeOperationItem(operation Operation) operationItem {
	return operationItem{PK: "OP#" + operation.ID, SK: "META", Kind: recordOperation, Operation: operation}
}

func makeCheckpointItem(checkpoint Checkpoint) checkpointItem {
	item := checkpointItem{
		PK: "CHECKPOINT#" + checkpoint.ID, SK: "META", Kind: recordCheckpoint,
		Checkpoint: checkpoint, OwnerPK: "OWNER#" + checkpoint.OwnerID,
		OwnerSK: fmt.Sprintf("CHECKPOINT#%020d#%s", checkpoint.CreatedAt.Unix(), checkpoint.ID),
	}
	if !checkpoint.Pinned && checkpoint.ExpiresAt != nil && checkpoint.State == CheckpointAvailable {
		item.DuePK, item.DueAt = "DUE#CHECKPOINT", sortableTime(*checkpoint.ExpiresAt)
	}
	return item
}

func makeIdempotencyItem(idem Idempotency, operation Operation) idempotencyItem {
	return idempotencyItem{
		PK: "IDEMPOTENCY#" + idem.OwnerID, SK: idem.Key, Kind: recordIdempotency,
		RequestHash: idem.RequestHash, EnvironmentID: operation.EnvironmentID,
		OperationID: operation.ID, CheckpointID: operation.CheckpointID,
		ExpiresAt: idem.ExpiresAt.Unix(), TTL: idem.ExpiresAt.Unix(),
	}
}

func makeLeaseOutboxItem(env Environment) leaseOutboxItem {
	return leaseOutboxItem{
		PK: "OUTBOX#LEASE", SK: "ENV#" + env.ID, Kind: recordLeaseOutbox,
		Environment: env, LeaseVersion: env.Lease.Version,
	}
}

type transactionCondition struct {
	Names  map[string]string
	Values map[string]types.AttributeValue
}

func putTransaction(item map[string]types.AttributeValue, expression string,
	condition *transactionCondition) types.TransactWriteItem {
	put := &types.Put{TableName: nil, Item: item}
	if expression != "" {
		put.ConditionExpression = aws.String(expression)
	}
	if condition != nil {
		put.ExpressionAttributeNames = condition.Names
		put.ExpressionAttributeValues = condition.Values
	}
	return types.TransactWriteItem{Put: put}
}

func checkpointReferenceTransaction(checkpoint Checkpoint, delta int64) types.TransactWriteItem {
	minimum := int64(0)
	if delta < 0 {
		minimum = 1
	}
	return types.TransactWriteItem{Update: &types.Update{
		Key:              key("CHECKPOINT#"+checkpoint.ID, "META"),
		UpdateExpression: aws.String("SET checkpoint.#references = checkpoint.#references + :delta"),
		ConditionExpression: aws.String("checkpoint.#state = :available AND checkpoint.#owner = :owner AND " +
			"checkpoint.#snapshot = :snapshot AND checkpoint.#references >= :minimum"),
		ExpressionAttributeNames: map[string]string{
			"#references": "ReferenceCount", "#state": "State", "#owner": "OwnerID", "#snapshot": "SnapshotID",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":delta": avn(delta), ":minimum": avn(minimum), ":available": avs(string(CheckpointAvailable)),
			":owner": avs(checkpoint.OwnerID), ":snapshot": avs(checkpoint.SnapshotID),
		},
	}}
}

func putIdempotencyTransaction(item map[string]types.AttributeValue, now time.Time) types.TransactWriteItem {
	return putTransaction(item, "attribute_not_exists(pk) OR expires_at <= :now", &transactionCondition{
		Values: map[string]types.AttributeValue{":now": avn(now.Unix())},
	})
}

func ownerQuotaTransaction(ownerID string, workspaceDelta, checkpointDelta, pinnedDelta int,
	limits QuotaLimits) types.TransactWriteItem {
	conditions := []string{}
	values := map[string]types.AttributeValue{
		":kind": avs(recordQuota), ":zero": avn(0), ":one": avn(1),
		":workspaces": avn(int64(workspaceDelta)), ":checkpoints": avn(int64(checkpointDelta)),
		":pinned": avn(int64(pinnedDelta)),
	}
	appendQuotaCondition(&conditions, values, "workspaces", "workspace", workspaceDelta,
		limits.MaxOwnerWorkspaces)
	appendQuotaCondition(&conditions, values, "checkpoints", "checkpoint", checkpointDelta,
		limits.MaxOwnerCheckpoints)
	appendQuotaCondition(&conditions, values, "pinned_checkpoints", "pinned", pinnedDelta,
		limits.MaxOwnerPinnedCheckpoints)
	return types.TransactWriteItem{Update: &types.Update{
		Key: key("QUOTA#OWNER#"+ownerID, "COUNTERS"),
		UpdateExpression: aws.String("SET #kind = if_not_exists(#kind, :kind), " +
			"workspaces = if_not_exists(workspaces, :zero) + :workspaces, " +
			"checkpoints = if_not_exists(checkpoints, :zero) + :checkpoints, " +
			"pinned_checkpoints = if_not_exists(pinned_checkpoints, :zero) + :pinned, " +
			"#version = if_not_exists(#version, :zero) + :one"),
		ConditionExpression:       aws.String(strings.Join(conditions, " AND ")),
		ExpressionAttributeNames:  map[string]string{"#kind": "kind", "#version": "version"},
		ExpressionAttributeValues: values,
	}}
}

func globalQuotaTransaction(workspaceDelta, runningDelta, gpuDelta, checkpointDelta, pinnedDelta int,
	limits QuotaLimits) types.TransactWriteItem {
	condition := []string{}
	values := map[string]types.AttributeValue{
		":kind": avs(recordQuota), ":zero": avn(0), ":one": avn(1),
		":workspaces": avn(int64(workspaceDelta)), ":running": avn(int64(runningDelta)),
		":gpu": avn(int64(gpuDelta)), ":checkpoints": avn(int64(checkpointDelta)),
		":pinned": avn(int64(pinnedDelta)),
	}
	appendQuotaCondition(&condition, values, "workspaces", "workspace", workspaceDelta, limits.MaxWorkspaces)
	appendQuotaCondition(&condition, values, "running", "running", runningDelta, limits.MaxRunning)
	appendQuotaCondition(&condition, values, "gpu_running", "gpu", gpuDelta, limits.MaxGPURunning)
	appendQuotaCondition(&condition, values, "checkpoints", "checkpoint", checkpointDelta,
		limits.MaxCheckpoints)
	appendQuotaCondition(&condition, values, "pinned_checkpoints", "pinned", pinnedDelta,
		limits.MaxPinnedCheckpoints)
	return types.TransactWriteItem{Update: &types.Update{
		Key: key("QUOTA#GLOBAL", "COUNTERS"),
		UpdateExpression: aws.String("SET #kind = if_not_exists(#kind, :kind), " +
			"workspaces = if_not_exists(workspaces, :zero) + :workspaces, " +
			"running = if_not_exists(running, :zero) + :running, " +
			"gpu_running = if_not_exists(gpu_running, :zero) + :gpu, " +
			"checkpoints = if_not_exists(checkpoints, :zero) + :checkpoints, " +
			"pinned_checkpoints = if_not_exists(pinned_checkpoints, :zero) + :pinned, " +
			"#version = if_not_exists(#version, :zero) + :one"),
		ConditionExpression:       aws.String(strings.Join(condition, " AND ")),
		ExpressionAttributeNames:  map[string]string{"#kind": "kind", "#version": "version"},
		ExpressionAttributeValues: values,
	}}
}

func appendQuotaCondition(conditions *[]string, values map[string]types.AttributeValue,
	attribute, label string, delta, maximum int) {
	if delta > 0 {
		if delta > maximum {
			*conditions = append(*conditions, "attribute_exists("+attribute+") AND attribute_not_exists("+attribute+")")
			return
		}
		ceiling := ":" + label + "_ceiling"
		*conditions = append(*conditions, "(attribute_not_exists("+attribute+") OR "+attribute+" <= "+ceiling+")")
		values[ceiling] = avn(int64(maximum - delta))
	} else if delta < 0 {
		minimum := ":" + label + "_minimum"
		*conditions = append(*conditions, attribute+" >= "+minimum)
		values[minimum] = avn(int64(-delta))
	}
}

func putRouteTransaction(route GatewayRoute) types.TransactWriteItem {
	item := map[string]types.AttributeValue{
		"pk": avs("HOST#" + route.Host), "sk": avs("WORKSPACE"),
		"workspace_id": avs(route.WorkspaceID), "name": avs(route.Name), "host": avs(route.Host),
		"upstream": avs(route.Upstream), "state": avs(string(StateRunning)),
		"acl_version": avn(route.ACLVersion), "visibility": avs(string(route.Visibility)),
		"tls_cert_sha256": avs(route.TLSCertSHA256),
	}
	if route.Visibility == VisibilityRestricted {
		item["allowed_principals"] = &types.AttributeValueMemberSS{Value: route.AllowedPrincipals}
	}
	return putTransaction(item, "attribute_not_exists(pk) OR workspace_id = :workspace", &transactionCondition{
		Values: map[string]types.AttributeValue{":workspace": avs(route.WorkspaceID)},
	})
}

func classifyTransactionError(err error, quotaIndexes ...int) error {
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return err
	}
	for _, index := range quotaIndexes {
		if index < len(canceled.CancellationReasons) &&
			aws.ToString(canceled.CancellationReasons[index].Code) == "ConditionalCheckFailed" {
			return ErrQuotaExceeded
		}
	}
	return ErrConflict
}

func (s *DynamoStore) transact(ctx context.Context, token string, items []types.TransactWriteItem) error {
	for index := range items {
		if items[index].Put != nil {
			items[index].Put.TableName = aws.String(s.table)
		}
		if items[index].Update != nil {
			items[index].Update.TableName = aws.String(s.table)
		}
		if items[index].Delete != nil {
			items[index].Delete.TableName = aws.String(s.table)
		}
		if items[index].ConditionCheck != nil {
			items[index].ConditionCheck.TableName = aws.String(s.table)
		}
	}
	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: items, ClientRequestToken: aws.String(transactionToken(token)),
	})
	return err
}

func transactionToken(operationID string) string {
	if len(operationID) <= 36 {
		return operationID
	}
	return operationID[len(operationID)-36:]
}

func key(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": avs(pk), "sk": avs(sk)}
}

func avs(value string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: value}
}

func avn(value int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(value, 10)}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func sortableTime(value time.Time) string {
	return fmt.Sprintf("%020d", value.UTC().Unix())
}
