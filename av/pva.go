package av

import (
	"encoding/json"
	"time"
)

// RFC3339Milli is the time layout used for all wall-clock JSON fields.
// It forces exactly three fractional digits so every timestamp is a fixed width.
const RFC3339Milli = "2006-01-02T15:04:05.000Z07:00"

// formatMilliTime converts a Unix-millisecond timestamp to an RFC3339Milli
// string. Returns "" for zero (no capture / no inference).
func formatMilliTime(ms int64) string {
	if ms == 0 {
		return ""
	}

	return time.UnixMilli(ms).UTC().Format(RFC3339Milli)
}

// parseMilliTime converts an RFC3339Milli string back to Unix milliseconds.
// Returns 0 for empty or unparseable values.
func parseMilliTime(s string) int64 {
	if s == "" {
		return 0
	}

	t, err := time.Parse(RFC3339Milli, s)
	if err != nil {
		return 0
	}

	return t.UnixMilli()
}

// Detection represents a single detected object within a video frame.
type Detection struct {
	X          uint32 `bson:"x"          json:"x"`          // bounding box left (pixels)
	Y          uint32 `bson:"y"          json:"y"`          // bounding box top (pixels)
	W          uint32 `bson:"w"          json:"w"`          // bounding box width (pixels)
	H          uint32 `bson:"h"          json:"h"`          // bounding box height (pixels)
	ClassID    uint32 `bson:"class_id"   json:"classId"`    // object class (person=0, vehicle=1, ...)
	Confidence uint32 `bson:"confidence" json:"confidence"` // detection confidence 0–100
	TrackID    int64  `bson:"track_id"   json:"trackId"`    // cross-frame tracking identity
	IsEvent    bool   `bson:"is_event"   json:"isEvent"`    // triggered an alert rule
}

// FrameAnalytics carries per-frame analytics results produced by object-detection,
// face-recognition, and license-plate-recognition pipelines. Attached to
// av.Packet.Analytics; nil means no analytics for that frame.
//
// Correlation: FramePTS matches av.Packet.FrameID (camera PTS ticks).
// CaptureMS matches av.Packet.WallClockTime for temporal alignment.
//
// Timing precision: timing fields are stored internally as Unix milliseconds
// (int64). Millisecond resolution is sufficient — video frames arrive at 25–30 fps
// (~33 ms apart) and the AnalyticsStore match tolerance is 200 ms.
//
// JSON wire format: timing fields are serialised as RFC3339 with millisecond
// precision (av.RFC3339Milli), e.g. "2025-01-15T12:34:56.789Z". BSON and proto
// continue to use int64.
type FrameAnalytics struct {
	// ── Source identity ───────────────────────────────────────────────
	SiteID    int32 `bson:"site_id"    json:"siteId"`
	ChannelID int32 `bson:"channel_id" json:"channelId"`

	// ── Frame correlation ────────────────────────────────────────────
	FramePTS     int64 `bson:"frame_pts"      json:"framePts"`             // camera PTS ticks — matches Packet.FrameID
	CaptureMS    int64 `bson:"capture_ms"     json:"capture,omitempty"`    // wall-clock ms when frame was captured
	CaptureEndMS int64 `bson:"capture_end_ms" json:"captureEnd,omitempty"` // wall-clock ms end of capture/exposure window
	InferenceMS  int64 `bson:"inference_ms"   json:"inference,omitempty"`  // wall-clock ms when inference completed (Unix ms)

	// ── Reference frame dimensions (for normalizing bbox coords) ────
	RefWidth  int32 `bson:"ref_width"  json:"refWidth"`
	RefHeight int32 `bson:"ref_height" json:"refHeight"`

	// ── Aggregate counts ─────────────────────────────────────────────
	VehicleCount int32 `bson:"vehicle_count" json:"vehicleCount"`
	PeopleCount  int32 `bson:"people_count"  json:"peopleCount"`

	// ── Detections ───────────────────────────────────────────────────
	Objects []*Detection `bson:"objects" json:"objects,omitempty"`
}

// frameAnalyticsJSON is the wire representation with RFC3339Milli timing strings.
type frameAnalyticsJSON struct {
	SiteID       int32        `json:"siteId"`
	ChannelID    int32        `json:"channelId"`
	FramePTS     int64        `json:"framePts"`
	Capture      string       `json:"capture,omitempty"`
	CaptureEnd   string       `json:"captureEnd,omitempty"`
	Inference    string       `json:"inference,omitempty"`
	RefWidth     int32        `json:"refWidth"`
	RefHeight    int32        `json:"refHeight"`
	VehicleCount int32        `json:"vehicleCount"`
	PeopleCount  int32        `json:"peopleCount"`
	Objects      []*Detection `json:"objects,omitempty"`
}

// MarshalJSON encodes FrameAnalytics with timing fields as RFC3339Milli strings.
func (a FrameAnalytics) MarshalJSON() ([]byte, error) {
	return json.Marshal(frameAnalyticsJSON{
		SiteID:       a.SiteID,
		ChannelID:    a.ChannelID,
		FramePTS:     a.FramePTS,
		Capture:      formatMilliTime(a.CaptureMS),
		CaptureEnd:   formatMilliTime(a.CaptureEndMS),
		Inference:    formatMilliTime(a.InferenceMS),
		RefWidth:     a.RefWidth,
		RefHeight:    a.RefHeight,
		VehicleCount: a.VehicleCount,
		PeopleCount:  a.PeopleCount,
		Objects:      a.Objects,
	})
}

// UnmarshalJSON decodes FrameAnalytics, parsing RFC3339Milli timing strings
// back to Unix-millisecond int64 fields.
func (a *FrameAnalytics) UnmarshalJSON(data []byte) error {
	var j frameAnalyticsJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	a.SiteID = j.SiteID
	a.ChannelID = j.ChannelID
	a.FramePTS = j.FramePTS
	a.CaptureMS = parseMilliTime(j.Capture)
	a.CaptureEndMS = parseMilliTime(j.CaptureEnd)
	a.InferenceMS = parseMilliTime(j.Inference)
	a.RefWidth = j.RefWidth
	a.RefHeight = j.RefHeight
	a.VehicleCount = j.VehicleCount
	a.PeopleCount = j.PeopleCount
	a.Objects = j.Objects

	return nil
}

// String returns a JSON representation of a, or "nil" when a is nil.
func (a *FrameAnalytics) String() string {
	if a == nil {
		return "nil"
	}

	b, err := json.Marshal(a)
	if err != nil {
		return ""
	}

	return string(b)
}
