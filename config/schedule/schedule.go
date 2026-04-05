// Package schedule provides types and interfaces for reading recording
// schedules from pluggable sources (JSON file, database, etc.).
package schedule

import (
	"context"
	"slices"
	"time"
)

// Schedule describes one recording directive: which channel to record,
// when to record it, where to store segments, and how long each segment is.
type Schedule struct {
	ID             string         `json:"id"`
	ChannelID      string         `json:"channelId"`
	StoragePath    string         `json:"storagePath"`
	SegmentMinutes int            `json:"segmentMinutes"`
	SegmentSizeMB  int            `json:"segmentSizeMb"`
	StartAt        time.Time      `json:"startAt"`
	EndAt          time.Time      `json:"endAt"`
	DaysOfWeek     []time.Weekday `json:"daysOfWeek"`

	// Storage I/O tuning.
	StorageProfile string `json:"storageProfile"`

	// Multi-tier retention — segments are retained for the longest applicable tier.
	// Set all to 0 to disable time-based retention (only MaxStorageGB applies).
	ContinuousDays int `json:"continuousDays"`
	MotionDays     int `json:"motionDays"`
	ObjectDays     int `json:"objectDays"`

	// Storage limits.
	MaxAgeDays   int     `json:"maxAgeDays"`
	MaxStorageGB float64 `json:"maxStorageGb"`

	// Disk-full protection.
	MinFreeGB float64 `json:"minFreeGb"`
	LowFreeGB float64 `json:"lowFreeGb"`

	// Near-live playback RAM cache.
	RingBufferSeconds int `json:"ringBufferSeconds"`
}

// ScheduleProvider is the single interface all schedule sources must satisfy.
// Implementations are expected to be safe for concurrent use.
type ScheduleProvider interface {
	// ListSchedules returns all schedules known to this provider.
	ListSchedules(ctx context.Context) ([]Schedule, error)

	// Close releases any held resources.
	Close() error
}

// IsActive reports whether s should be recording at time now.
// It checks the optional StartAt/EndAt window and the DaysOfWeek constraint.
func IsActive(s Schedule, now time.Time) bool {
	if !s.StartAt.IsZero() && now.Before(s.StartAt) {
		return false
	}

	if !s.EndAt.IsZero() && !now.Before(s.EndAt) {
		return false
	}

	if len(s.DaysOfWeek) == 0 {
		return true
	}

	today := now.Weekday()

	return slices.Contains(s.DaysOfWeek, today)
}
