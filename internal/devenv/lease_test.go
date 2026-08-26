package devenv

import (
	"testing"
	"time"
)

func TestNewLeaseDefaults(t *testing.T) {
	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.FixedZone("ICT", 7*60*60))
	lease := NewLease(now)
	utc := now.UTC()
	if got, want := lease.IdleExpiresAt, utc.Add(4*time.Hour); !got.Equal(want) {
		t.Fatalf("idle deadline = %s, want %s", got, want)
	}
	if got, want := lease.HardExpiresAt, utc.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("hard deadline = %s, want %s", got, want)
	}
	if lease.Version != 1 {
		t.Fatalf("version = %d, want 1", lease.Version)
	}
}

func TestLeaseExtend(t *testing.T) {
	now := time.Date(2026, time.August, 27, 3, 0, 0, 0, time.UTC)
	lease := NewLease(now)
	extended, err := lease.Extend(now.Add(time.Hour), 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := extended.IdleExpiresAt, now.Add(8*time.Hour); !got.Equal(want) {
		t.Fatalf("deadline = %s, want %s", got, want)
	}
	if extended.Version != 2 {
		t.Fatalf("version = %d, want 2", extended.Version)
	}
}

func TestLeaseExtendCapsAtHardDeadline(t *testing.T) {
	now := time.Date(2026, time.August, 27, 3, 0, 0, 0, time.UTC)
	lease := NewLease(now)
	lease.IdleExpiresAt = lease.HardExpiresAt.Add(-time.Hour)
	extended, err := lease.Extend(now.Add(2*time.Hour), 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !extended.IdleExpiresAt.Equal(lease.HardExpiresAt) {
		t.Fatalf("deadline = %s, want hard deadline %s", extended.IdleExpiresAt, lease.HardExpiresAt)
	}
}

func TestLeaseRejectsInvalidExtension(t *testing.T) {
	lease := NewLease(time.Now())
	for _, extension := range []time.Duration{0, 30 * time.Minute, 5 * time.Hour} {
		if _, err := lease.Extend(time.Now(), extension); err == nil {
			t.Fatalf("expected %s extension to fail", extension)
		}
	}
}

func TestActivityNeverShortensExplicitExtension(t *testing.T) {
	now := time.Date(2026, time.August, 27, 3, 0, 0, 0, time.UTC)
	lease := NewLease(now)
	lease.IdleExpiresAt = now.Add(12 * time.Hour)
	updated, err := lease.RecordActivity(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.IdleExpiresAt.Equal(lease.IdleExpiresAt) {
		t.Fatalf("activity shortened deadline from %s to %s", lease.IdleExpiresAt, updated.IdleExpiresAt)
	}
}

func TestRetentionDeadlines(t *testing.T) {
	now := time.Date(2026, time.August, 27, 3, 0, 0, 0, time.UTC)
	if got := ArchiveDeadline(now); !got.Equal(now.Add(7 * 24 * time.Hour)) {
		t.Fatalf("archive deadline = %s", got)
	}
	if got := CheckpointDeadline(now); !got.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("checkpoint deadline = %s", got)
	}
}
