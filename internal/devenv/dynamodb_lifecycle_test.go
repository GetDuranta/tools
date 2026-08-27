package devenv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type captureDynamo struct {
	DynamoDBAPI
	transaction *dynamodb.TransactWriteItemsInput
	update      *dynamodb.UpdateItemInput
	updateErr   error
	deleteErr   error
}

func (c *captureDynamo) GetItem(context.Context, *dynamodb.GetItemInput,
	...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (c *captureDynamo) TransactWriteItems(_ context.Context, input *dynamodb.TransactWriteItemsInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	c.transaction = input
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func (c *captureDynamo) DeleteItem(context.Context, *dynamodb.DeleteItemInput,
	...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return nil, c.deleteErr
}

func (c *captureDynamo) UpdateItem(_ context.Context, input *dynamodb.UpdateItemInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	c.update = input
	return &dynamodb.UpdateItemOutput{}, c.updateErr
}

func TestDynamoCreateReferencesCheckpointInSameTransaction(t *testing.T) {
	client := &captureDynamo{}
	store := NewDynamoStore(client, "state", "owner", "due")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	checkpoint := Checkpoint{
		ID: "checkpoint-1", OwnerID: "owner-1", SnapshotID: "snap-1",
		State: CheckpointAvailable, ReferenceCount: 1,
	}
	_, err := store.Create(context.Background(), CreateMutation{
		Environment: Environment{
			ID: "env-1", OwnerID: "owner-1", Name: "workspace", Profile: ProfileStandard,
			State: StateProvisioning, CreatedAt: now, Lease: NewLease(now), WorkspaceSlot: true, RunningSlot: true,
		},
		Operation:        Operation{ID: "op-1", EnvironmentID: "env-1"},
		Idempotency:      Idempotency{OwnerID: "owner-1", Key: "create-env-1", ExpiresAt: now.Add(time.Hour)},
		SourceCheckpoint: &checkpoint,
	}, DefaultQuotaLimits())
	if err != nil {
		t.Fatal(err)
	}
	update := checkpointUpdate(t, client.transaction, checkpoint.ID)
	if got := attributeNumber(t, update.ExpressionAttributeValues[":delta"]); got != 1 {
		t.Fatalf("unexpected reference delta: %d", got)
	}
	condition := stringValuePointer(update.ConditionExpression)
	for _, fragment := range []string{"#state = :available", "#owner = :owner", "#snapshot = :snapshot"} {
		if !strings.Contains(condition, fragment) {
			t.Fatalf("checkpoint update is missing %q: %s", fragment, condition)
		}
	}
}

func TestDynamoCompletionReleasesCheckpointInSameTransaction(t *testing.T) {
	client := &captureDynamo{}
	store := NewDynamoStore(client, "state", "owner", "due")
	checkpoint := Checkpoint{
		ID: "checkpoint-1", OwnerID: "owner-1", SnapshotID: "snap-1",
		State: CheckpointAvailable, ReferenceCount: 2,
	}
	_, err := store.Complete(context.Background(), CompletionMutation{
		Environment:              Environment{ID: "env-1", OwnerID: "owner-1"},
		Operation:                Operation{ID: "op-1", Status: OperationSucceeded, ClaimToken: "claim-1"},
		ExpectedEnvironmentState: StateStarting, ExpectedEnvironmentVersion: 1,
		ExpectedOperationStatus: OperationRunning, ExpectedOperationClaim: "claim-1",
		ReleaseCheckpoint: &checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	update := checkpointUpdate(t, client.transaction, checkpoint.ID)
	if got := attributeNumber(t, update.ExpressionAttributeValues[":delta"]); got != -1 {
		t.Fatalf("unexpected reference delta: %d", got)
	}
	if got := attributeNumber(t, update.ExpressionAttributeValues[":minimum"]); got != 1 {
		t.Fatalf("decrement is not guarded against underflow: %d", got)
	}
}

func TestDynamoCheckpointDeleteRequiresZeroReferences(t *testing.T) {
	client := &captureDynamo{}
	store := NewDynamoStore(client, "state", "owner", "due")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	checkpoint := Checkpoint{
		ID: "checkpoint-1", OwnerID: "owner-1", SnapshotID: "snap-1",
		State: CheckpointAvailable,
	}
	_, _, err := store.BeginCheckpointDelete(context.Background(), CheckpointMutation{
		Checkpoint:  checkpoint,
		Operation:   Operation{ID: "op-1", CheckpointID: checkpoint.ID},
		Idempotency: Idempotency{OwnerID: "owner-1", Key: "delete-checkpoint-1", ExpiresAt: now.Add(time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var condition string
	for _, item := range client.transaction.TransactItems {
		if item.Put != nil && attributeString(item.Put.Item["pk"]) == "CHECKPOINT#"+checkpoint.ID {
			condition = stringValuePointer(item.Put.ConditionExpression)
		}
	}
	if !strings.Contains(condition, "#references = :zero") {
		t.Fatalf("checkpoint delete lacks reference guard: %s", condition)
	}
}

func TestStaleLeaseAckReturnsConflict(t *testing.T) {
	client := &captureDynamo{deleteErr: &types.ConditionalCheckFailedException{}}
	store := NewDynamoStore(client, "state", "owner", "due")
	if err := store.AckLeaseSchedule(context.Background(), "env-1", 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected stale ACK conflict, got %v", err)
	}
}

func TestReleaseOperationClaimIsConditionalOnExactClaim(t *testing.T) {
	client := &captureDynamo{}
	store := NewDynamoStore(client, "state", "owner", "due")
	now := time.Date(2026, 8, 28, 12, 0, 0, 123, time.UTC)
	if err := store.ReleaseOperationClaim(context.Background(), "op-1", "claim-1", now); err != nil {
		t.Fatal(err)
	}
	if got := attributeString(client.update.Key["pk"]); got != "OP#op-1" {
		t.Fatalf("operation key = %q", got)
	}
	condition := aws.ToString(client.update.ConditionExpression)
	for _, expected := range []string{"operation.#status = :running", "operation.#claim = :claim"} {
		if !strings.Contains(condition, expected) {
			t.Fatalf("release condition is missing %q: %s", expected, condition)
		}
	}
	update := aws.ToString(client.update.UpdateExpression)
	for _, expected := range []string{
		"operation.#status = :queued", "operation.#updated = :updated",
		"REMOVE operation.#claim, operation.#claim_until",
	} {
		if !strings.Contains(update, expected) {
			t.Fatalf("release update is missing %q: %s", expected, update)
		}
	}
	if got := attributeString(client.update.ExpressionAttributeValues[":claim"]); got != "claim-1" {
		t.Fatalf("claim condition = %q", got)
	}
	if got := attributeString(client.update.ExpressionAttributeValues[":updated"]); got != now.Format(time.RFC3339Nano) {
		t.Fatalf("updated timestamp = %q", got)
	}
}

func TestReleaseOperationClaimRejectsStaleClaim(t *testing.T) {
	client := &captureDynamo{updateErr: &types.ConditionalCheckFailedException{}}
	store := NewDynamoStore(client, "state", "owner", "due")
	if err := store.ReleaseOperationClaim(context.Background(), "op-1", "stale", time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected stale claim conflict, got %v", err)
	}
}

func TestQuotaReconciliationNeverLowersCounters(t *testing.T) {
	current := quotaItem{
		Workspaces: 10, Running: 8, GPURunning: 3, Checkpoints: 20, PinnedCheckpoints: 7,
	}
	observed := quotaItem{
		Workspaces: 4, Running: 5, GPURunning: 1, Checkpoints: 12, PinnedCheckpoints: 2,
	}
	got := raiseOnlyQuota(current, observed, true)
	if got.Workspaces != 10 || got.Running != 8 || got.GPURunning != 3 ||
		got.Checkpoints != 20 || got.PinnedCheckpoints != 7 {
		t.Fatalf("quota repair lowered counters: %+v", got)
	}
}

func TestObservedCheckpointQuotaIncludesReservationsAndLiveRecords(t *testing.T) {
	observed := observedQuotas([]Environment{{
		OwnerID: "owner-1", WorkspaceSlot: true,
		CheckpointQuotaReserved: true, PinnedCheckpointQuotaReserved: true,
	}}, []Checkpoint{
		{OwnerID: "owner-1", State: CheckpointAvailable},
		{OwnerID: "owner-2", State: CheckpointDeleting, Pinned: true},
		{OwnerID: "owner-2", State: CheckpointDeleted, Pinned: true},
	})
	global := observed["QUOTA#GLOBAL"]
	ownerOne := observed["QUOTA#OWNER#owner-1"]
	ownerTwo := observed["QUOTA#OWNER#owner-2"]
	if global.Workspaces != 1 || global.Checkpoints != 3 || global.PinnedCheckpoints != 2 {
		t.Fatalf("unexpected global checkpoint observation: %+v", global)
	}
	if ownerOne.Workspaces != 1 || ownerOne.Checkpoints != 2 || ownerOne.PinnedCheckpoints != 1 {
		t.Fatalf("unexpected owner-1 checkpoint observation: %+v", ownerOne)
	}
	if ownerTwo.Checkpoints != 1 || ownerTwo.PinnedCheckpoints != 1 {
		t.Fatalf("unexpected owner-2 checkpoint observation: %+v", ownerTwo)
	}
}

func TestArchiveQuotaReservationIsAtomicUnderConcurrency(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		owners [2]string
		limit  func(*QuotaLimits)
	}{
		{name: "owner total", owners: [2]string{"owner-1", "owner-1"},
			limit: func(limits *QuotaLimits) { limits.MaxOwnerCheckpoints = 1 }},
		{name: "owner pinned", owners: [2]string{"owner-1", "owner-1"},
			limit: func(limits *QuotaLimits) { limits.MaxOwnerPinnedCheckpoints = 1 }},
		{name: "global total", owners: [2]string{"owner-1", "owner-2"},
			limit: func(limits *QuotaLimits) { limits.MaxCheckpoints = 1 }},
		{name: "global pinned", owners: [2]string{"owner-1", "owner-2"},
			limit: func(limits *QuotaLimits) { limits.MaxPinnedCheckpoints = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &archiveQuotaDynamo{environments: map[string]Environment{
				"env-1": quotaTestEnvironment("env-1", test.owners[0], now),
				"env-2": quotaTestEnvironment("env-2", test.owners[1], now),
			}, quotas: map[string]quotaCounts{}}
			store := NewDynamoStore(client, "state", "owner", "due")
			limits := DefaultQuotaLimits()
			limits.MaxOwnerCheckpoints = 100
			limits.MaxOwnerPinnedCheckpoints = 100
			limits.MaxCheckpoints = 100
			limits.MaxPinnedCheckpoints = 100
			test.limit(&limits)
			start := make(chan struct{})
			errorsFound := make(chan error, 2)
			for index, id := range []string{"env-1", "env-2"} {
				go func(index int, id, owner string) {
					<-start
					_, err := store.Begin(context.Background(), BeginMutation{
						EnvironmentID: id, OwnerID: owner, ExpectedState: StateRunning, ExpectedVersion: 1,
						NextState: StateArchiving, CurrentProfile: ProfileStandard,
						Operation: Operation{
							ID: fmt.Sprintf("op-%d", index), EnvironmentID: id, OwnerID: owner,
							Action: ActionArchive, CheckpointPinned: true, RequestedAt: now,
						},
						Idempotency: Idempotency{
							OwnerID: owner, Key: fmt.Sprintf("archive-%d", index), ExpiresAt: now.Add(time.Hour),
						},
					}, limits)
					errorsFound <- err
				}(index, id, test.owners[index])
			}
			close(start)
			succeeded, limited := 0, 0
			for range 2 {
				err := <-errorsFound
				switch {
				case err == nil:
					succeeded++
				case errors.Is(err, ErrQuotaExceeded):
					limited++
				default:
					t.Fatalf("unexpected archive result: %v", err)
				}
			}
			if succeeded != 1 || limited != 1 {
				t.Fatalf("concurrent archives bypassed quota: succeeded=%d limited=%d", succeeded, limited)
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if client.quotas["QUOTA#GLOBAL"].checkpoints != 1 || client.reservedWrites != 1 {
				t.Fatalf("archive reservation was not atomic: quotas=%+v writes=%d",
					client.quotas, client.reservedWrites)
			}
		})
	}
}

func TestSystemArchiveUsesBoundedWorkspaceHeadroom(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	limits := QuotaLimits{
		MaxOwnerWorkspaces: 3, MaxWorkspaces: 5, MaxRunning: 5, MaxGPURunning: 1,
		MaxOwnerCheckpoints: 2, MaxOwnerPinnedCheckpoints: 2,
		MaxCheckpoints: 4, MaxPinnedCheckpoints: 4,
	}
	begin := func(store *DynamoStore, id string, state State, actor string) error {
		_, err := store.Begin(context.Background(), BeginMutation{
			EnvironmentID: id, OwnerID: "owner-1", ExpectedState: state, ExpectedVersion: 1,
			NextState: StateArchiving, CurrentProfile: ProfileStandard,
			Operation: Operation{
				ID: "op-" + id, EnvironmentID: id, OwnerID: "owner-1", ActorPrincipal: actor,
				Action: ActionArchive, RequestedAt: now,
			},
			Idempotency: Idempotency{
				OwnerID: actor, Key: "archive-" + id, ExpiresAt: now.Add(time.Hour),
			},
		}, limits)
		return err
	}

	client := &archiveQuotaDynamo{
		environments: map[string]Environment{
			"user":   quotaTestEnvironment("user", "owner-1", now),
			"system": quotaTestEnvironment("system", "owner-1", now),
		},
		quotas: map[string]quotaCounts{
			"QUOTA#OWNER#owner-1": {checkpoints: limits.MaxOwnerCheckpoints},
			"QUOTA#GLOBAL":        {checkpoints: limits.MaxCheckpoints},
		},
	}
	client.environments["system"] = quotaTestEnvironment("system", "owner-1", now)
	client.environments["system"] = withState(client.environments["system"], StateError)
	store := NewDynamoStore(client, "state", "owner", "due")
	if err := begin(store, "user", StateRunning, "owner-1"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("user archive bypassed checkpoint quota: %v", err)
	}
	if err := begin(store, "system", StateError, "system:lease"); err != nil {
		t.Fatalf("system cleanup was blocked at the user checkpoint quota: %v", err)
	}

	client = &archiveQuotaDynamo{
		environments: map[string]Environment{
			"system": withState(quotaTestEnvironment("system", "owner-1", now), StateError),
		},
		quotas: map[string]quotaCounts{
			"QUOTA#OWNER#owner-1": {
				checkpoints: limits.MaxOwnerCheckpoints + limits.MaxOwnerWorkspaces,
			},
			"QUOTA#GLOBAL": {
				checkpoints: limits.MaxCheckpoints + limits.MaxWorkspaces,
			},
		},
	}
	store = NewDynamoStore(client, "state", "owner", "due")
	if err := begin(store, "system", StateError, "system:lease"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("system cleanup exceeded configured emergency headroom: %v", err)
	}
}

func TestArchiveRetryReusesQuotaReservationAtFullQuota(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	limits := DefaultQuotaLimits()
	env := withState(quotaTestEnvironment("env-1", "owner-1", now), StateError)
	env.CheckpointQuotaReserved = true
	env.PinnedCheckpointQuotaReserved = true
	env.CheckpointQuotaName = "keep-this-checkpoint"
	client := &archiveQuotaDynamo{
		environments: map[string]Environment{"env-1": env},
		quotas: map[string]quotaCounts{
			"QUOTA#OWNER#owner-1": {
				checkpoints: limits.MaxOwnerCheckpoints, pinned: limits.MaxOwnerPinnedCheckpoints,
			},
			"QUOTA#GLOBAL": {
				checkpoints: limits.MaxCheckpoints, pinned: limits.MaxPinnedCheckpoints,
			},
		},
	}
	store := NewDynamoStore(client, "state", "owner", "due")
	result, err := store.Begin(context.Background(), BeginMutation{
		EnvironmentID: env.ID, OwnerID: env.OwnerID, ExpectedState: StateError, ExpectedVersion: env.Version,
		NextState: StateArchiving, CurrentProfile: env.Profile,
		Operation: Operation{
			ID: "op-retry", EnvironmentID: env.ID, OwnerID: env.OwnerID, ActorPrincipal: "system:lease",
			Action: ActionArchive, RequestedAt: now,
		},
		Idempotency: Idempotency{
			OwnerID: "system:lease", Key: "archive-retry", ExpiresAt: now.Add(time.Hour),
		},
	}, limits)
	if err != nil {
		t.Fatalf("archive retry was blocked by a reservation it already owns: %v", err)
	}
	if !result.Environment.CheckpointQuotaReserved || !result.Environment.PinnedCheckpointQuotaReserved ||
		result.Environment.CheckpointQuotaName != env.CheckpointQuotaName ||
		!result.Operation.CheckpointPinned || result.Operation.CheckpointName != env.CheckpointQuotaName {
		t.Fatalf("archive retry lost reservation metadata: %+v %+v", result.Environment, result.Operation)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if got := client.quotas["QUOTA#OWNER#owner-1"]; got.checkpoints != limits.MaxOwnerCheckpoints ||
		got.pinned != limits.MaxOwnerPinnedCheckpoints {
		t.Fatalf("archive retry double-counted owner reservation: %+v", got)
	}
	if got := client.quotas["QUOTA#GLOBAL"]; got.checkpoints != limits.MaxCheckpoints ||
		got.pinned != limits.MaxPinnedCheckpoints {
		t.Fatalf("archive retry double-counted global reservation: %+v", got)
	}
}

func TestFailedArchiveAndCheckpointDeleteReleaseQuota(t *testing.T) {
	client := &captureDynamo{}
	store := NewDynamoStore(client, "state", "owner", "due")
	limits := DefaultQuotaLimits()
	_, err := store.Complete(context.Background(), CompletionMutation{
		Environment: Environment{ID: "env-1", OwnerID: "owner-1"},
		Operation: Operation{ID: "op-archive", Action: ActionArchive, Status: OperationFailed,
			ClaimToken: "claim-1"},
		ExpectedEnvironmentState: StateArchiving, ExpectedEnvironmentVersion: 1,
		ExpectedOperationStatus: OperationRunning, ExpectedOperationClaim: "claim-1",
		CheckpointDelta: -1, PinnedCheckpointDelta: -1, QuotaLimits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointQuotaDelta(t, client.transaction, "QUOTA#OWNER#owner-1", -1, -1)
	assertCheckpointQuotaDelta(t, client.transaction, "QUOTA#GLOBAL", -1, -1)

	checkpoint := Checkpoint{
		ID: "checkpoint-1", OwnerID: "owner-1", State: CheckpointDeleting, Pinned: true,
	}
	_, err = store.CompleteCheckpointDelete(context.Background(), Operation{
		ID: "op-delete", CheckpointID: checkpoint.ID, Status: OperationRunning, ClaimToken: "claim-2",
	}, checkpoint, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointQuotaDelta(t, client.transaction, "QUOTA#OWNER#owner-1", -1, -1)
	assertCheckpointQuotaDelta(t, client.transaction, "QUOTA#GLOBAL", -1, -1)
}

func TestErrorDueIndexUsesRecoveryRetryWithoutMovingCleanupDeadline(t *testing.T) {
	cleanupAfter := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	retryAfter := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)
	item := makeEnvironmentItem(Environment{
		ID: "env-1", OwnerID: "owner-1", State: StateError,
		CleanupAfter: &cleanupAfter, RecoveryRetryAfter: &retryAfter,
	})
	if item.DueAt != sortableTime(retryAfter) {
		t.Fatalf("ERROR due index ignored recovery retry: %q", item.DueAt)
	}
}

func checkpointUpdate(t *testing.T, input *dynamodb.TransactWriteItemsInput, id string) *types.Update {
	t.Helper()
	if input == nil {
		t.Fatal("transaction was not written")
	}
	for _, item := range input.TransactItems {
		if item.Update != nil && attributeString(item.Update.Key["pk"]) == "CHECKPOINT#"+id {
			return item.Update
		}
	}
	t.Fatalf("checkpoint update %s not found", id)
	return nil
}

func attributeString(value types.AttributeValue) string {
	if value, ok := value.(*types.AttributeValueMemberS); ok {
		return value.Value
	}
	return ""
}

func attributeNumber(t *testing.T, value types.AttributeValue) int64 {
	t.Helper()
	valueNumber, ok := value.(*types.AttributeValueMemberN)
	if !ok {
		t.Fatalf("not a number attribute: %T", value)
	}
	var result int64
	if _, err := fmt.Sscan(valueNumber.Value, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func stringValuePointer(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type quotaCounts struct {
	checkpoints int
	pinned      int
}

type archiveQuotaDynamo struct {
	DynamoDBAPI
	mu             sync.Mutex
	environments   map[string]Environment
	quotas         map[string]quotaCounts
	reservedWrites int
}

func (d *archiveQuotaDynamo) GetItem(_ context.Context, input *dynamodb.GetItemInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	pk := attributeString(input.Key["pk"])
	if !strings.HasPrefix(pk, "ENV#") {
		return &dynamodb.GetItemOutput{}, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	env, found := d.environments[strings.TrimPrefix(pk, "ENV#")]
	if !found {
		return &dynamodb.GetItemOutput{}, nil
	}
	raw, err := attributevalue.MarshalMap(makeEnvironmentItem(env))
	return &dynamodb.GetItemOutput{Item: raw}, err
}

func (d *archiveQuotaDynamo) TransactWriteItems(_ context.Context, input *dynamodb.TransactWriteItemsInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	type change struct {
		pk                  string
		checkpoints, pinned int
	}
	changes := []change{}
	reserved := false
	for index, item := range input.TransactItems {
		if item.Put != nil && strings.HasPrefix(attributeString(item.Put.Item["pk"]), "ENV#") {
			var stored environmentItem
			if err := attributevalue.UnmarshalMap(item.Put.Item, &stored); err != nil {
				return nil, err
			}
			if stored.Environment.CheckpointQuotaReserved {
				reserved = true
			}
		}
		if item.Update == nil {
			continue
		}
		pk := attributeString(item.Update.Key["pk"])
		if !strings.HasPrefix(pk, "QUOTA#") {
			continue
		}
		checkpointDelta := attributeInt(item.Update.ExpressionAttributeValues[":checkpoints"])
		pinnedDelta := attributeInt(item.Update.ExpressionAttributeValues[":pinned"])
		current := d.quotas[pk]
		if checkpointDelta > 0 && current.checkpoints >
			attributeInt(item.Update.ExpressionAttributeValues[":checkpoint_ceiling"]) ||
			pinnedDelta > 0 && current.pinned >
				attributeInt(item.Update.ExpressionAttributeValues[":pinned_ceiling"]) {
			reasons := make([]types.CancellationReason, len(input.TransactItems))
			reasons[index].Code = aws.String("ConditionalCheckFailed")
			return nil, &types.TransactionCanceledException{CancellationReasons: reasons}
		}
		changes = append(changes, change{pk: pk, checkpoints: checkpointDelta, pinned: pinnedDelta})
	}
	for _, change := range changes {
		current := d.quotas[change.pk]
		current.checkpoints += change.checkpoints
		current.pinned += change.pinned
		d.quotas[change.pk] = current
	}
	if reserved {
		d.reservedWrites++
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func quotaTestEnvironment(id, ownerID string, now time.Time) Environment {
	return Environment{
		ID: id, OwnerID: ownerID, Name: "workspace", State: StateRunning,
		Profile: ProfileStandard, Visibility: VisibilityRestricted,
		Source: Source{Repository: "repo", Ref: "main"}, Lease: NewLease(now), Version: 1,
	}
}

func withState(env Environment, state State) Environment {
	env.State = state
	return env
}

func attributeInt(value types.AttributeValue) int {
	number, ok := value.(*types.AttributeValueMemberN)
	if !ok {
		return 0
	}
	var result int
	_, _ = fmt.Sscan(number.Value, &result)
	return result
}

func assertCheckpointQuotaDelta(t *testing.T, input *dynamodb.TransactWriteItemsInput,
	pk string, checkpoints, pinned int64) {
	t.Helper()
	if input == nil {
		t.Fatal("transaction was not written")
	}
	for _, item := range input.TransactItems {
		if item.Update == nil || attributeString(item.Update.Key["pk"]) != pk {
			continue
		}
		if got := attributeNumber(t, item.Update.ExpressionAttributeValues[":checkpoints"]); got != checkpoints {
			t.Fatalf("%s checkpoint delta = %d", pk, got)
		}
		if got := attributeNumber(t, item.Update.ExpressionAttributeValues[":pinned"]); got != pinned {
			t.Fatalf("%s pinned checkpoint delta = %d", pk, got)
		}
		return
	}
	t.Fatalf("quota update %s not found", pk)
}
