package devenv

import "testing"

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
