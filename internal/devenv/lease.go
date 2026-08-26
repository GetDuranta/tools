package devenv

import (
	"errors"
	"time"
)

func NewLease(now time.Time) Lease {
	now = now.UTC()
	return Lease{IdleExpiresAt: now.Add(DefaultIdleTTL), HardExpiresAt: now.Add(DefaultHardTTL), Version: 1}
}

func (l Lease) DueAt() time.Time {
	if l.HardExpiresAt.Before(l.IdleExpiresAt) {
		return l.HardExpiresAt
	}
	return l.IdleExpiresAt
}

func (e Environment) ScheduledActionAt() time.Time {
	dueAt := e.Lease.DueAt()
	if e.State == StateStopped && e.ArchiveAfter != nil {
		return *e.ArchiveAfter
	}
	if e.State == StateError {
		if e.RecoveryRetryAfter != nil {
			return *e.RecoveryRetryAfter
		}
		if e.CleanupAfter != nil {
			return *e.CleanupAfter
		}
	}
	return dueAt
}

func (l Lease) Extend(now time.Time, extension time.Duration) (Lease, error) {
	if extension < MinExtension || extension > MaxExtension || extension%time.Hour != 0 {
		return Lease{}, errors.New("extension must be a whole number of hours from 1 through 4")
	}
	now = now.UTC()
	if !now.Before(l.HardExpiresAt) {
		return Lease{}, errors.New("hard lease deadline has passed")
	}
	base := l.IdleExpiresAt
	if now.After(base) {
		base = now
	}
	l.IdleExpiresAt = base.Add(extension)
	if l.IdleExpiresAt.After(l.HardExpiresAt) {
		l.IdleExpiresAt = l.HardExpiresAt
	}
	l.Version++
	return l, nil
}

func (l Lease) RecordActivity(now time.Time) (Lease, error) {
	now = now.UTC()
	if !now.Before(l.HardExpiresAt) {
		return Lease{}, errors.New("hard lease deadline has passed")
	}
	candidate := now.Add(DefaultIdleTTL)
	if candidate.After(l.HardExpiresAt) {
		candidate = l.HardExpiresAt
	}
	if candidate.After(l.IdleExpiresAt) {
		l.IdleExpiresAt = candidate
	}
	l.Version++
	return l, nil
}

func ArchiveDeadline(stoppedAt time.Time) time.Time {
	return stoppedAt.UTC().Add(DefaultStoppedTTL)
}

func CheckpointDeadline(createdAt time.Time) time.Time {
	return createdAt.UTC().Add(DefaultCheckpointTTL)
}
