package devenv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/GetDuranta/tools/internal/devaccess"
	"github.com/aws/aws-lambda-go/events"
)

type IAMIdentityResolver struct {
	Namespace               string
	TrustedAutomationRoles  map[string]struct{}
	InteractiveRolePrefixes []string
	GatewayRoleARN          string
}

func (r IAMIdentityResolver) Resolve(request events.APIGatewayV2HTTPRequest) (Identity, error) {
	return r.resolve(request, false)
}

func (r IAMIdentityResolver) ResolveActivity(request events.APIGatewayV2HTTPRequest) (Identity, error) {
	return r.resolve(request, true)
}

func (r IAMIdentityResolver) resolve(request events.APIGatewayV2HTTPRequest, allowInstance bool) (Identity, error) {
	if request.RequestContext.Authorizer == nil || request.RequestContext.Authorizer.IAM == nil {
		return Identity{}, ErrUnauthorized
	}
	iam := request.RequestContext.Authorizer.IAM
	if iam.AccountID == "" || iam.UserARN == "" || iam.UserID == "" || r.Namespace == "" {
		return Identity{}, ErrUnauthorized
	}
	roleARN, sessionName, err := assumedRole(iam.UserARN, iam.AccountID)
	if err != nil {
		return Identity{}, ErrUnauthorized
	}
	email, emailErr := devaccess.NormalizeEmail(sessionName)
	if emailErr == nil && hasRolePrefix(roleARN, r.InteractiveRolePrefixes) {
		ownerID, ownerErr := devaccess.CanonicalOwnerID(r.Namespace, email)
		if ownerErr != nil {
			return Identity{}, ErrUnauthorized
		}
		return Identity{
			PrincipalID: ownerID, Source: IdentitySourceAWSIAM, AuditSubject: iam.UserARN, AccountID: iam.AccountID,
			Email: email, SessionName: sessionName, RoleARN: roleARN,
		}, nil
	}
	if _, trusted := r.TrustedAutomationRoles[roleARN]; !trusted {
		if !allowInstance {
			return Identity{}, ErrUnauthorized
		}
		digest := sha256.Sum256([]byte("instance:v1\x00" + r.Namespace + "\x00" + iam.UserARN))
		return Identity{
			PrincipalID: "instance:v1:" + hex.EncodeToString(digest[:]), Source: IdentitySourceAWSIAM,
			AuditSubject: iam.UserARN, AccountID: iam.AccountID, SessionName: sessionName, RoleARN: roleARN,
		}, nil
	}
	digest := sha256.Sum256([]byte("automation:v1\x00" + r.Namespace + "\x00" + roleARN))
	return Identity{
		PrincipalID: "automation:v1:" + hex.EncodeToString(digest[:]), Source: IdentitySourceAWSIAM,
		AuditSubject: iam.UserARN, AccountID: iam.AccountID, SessionName: sessionName, RoleARN: roleARN,
		Internal: true,
	}, nil
}

func hasRolePrefix(roleARN string, prefixes []string) bool {
	rolePartition, roleAccount, _, roleName, roleOK := iamRole(roleARN)
	if !roleOK {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		prefixPartition, prefixAccount, prefixPath, prefixName, prefixOK := iamRole(prefix)
		if roleOK && prefixOK && rolePartition == prefixPartition && roleAccount == prefixAccount &&
			identityCenterPath.MatchString(prefixPath) && identityCenterPrefix.MatchString(prefixName) &&
			matchesGeneratedRoleName(roleName, prefixName) {
			return true
		}
	}
	return false
}

var identityCenterPath = regexp.MustCompile(`^aws-reserved/sso\.amazonaws\.com/(?:[a-z]{2}(?:-gov)?-[a-z]+-[0-9]/)?$`)
var identityCenterPrefix = regexp.MustCompile(`^AWSReservedSSO_[A-Za-z0-9+=,.@_-]+_$`)

func iamRole(raw string) (partition, accountID, path, name string, ok bool) {
	parts := strings.SplitN(raw, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] == "" || parts[2] != "iam" ||
		parts[3] != "" || len(parts[4]) != 12 || strings.Trim(parts[4], "0123456789") != "" {
		return "", "", "", "", false
	}
	resource, found := strings.CutPrefix(parts[5], "role/")
	if !found {
		return "", "", "", "", false
	}
	path = ""
	name = resource
	if slash := strings.LastIndexByte(resource, '/'); slash >= 0 {
		path = resource[:slash+1]
		name = resource[slash+1:]
	}
	if name == "" {
		return "", "", "", "", false
	}
	return parts[1], parts[4], path, name, true
}

