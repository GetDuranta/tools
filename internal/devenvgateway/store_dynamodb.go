package devenvgateway

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	workspaceSortKey = "WORKSPACE"
	bootstrapSortKey = "BOOTSTRAP"
	sessionSortKey   = "SESSION"
)

var tokenHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type DynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	TransactGetItems(context.Context, *dynamodb.TransactGetItemsInput,
		...func(*dynamodb.Options)) (*dynamodb.TransactGetItemsOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput,
		...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

type DynamoStore struct {
	client DynamoAPI
	table  string
	now    func() time.Time
}

func NewDynamoStore(client DynamoAPI, table string) *DynamoStore {
	return &DynamoStore{client: client, table: table, now: time.Now}
}

func (s *DynamoStore) WorkspaceForPrincipals(ctx context.Context, host string,
	principals []string) (Workspace, error) {
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: workspaceKey(host), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Workspace{}, err
	}
	workspace, err := decodeWorkspace(result.Item)
	if err != nil || workspace.Host != host || !workspace.Running() || !workspace.Allows(principals) {
		return Workspace{}, ErrNotFound
	}
	return workspace, nil
}

func (s *DynamoStore) PutBootstrap(ctx context.Context, grant BootstrapGrant) error {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if !tokenHashPattern.MatchString(grant.CodeHash) || grant.WorkspaceID == "" || grant.Host == "" || grant.Subject == "" ||
		len(grant.Principals) == 0 || grant.ACLVersion <= 0 || grant.ReturnPath == "" || grant.ExpiresAt.IsZero() {
		return errors.New("invalid bootstrap grant")
	}
	if !now.Before(grant.ExpiresAt) || grant.ExpiresAt.After(now.Add(time.Minute)) {
		return errors.New("bootstrap grant expiry must be within 60 seconds")
	}
	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item: map[string]types.AttributeValue{
			"pk":           stringValue("CODE#" + grant.CodeHash),
			"sk":           stringValue(bootstrapSortKey),
			"workspace_id": stringValue(grant.WorkspaceID),
			"host":         stringValue(grant.Host),
			"subject":      stringValue(grant.Subject),
			"principals":   stringSet(grant.Principals),
			"acl_version":  numberValue(grant.ACLVersion),
			"return_path":  stringValue(grant.ReturnPath),
			"expires_at":   numberValue(grant.ExpiresAt.Unix()),
			"ttl":          numberValue(grant.ExpiresAt.Unix()),
		},
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if isConditionalFailure(err) {
		return ErrConflict
	}
	return err
}

func (s *DynamoStore) ConsumeBootstrap(ctx context.Context, codeHash, host string,
	session Session, now time.Time) (BootstrapGrant, error) {
	if !tokenHashPattern.MatchString(codeHash) || !tokenHashPattern.MatchString(session.TokenHash) {
		return BootstrapGrant{}, ErrNotFound
	}
	codeKey := bootstrapKey(codeHash)
	result, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: codeKey, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return BootstrapGrant{}, err
	}
	grant, err := decodeBootstrap(result.Item, codeHash)
	if err != nil || grant.Host != host || !now.Before(grant.ExpiresAt) || session.TokenHash == "" ||
		session.ExpiresAt.IsZero() || !now.Before(session.ExpiresAt) {
		return BootstrapGrant{}, ErrNotFound
	}

	condition, names, values := workspaceBindingCondition(grant.WorkspaceID, grant.Host, grant.ACLVersion)
	session.WorkspaceID = grant.WorkspaceID
	session.Host = grant.Host
	session.Subject = grant.Subject
	session.Principals = grant.Principals
	session.ACLVersion = grant.ACLVersion
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Delete: &types.Delete{
				TableName: aws.String(s.table), Key: codeKey,
				ConditionExpression: aws.String("expires_at > :now AND host = :host AND workspace_id = :workspace AND acl_version = :acl AND subject = :subject AND principals = :principals"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":now": numberValue(now.Unix()), ":host": stringValue(host),
					":workspace": stringValue(grant.WorkspaceID), ":acl": numberValue(grant.ACLVersion),
					":subject": stringValue(grant.Subject), ":principals": stringSet(grant.Principals),
				},
			}},
			{ConditionCheck: &types.ConditionCheck{
				TableName: aws.String(s.table), Key: workspaceKey(host), ConditionExpression: aws.String(condition),
				ExpressionAttributeNames: names, ExpressionAttributeValues: values,
			}},
			{Put: &types.Put{
				TableName: aws.String(s.table), Item: sessionItem(session),
				ConditionExpression: aws.String("attribute_not_exists(pk)"),
			}},
		},
	})
	if isConditionalFailure(err) {
		return BootstrapGrant{}, ErrNotFound
	}
	if err != nil {
		return BootstrapGrant{}, err
	}
	return grant, nil
}

