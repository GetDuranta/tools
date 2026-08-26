package devenvgateway

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeDynamoAPI struct {
	getOutput     *dynamodb.GetItemOutput
	getInputs     []*dynamodb.GetItemInput
	putInput      *dynamodb.PutItemInput
	transactGet   *dynamodb.TransactGetItemsOutput
	transactWrite *dynamodb.TransactWriteItemsInput
}

func (f *fakeDynamoAPI) GetItem(_ context.Context, input *dynamodb.GetItemInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.getInputs = append(f.getInputs, input)
	if f.getOutput == nil {
		return &dynamodb.GetItemOutput{}, nil
	}
	return f.getOutput, nil
}

func (f *fakeDynamoAPI) PutItem(_ context.Context, input *dynamodb.PutItemInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putInput = input
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamoAPI) TransactGetItems(_ context.Context, _ *dynamodb.TransactGetItemsInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.TransactGetItemsOutput, error) {
	if f.transactGet == nil {
		return &dynamodb.TransactGetItemsOutput{}, nil
	}
	return f.transactGet, nil
}

func (f *fakeDynamoAPI) TransactWriteItems(_ context.Context, input *dynamodb.TransactWriteItemsInput,
	_ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.transactWrite = input
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func TestDynamoStoreWritesHashOnlyBootstrapRecord(t *testing.T) {
	client := &fakeDynamoAPI{}
	store := NewDynamoStore(client, "dev-environments")
	expiresAt := time.Date(2026, 8, 27, 12, 1, 0, 0, time.UTC)
	store.now = func() time.Time { return expiresAt.Add(-time.Minute) }
	grant := BootstrapGrant{
		CodeHash: strings.Repeat("a", 64), WorkspaceID: "env-1", Host: "feature.preview.test",
		Subject: "owner:v1:one", Principals: []string{"owner:v1:one"}, ACLVersion: 2,
		ReturnPath: "/a/", ExpiresAt: expiresAt,
	}
	if err := store.PutBootstrap(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	if client.putInput == nil || stringAttribute(client.putInput.Item["pk"]) != "CODE#"+grant.CodeHash ||
		stringAttribute(client.putInput.Item["sk"]) != bootstrapSortKey ||
		numberAttribute(client.putInput.Item["expires_at"]) != expiresAt.Unix() ||
		numberAttribute(client.putInput.Item["ttl"]) != expiresAt.Unix() ||
		client.putInput.Item["code"] != nil || client.putInput.Item["token"] != nil {
		t.Fatalf("unexpected item: %#v", client.putInput.Item)
	}
}

func TestDynamoStoreConsumesGrantAtomicallyWithExactBinding(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	codeHash := strings.Repeat("b", 64)
	client := &fakeDynamoAPI{getOutput: &dynamodb.GetItemOutput{Item: bootstrapItem(BootstrapGrant{
		CodeHash: codeHash, WorkspaceID: "env-1", Host: "feature.preview.test",
		Subject: "owner:v1:one", Principals: []string{"iam:one"}, ACLVersion: 7,
		ReturnPath: "/a/", ExpiresAt: now.Add(time.Minute),
	})}}
	store := NewDynamoStore(client, "dev-environments")
	grant, err := store.ConsumeBootstrap(context.Background(), codeHash, "feature.preview.test",
		Session{TokenHash: strings.Repeat("c", 64), ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Subject != "owner:v1:one" || client.transactWrite == nil ||
		len(client.transactWrite.TransactItems) != 3 {
		t.Fatalf("unexpected consume: %#v %#v", grant, client.transactWrite)
	}
	items := client.transactWrite.TransactItems
	deleteCondition := *items[0].Delete.ConditionExpression
	if !strings.Contains(deleteCondition, "subject = :subject") ||
		!strings.Contains(deleteCondition, "principals = :principals") {
		t.Fatalf("grant binding missing: %s", deleteCondition)
	}
	workspaceCondition := *items[1].ConditionCheck.ConditionExpression
	if workspaceCondition != "#state = :running AND workspace_id = :workspace AND host = :host AND acl_version = :acl" ||
		items[1].ConditionCheck.ExpressionAttributeValues[":running"].(*types.AttributeValueMemberS).Value != "RUNNING" {
		t.Fatalf("unexpected workspace binding: %s", workspaceCondition)
	}
	session := items[2].Put.Item
	if stringAttribute(session["workspace_id"]) != "env-1" ||
		stringAttribute(session["subject"]) != "owner:v1:one" ||
		numberAttribute(session["acl_version"]) != 7 {
		t.Fatalf("unexpected session: %#v", session)
	}
}

func TestDynamoStoreSessionSurvivesACLPrincipalMappingButNotVersionChange(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	tokenHash := strings.Repeat("d", 64)
	session := Session{
		TokenHash: tokenHash, WorkspaceID: "env-1", Host: "feature.preview.test",
		Subject: "iam:owner", Principals: []string{"iam:owner"}, ACLVersion: 5, ExpiresAt: now.Add(time.Hour),
	}
	workspace := gatewayWorkspace("http://10.0.1.2:8080")
	workspace.Visibility = WorkspaceVisibilityRestricted
	workspace.AllowedPrincipals = []string{"owner:v1:different-auth-namespace"}
	workspace.ACLVersion = 5
	client := &fakeDynamoAPI{transactGet: &dynamodb.TransactGetItemsOutput{Responses: []types.ItemResponse{
		{Item: sessionItem(session)}, {Item: workspaceItem(workspace)},
	}}}
	store := NewDynamoStore(client, "dev-environments")
	if _, err := store.AuthorizedWorkspace(context.Background(), tokenHash, workspace.Host, now); err != nil {
		t.Fatalf("session should remain bound by ACL version: %v", err)
	}
	workspace.ACLVersion++
	client.transactGet.Responses[1].Item = workspaceItem(workspace)
	if _, err := store.AuthorizedWorkspace(context.Background(), tokenHash, workspace.Host, now); err == nil {
		t.Fatal("ACL version change did not revoke the session")
	}
}

func TestDynamoStoreAppliesVisibilityWhenIssuingHumanGrant(t *testing.T) {
	workspace := gatewayWorkspace("http://10.0.1.2:8080")
	client := &fakeDynamoAPI{getOutput: &dynamodb.GetItemOutput{Item: workspaceItem(workspace)}}
	store := NewDynamoStore(client, "dev-environments")
	if _, err := store.WorkspaceForPrincipals(context.Background(), workspace.Host, []string{"user:any"}); err != nil {
		t.Fatalf("organization preview rejected: %v", err)
	}
	workspace.Visibility = WorkspaceVisibilityRestricted
	workspace.AllowedPrincipals = []string{"group:allowed"}
	client.getOutput.Item = workspaceItem(workspace)
	if _, err := store.WorkspaceForPrincipals(context.Background(), workspace.Host, []string{"group:other"}); err == nil {
		t.Fatal("restricted preview allowed an unrelated principal")
	}
	if _, err := store.WorkspaceForPrincipals(context.Background(), workspace.Host, []string{"group:allowed"}); err != nil {
		t.Fatalf("restricted preview rejected its ACL: %v", err)
	}
}

func bootstrapItem(grant BootstrapGrant) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": stringValue("CODE#" + grant.CodeHash), "sk": stringValue(bootstrapSortKey),
		"workspace_id": stringValue(grant.WorkspaceID), "host": stringValue(grant.Host),
		"subject": stringValue(grant.Subject), "principals": stringSet(grant.Principals),
		"acl_version": numberValue(grant.ACLVersion), "return_path": stringValue(grant.ReturnPath),
		"expires_at": numberValue(grant.ExpiresAt.Unix()), "ttl": numberValue(grant.ExpiresAt.Unix()),
	}
}

func workspaceItem(workspace Workspace) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{
		"pk": stringValue("HOST#" + workspace.Host), "sk": stringValue(workspaceSortKey),
		"workspace_id": stringValue(workspace.ID), "name": stringValue(workspace.Name),
		"host": stringValue(workspace.Host), "upstream": stringValue(workspace.Upstream),
		"state": stringValue(workspace.State), "visibility": stringValue(workspace.Visibility),
		"acl_version": numberValue(workspace.ACLVersion),
	}
	if len(workspace.AllowedPrincipals) > 0 {
		item["allowed_principals"] = stringSet(workspace.AllowedPrincipals)
	}
	if workspace.TLSCertSHA256 != "" {
		item["tls_cert_sha256"] = stringValue(workspace.TLSCertSHA256)
	}
	return item
}

func stringAttribute(value types.AttributeValue) string {
	if value, ok := value.(*types.AttributeValueMemberS); ok {
		return value.Value
	}
	return ""
}

func numberAttribute(value types.AttributeValue) int64 {
	if value, ok := value.(*types.AttributeValueMemberN); ok {
		parsed, _ := strconv.ParseInt(value.Value, 10, 64)
		return parsed
	}
	return 0
}
