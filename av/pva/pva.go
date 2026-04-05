// Package pva defines per-frame analytics types produced by object-detection
// pipelines and attached to av.Packet.Analytics in the streaming pipeline.
package pva

import (
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// Detection is an alias for av.Detection.
type Detection = av.Detection

// FrameAnalytics is an alias for av.FrameAnalytics.
// Callers may use either pva.FrameAnalytics or av.FrameAnalytics — they are the same type.
type FrameAnalytics = av.FrameAnalytics

// Source is the interface implemented by any component that can supply
// FrameAnalytics for a given frame. The merger calls Fetch on every packet;
// return nil when no analytics are available for that frame.
type Source interface {
	Fetch(frameID int64, wallClock time.Time) *FrameAnalytics
}

// NilSource is a Source that always returns nil.
// Use it when analytics are not connected.
type NilSource struct{}

func (NilSource) Fetch(_ int64, _ time.Time) *FrameAnalytics { return nil }