func (s *DynamoStore) AuthorizedWorkspace(ctx context.Context, tokenHash, host string,
	now time.Time) (Workspace, error) {
	if !tokenHashPattern.MatchString(tokenHash) {
		return Workspace{}, ErrNotFound
	}
	result, err := s.client.TransactGetItems(ctx, &dynamodb.TransactGetItemsInput{
		TransactItems: []types.TransactGetItem{
			{Get: &types.Get{TableName: aws.String(s.table), Key: sessionKey(tokenHash)}},
			{Get: &types.Get{TableName: aws.String(s.table), Key: workspaceKey(host)}},
		},
	})
	if err != nil {
		return Workspace{}, err
	}
	if len(result.Responses) != 2 {
		return Workspace{}, ErrNotFound
	}
	session, err := decodeSession(result.Responses[0].Item, tokenHash)
	if err != nil || session.Host != host || !now.Before(session.ExpiresAt) {
		return Workspace{}, ErrNotFound
	}
	workspace, err := decodeWorkspace(result.Responses[1].Item)
	if err != nil || workspace.Host != host || !workspace.Running() || workspace.ID != session.WorkspaceID ||
		workspace.ACLVersion != session.ACLVersion {
		return Workspace{}, ErrNotFound
	}
	return workspace, nil
}

func workspaceKey(host string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": stringValue("HOST#" + host), "sk": stringValue(workspaceSortKey)}
}

func bootstrapKey(hash string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": stringValue("CODE#" + hash), "sk": stringValue(bootstrapSortKey)}
}

func sessionKey(hash string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": stringValue("SESSION#" + hash), "sk": stringValue(sessionSortKey)}
}

func sessionItem(session Session) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": stringValue("SESSION#" + session.TokenHash), "sk": stringValue(sessionSortKey),
		"workspace_id": stringValue(session.WorkspaceID), "host": stringValue(session.Host),
		"subject": stringValue(session.Subject), "principals": stringSet(session.Principals),
		"acl_version": numberValue(session.ACLVersion), "expires_at": numberValue(session.ExpiresAt.Unix()),
		"ttl": numberValue(session.ExpiresAt.Unix()),
	}
}

func workspaceBindingCondition(workspaceID, host string, aclVersion int64) (string, map[string]string,
	map[string]types.AttributeValue) {
	names := map[string]string{"#state": "state"}
	values := map[string]types.AttributeValue{
		":running": stringValue(WorkspaceStateRunning), ":workspace": stringValue(workspaceID),
		":host": stringValue(host), ":acl": numberValue(aclVersion),
	}
	return "#state = :running AND workspace_id = :workspace AND host = :host AND acl_version = :acl", names, values
}

func decodeWorkspace(item map[string]types.AttributeValue) (Workspace, error) {
	workspace := Workspace{}
	var err error
	if len(item) == 0 {
		return workspace, ErrNotFound
	}
	workspace.ID, err = requiredString(item, "workspace_id")
	if err != nil {
		return Workspace{}, err
	}
	workspace.Name, _ = optionalString(item, "name")
	workspace.Host, err = requiredString(item, "host")
	if err != nil {
		return Workspace{}, err
	}
	workspace.Upstream, err = requiredString(item, "upstream")
	if err != nil {
		return Workspace{}, err
	}
	workspace.TLSCertSHA256, _ = optionalString(item, "tls_cert_sha256")
	workspace.State, err = requiredString(item, "state")
	if err != nil {
		return Workspace{}, err
	}
	workspace.Visibility, err = requiredString(item, "visibility")
	if err != nil {
		return Workspace{}, err
	}
	workspace.ACLVersion, err = requiredInt64(item, "acl_version")
	if err != nil {
		return Workspace{}, err
	}
	workspace.AllowedPrincipals, _ = optionalStringSet(item, "allowed_principals")
	if workspace.Visibility != WorkspaceVisibilityOrg && workspace.Visibility != WorkspaceVisibilityRestricted {
		return Workspace{}, errors.New("invalid workspace visibility")
	}
	if workspace.Visibility == WorkspaceVisibilityRestricted && len(workspace.AllowedPrincipals) == 0 {
		return Workspace{}, errors.New("restricted workspace has no principals")
	}
	return workspace, nil
}