func matchesGeneratedRoleName(name, prefix string) bool {
	suffix, found := strings.CutPrefix(name, prefix)
	if !found || len(suffix) != 16 {
		return false
	}
	for _, char := range suffix {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}

func (r IAMIdentityResolver) ResolveGatewayActor(request events.APIGatewayV2HTTPRequest,
	email, auditSubject string) (Identity, error) {
	iam, err := r.resolveGatewayRole(request)
	if err != nil {
		return Identity{}, ErrUnauthorized
	}
	normalized, err := devaccess.NormalizeEmail(email)
	if err != nil {
		return Identity{}, ErrUnauthorized
	}
	ownerID, err := devaccess.CanonicalOwnerID(r.Namespace, normalized)
	if err != nil {
		return Identity{}, ErrUnauthorized
	}
	return Identity{
		PrincipalID: ownerID, Source: IdentitySourceALBOIDC, Email: normalized,
		AuditSubject: strings.TrimSpace(auditSubject), AccountID: iam.AccountID,
	}, nil
}

func (r IAMIdentityResolver) ResolveGatewaySystem(request events.APIGatewayV2HTTPRequest) (Identity, error) {
	iam, err := r.resolveGatewayRole(request)
	if err != nil {
		return Identity{}, err
	}
	digest := sha256.Sum256([]byte("gateway:v1\x00" + r.Namespace + "\x00" + r.GatewayRoleARN))
	return Identity{
		PrincipalID: "gateway:v1:" + hex.EncodeToString(digest[:]), Source: IdentitySourceSystem,
		AuditSubject: iam.UserARN, AccountID: iam.AccountID, Internal: true,
	}, nil
}

func (r IAMIdentityResolver) resolveGatewayRole(request events.APIGatewayV2HTTPRequest) (*events.APIGatewayV2HTTPRequestContextAuthorizerIAMDescription, error) {
	if request.RequestContext.Authorizer == nil || request.RequestContext.Authorizer.IAM == nil {
		return nil, ErrUnauthorized
	}
	iam := request.RequestContext.Authorizer.IAM
	roleARN, _, err := assumedRole(iam.UserARN, iam.AccountID)
	if err != nil || r.GatewayRoleARN == "" || roleARN != r.GatewayRoleARN {
		return nil, ErrUnauthorized
	}
	return iam, nil
}

type HTTPHandler struct {
	Service           *Service
	Identity          IAMIdentityResolver
	VerifyGatewayOIDC func(context.Context, string) (string, string, error)
}

func (h *HTTPHandler) Handle(ctx context.Context,
	request events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	if request.RouteKey == "POST /internal/v1/gateway/control" {
		return h.handleGatewayControl(ctx, request), nil
	}
	resolve := h.Identity.Resolve
	if request.RouteKey == "POST /internal/v1/environments/{id}/activity" {
		resolve = h.Identity.ResolveActivity
	}
	identity, err := resolve(request)
	if err != nil {
		return errorResponse(err), nil
	}
	if h.Service == nil {
		return errorResponse(errors.New("service is not configured")), nil
	}
	id := request.PathParameters["id"]
	switch request.RouteKey {
	case "POST /v1/source-uploads":
		var upload SourceUpload
		upload, err = h.Service.SourceUpload(ctx, identity)
		if err == nil {
			return jsonResponse(http.StatusCreated, upload), nil
		}
	case "POST /v1/environments":
		var body CreateRequest
		if err = decodeBody(request.Body, &body); err == nil {
			var result MutationResult
			result, err = h.Service.Create(ctx, identity, body, header(request.Headers, "Idempotency-Key"))
			if err == nil {
				return jsonResponse(http.StatusAccepted, result), nil
			}
		}
	case "GET /v1/environments":
		var environments []Environment
		environments, err = h.Service.List(ctx, identity)
		if err == nil {
			return jsonResponse(http.StatusOK, map[string]any{"environments": environments}), nil
		}
	case "GET /v1/environments/{id}":
		var environment Environment
		environment, err = h.Service.Get(ctx, identity, id)
		if err == nil {
			return jsonResponse(http.StatusOK, environment), nil
		}
	case "POST /v1/environments/{id}/start":
		var body StartRequest
		if err = decodeOptionalBody(request.Body, &body); err == nil {
			var result MutationResult
			result, err = h.Service.Start(ctx, identity, id, body, header(request.Headers, "Idempotency-Key"))
			if err == nil {
				return jsonResponse(http.StatusAccepted, result), nil
			}
		}
	case "POST /v1/environments/{id}/extend":
		var body ExtendRequest
		if err = decodeBody(request.Body, &body); err == nil {
			var result MutationResult
			result, err = h.Service.Extend(ctx, identity, id, body, header(request.Headers, "Idempotency-Key"))
			if err == nil {
				return jsonResponse(http.StatusOK, result), nil
			}
		}
	case "POST /v1/environments/{id}/stop":
		var result MutationResult
		result, err = h.Service.Stop(ctx, identity, id, header(request.Headers, "Idempotency-Key"))
		if err == nil {
			return jsonResponse(http.StatusAccepted, result), nil
		}
	case "POST /v1/environments/{id}/archive":
		var body ArchiveRequest
		if err = decodeOptionalBody(request.Body, &body); err == nil {
			var result MutationResult
			result, err = h.Service.Archive(ctx, identity, id, body, header(request.Headers, "Idempotency-Key"))
			if err == nil {
				return jsonResponse(http.StatusAccepted, result), nil
			}
		}
	case "DELETE /v1/environments/{id}":
		var result MutationResult
		result, err = h.Service.Delete(ctx, identity, id, header(request.Headers, "Idempotency-Key"))
		if err == nil {
			return jsonResponse(http.StatusAccepted, result), nil
		}
	case "GET /v1/operations/{id}":
		var operation Operation
		operation, err = h.Service.Operation(ctx, identity, id)
		if err == nil {
			return jsonResponse(http.StatusOK, operation), nil
		}
	case "GET /v1/checkpoints":
		var checkpoints []Checkpoint
		checkpoints, err = h.Service.Checkpoints(ctx, identity)
		if err == nil {
			return jsonResponse(http.StatusOK, map[string]any{"checkpoints": checkpoints}), nil
		}
	case "DELETE /v1/checkpoints/{id}":
		var operation Operation
		operation, err = h.Service.DeleteCheckpoint(ctx, identity, id, header(request.Headers, "Idempotency-Key"))
		if err == nil {
			return jsonResponse(http.StatusAccepted, operation), nil
		}
	case "POST /v1/environments/{id}/browser-link":
		var link BrowserLink
		link, err = h.Service.BrowserLink(ctx, identity, id)
		if err == nil {
			return jsonResponse(http.StatusOK, link), nil
		}
	case "POST /internal/v1/environments/{id}/activity":
		var body ActivityRequest
		if err = decodeBody(request.Body, &body); err == nil {
			var environment Environment
			environment, err = h.Service.Activity(ctx, identity, id, body)
			if err == nil {
				return jsonResponse(http.StatusOK, environment), nil
			}
		}
	case "POST /internal/v1/reconcile":
		var body struct {
			Limit int32 `json:"limit,omitempty"`
		}
		if err = decodeOptionalBody(request.Body, &body); err == nil {
			var result ReconcileResult
			result, err = h.Service.Reconcile(ctx, identity, body.Limit)
			if err == nil {
				return jsonResponse(http.StatusOK, result), nil
			}
		}
	default:
		err = ErrNotFound
	}
	response := errorResponse(err)
	if response.StatusCode == http.StatusInternalServerError {
		log.Printf("request failed: route=%q: %v", request.RouteKey, err)
	}
	return response, nil
}

type gatewayControlRequest struct {
	ActorEmail    string `json:"actorEmail"`
	AuditSubject  string `json:"auditSubject,omitempty"`
	Action        string `json:"action"`
	EnvironmentID string `json:"environmentId,omitempty"`
	CheckpointID  string `json:"checkpointId,omitempty"`
	Hours         int    `json:"hours,omitempty"`
	OIDCData      string `json:"oidcData,omitempty"`
}

func (h *HTTPHandler) handleGatewayControl(ctx context.Context,
	request events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	if h.Service == nil {
		return errorResponse(errors.New("service is not configured"))
	}
	var body gatewayControlRequest
	if err := decodeBody(request.Body, &body); err != nil {
		return errorResponse(err)
	}
	var identity Identity
	var err error
	if body.Action == "activity" {
		identity, err = h.Identity.ResolveGatewaySystem(request)
	} else {
		if _, roleErr := h.Identity.resolveGatewayRole(request); roleErr != nil || h.VerifyGatewayOIDC == nil {
			return errorResponse(ErrUnauthorized)
		}
		email, subject, verifyErr := h.VerifyGatewayOIDC(ctx, body.OIDCData)
		if verifyErr != nil {
			return errorResponse(ErrUnauthorized)
		}
		identity, err = h.Identity.ResolveGatewayActor(request, email, subject)
	}
	if err != nil {
		return errorResponse(err)
	}
	key := header(request.Headers, "Idempotency-Key")
	switch body.Action {
	case "list":
		environments, callErr := h.Service.List(ctx, identity)
		if callErr != nil {
			return errorResponse(callErr)
		}
		return jsonResponse(http.StatusOK, map[string]any{"environments": environments})
	case "get":
		environment, callErr := h.Service.Get(ctx, identity, body.EnvironmentID)
		if callErr != nil {
			return errorResponse(callErr)
		}
		return jsonResponse(http.StatusOK, environment)
	case "checkpoints":
		checkpoints, callErr := h.Service.Checkpoints(ctx, identity)
		if callErr != nil {
			return errorResponse(callErr)
		}
		return jsonResponse(http.StatusOK, map[string]any{"checkpoints": checkpoints})
	case "start":
		result, callErr := h.Service.Start(ctx, identity, body.EnvironmentID, StartRequest{}, key)
		return gatewayMutationResponse(result, callErr)
	case "extend":
		result, callErr := h.Service.Extend(ctx, identity, body.EnvironmentID, ExtendRequest{Hours: body.Hours}, key)
		return gatewayMutationResponse(result, callErr)
	case "stop":
		result, callErr := h.Service.Stop(ctx, identity, body.EnvironmentID, key)
		return gatewayMutationResponse(result, callErr)
	case "archive":
		result, callErr := h.Service.Archive(ctx, identity, body.EnvironmentID, ArchiveRequest{}, key)
		return gatewayMutationResponse(result, callErr)
	case "delete":
		result, callErr := h.Service.Delete(ctx, identity, body.EnvironmentID, key)
		return gatewayMutationResponse(result, callErr)
	case "delete-checkpoint":
		operation, callErr := h.Service.DeleteCheckpoint(ctx, identity, body.CheckpointID, key)
		if callErr != nil {
			return errorResponse(callErr)
		}
		return jsonResponse(http.StatusAccepted, operation)
	case "browser-link":
		link, callErr := h.Service.BrowserLink(ctx, identity, body.EnvironmentID)
		if callErr != nil {
			return errorResponse(callErr)
		}
		return jsonResponse(http.StatusOK, link)
	case "activity":
		environment, callErr := h.Service.Activity(ctx, identity, body.EnvironmentID,
			ActivityRequest{Kind: "preview"})
		if callErr != nil {
			return errorResponse(callErr)
		}
		return jsonResponse(http.StatusOK, environment)
	default:
		return errorResponse(&FieldError{Field: "action", Err: errors.New("unsupported gateway action")})
	}
}

func gatewayMutationResponse(result MutationResult, err error) events.APIGatewayV2HTTPResponse {
	if err != nil {
		return errorResponse(err)
	}
	return jsonResponse(http.StatusAccepted, result)
}

func assumedRole(rawARN, accountID string) (string, string, error) {
	prefix := "arn:aws:sts::" + accountID + ":assumed-role/"
	if !strings.HasPrefix(rawARN, prefix) {
		return "", "", errors.New("not an assumed role")
	}
	parts := strings.Split(strings.TrimPrefix(rawARN, prefix), "/")
	if len(parts) < 2 {
		return "", "", errors.New("invalid assumed-role ARN")
	}
	session := parts[len(parts)-1]
	role := strings.Join(parts[:len(parts)-1], "/")
	if role == "" || session == "" {
		return "", "", errors.New("invalid assumed-role ARN")
	}
	return "arn:aws:iam::" + accountID + ":role/" + role, session, nil
}

func decodeBody(raw string, target any) error {
	if raw == "" {
		return &FieldError{Field: "body", Err: io.EOF}
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &FieldError{Field: "body", Err: err}
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return &FieldError{Field: "body", Err: errors.New("multiple JSON values")}
	}
	return nil
}

func decodeOptionalBody(raw string, target any) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return decodeBody(raw, target)
}

