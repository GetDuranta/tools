package devenv

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type noCallStore struct{ Store }

func TestZeroGPUQuotaDisablesGPUProfile(t *testing.T) {
	limits := DefaultQuotaLimits()
	limits.MaxGPURunning = 0
	service := NewService(nil, nil, nil, nil)
	if err := service.SetQuotaLimits(limits); err != nil {
		t.Fatal(err)
	}
	if !service.profileEnabled(ProfileStandard) || service.profileEnabled(ProfileGPUCVML) {
		t.Fatal("zero GPU quota did not disable only the GPU profile")
	}
	limits.MaxGPURunning = -1
	if err := limits.Validate(); err == nil {
		t.Fatal("negative GPU quota was accepted")
	}
}

func TestZeroGPUQuotaRejectsFirstStoreReservation(t *testing.T) {
	limits := DefaultQuotaLimits()
	limits.MaxGPURunning = 0
	transaction := globalQuotaTransaction(1, 1, 1, 0, 0, limits)
	condition := aws.ToString(transaction.Update.ConditionExpression)
	if !strings.Contains(condition, "attribute_exists(gpu_running) AND attribute_not_exists(gpu_running)") {
		t.Fatalf("zero GPU quota is not fail-closed: %s", condition)
	}
}

func TestCreateRejectsDisabledGPUProfileBeforeStore(t *testing.T) {
	limits := DefaultQuotaLimits()
	limits.MaxGPURunning = 0
	service := NewService(&noCallStore{}, nil, nil, nil)
	if err := service.SetQuotaLimits(limits); err != nil {
		t.Fatal(err)
	}
	_, err := service.Create(context.Background(), Identity{
		PrincipalID: "owner-1", Source: IdentitySourceAWSIAM,
	}, CreateRequest{
		Name: "gpu-workspace", Profile: ProfileGPUCVML, Visibility: VisibilityRestricted,
		Source: Source{Repository: "repo", Ref: "main"},
	}, "create-gpu-workspace")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("disabled GPU create returned %v", err)
	}
}

func TestStartRejectsDisabledArchivedGPUProfileBeforeBegin(t *testing.T) {
	limits := DefaultQuotaLimits()
	limits.MaxGPURunning = 0
	store := &lifecycleStore{env: Environment{
		ID: "env-1", OwnerID: "owner-1", State: StateArchived, Profile: ProfileGPUCVML,
	}}
	service := NewService(store, nil, nil, nil)
	if err := service.SetQuotaLimits(limits); err != nil {
		t.Fatal(err)
	}
	_, err := service.Start(context.Background(), Identity{
		PrincipalID: "owner-1", Source: IdentitySourceAWSIAM,
	}, "env-1", StartRequest{}, "start-gpu-workspace")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("disabled GPU start returned %v", err)
	}
	if store.beginMutation.Operation.ID != "" {
		t.Fatalf("disabled GPU start reached store Begin: %+v", store.beginMutation)
	}
}