func decodeBootstrap(item map[string]types.AttributeValue, hash string) (BootstrapGrant, error) {
	grant := BootstrapGrant{CodeHash: hash}
	var err error
	if len(item) == 0 {
		return grant, ErrNotFound
	}
	grant.WorkspaceID, err = requiredString(item, "workspace_id")
	if err != nil {
		return BootstrapGrant{}, err
	}
	grant.Host, err = requiredString(item, "host")
	if err != nil {
		return BootstrapGrant{}, err
	}
	grant.Subject, err = requiredString(item, "subject")
	if err != nil {
		return BootstrapGrant{}, err
	}
	grant.Principals, err = requiredStringSet(item, "principals")
	if err != nil {
		return BootstrapGrant{}, err
	}
	grant.ACLVersion, err = requiredInt64(item, "acl_version")
	if err != nil {
		return BootstrapGrant{}, err
	}
	grant.ReturnPath, err = requiredString(item, "return_path")
	if err != nil {
		return BootstrapGrant{}, err
	}
	expiresAt, err := requiredInt64(item, "expires_at")
	grant.ExpiresAt = time.Unix(expiresAt, 0)
	return grant, err
}

func decodeSession(item map[string]types.AttributeValue, hash string) (Session, error) {
	session := Session{TokenHash: hash}
	var err error
	if len(item) == 0 {
		return session, ErrNotFound
	}
	session.WorkspaceID, err = requiredString(item, "workspace_id")
	if err != nil {
		return Session{}, err
	}
	session.Host, err = requiredString(item, "host")
	if err != nil {
		return Session{}, err
	}
	session.Subject, err = requiredString(item, "subject")
	if err != nil {
		return Session{}, err
	}
	session.Principals, err = requiredStringSet(item, "principals")
	if err != nil {
		return Session{}, err
	}
	session.ACLVersion, err = requiredInt64(item, "acl_version")
	if err != nil {
		return Session{}, err
	}
	expiresAt, err := requiredInt64(item, "expires_at")
	session.ExpiresAt = time.Unix(expiresAt, 0)
	return session, err
}

func stringValue(value string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: value}
}

func stringSet(values []string) types.AttributeValue {
	return &types.AttributeValueMemberSS{Value: values}
}

func numberValue(value int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(value, 10)}
}

func requiredString(item map[string]types.AttributeValue, name string) (string, error) {
	value, ok := item[name].(*types.AttributeValueMemberS)
	if !ok || value.Value == "" {
		return "", fmt.Errorf("missing %s", name)
	}
	return value.Value, nil
}

func optionalString(item map[string]types.AttributeValue, name string) (string, bool) {
	value, ok := item[name].(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return value.Value, true
}

func requiredStringSet(item map[string]types.AttributeValue, name string) ([]string, error) {
	value, ok := item[name].(*types.AttributeValueMemberSS)
	if !ok || len(value.Value) == 0 {
		return nil, fmt.Errorf("missing %s", name)
	}
	return value.Value, nil
}

func optionalStringSet(item map[string]types.AttributeValue, name string) ([]string, bool) {
	value, ok := item[name].(*types.AttributeValueMemberSS)
	if !ok {
		return nil, false
	}
	return value.Value, true
}

func requiredInt64(item map[string]types.AttributeValue, name string) (int64, error) {
	value, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("missing %s", name)
	}
	parsed, err := strconv.ParseInt(value.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return parsed, nil
}

func intersects(left, right []string) bool {
	set := make(map[string]struct{}, len(left))
	for _, value := range left {
		set[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := set[value]; ok {
			return true
		}
	}
	return false
}

func isConditionalFailure(err error) bool {
	if err == nil {
		return false
	}
	var conditional *types.ConditionalCheckFailedException
	var canceled *types.TransactionCanceledException
	return errors.As(err, &conditional) || errors.As(err, &canceled)
}