func header(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func jsonResponse(status int, value any) events.APIGatewayV2HTTPResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return errorResponse(err)
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status, Headers: map[string]string{
			"content-type": "application/json", "cache-control": "no-store",
		}, Body: string(body),
	}
}

func errorResponse(err error) events.APIGatewayV2HTTPResponse {
	status, code, message := http.StatusInternalServerError, "internal", "internal error"
	switch {
	case errors.Is(err, ErrUnauthorized):
		status, code, message = http.StatusUnauthorized, "unauthorized", "unauthorized"
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, ErrQuotaExceeded):
		status, code, message = http.StatusTooManyRequests, "quota_exceeded", err.Error()
	case errors.Is(err, ErrIdempotency):
		status, code, message = http.StatusConflict, "idempotency_key_reused", err.Error()
	case errors.Is(err, ErrConflict), errors.Is(err, ErrNotReady):
		status, code, message = http.StatusConflict, "conflict", err.Error()
	default:
		var field *FieldError
		if errors.As(err, &field) {
			status, code, message = http.StatusBadRequest, "invalid_argument", field.Error()
		}
	}
	return jsonResponseRaw(status, map[string]string{"code": code, "message": message})
}

func jsonResponseRaw(status int, value any) events.APIGatewayV2HTTPResponse {
	body, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		body = []byte(`{"code":"internal","message":"internal error"}`)
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"content-type": "application/json", "cache-control": "no-store"},
		Body:       string(body),
	}
}
