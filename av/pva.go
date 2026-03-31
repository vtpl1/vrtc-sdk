package av

import (
	"encoding/json"
)

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
type FrameAnalytics struct {
	// ── Source identity ───────────────────────────────────────────────
	SiteID    int32 `bson:"site_id"    json:"siteId"`
	ChannelID int32 `bson:"channel_id" json:"channelId"`

	// ── Frame correlation ────────────────────────────────────────────
	FramePTS     int64 `bson:"frame_pts"      json:"framePts"`     // camera PTS ticks — matches Packet.FrameID
	CaptureMS    int64 `bson:"capture_ms"     json:"captureMs"`    // wall-clock ms when frame was captured
	CaptureEndMS int64 `bson:"capture_end_ms" json:"captureEndMs"` // wall-clock ms end of capture window
	InferenceMS  int64 `bson:"inference_ms"   json:"inferenceMs"`  // wall-clock ms when inference completed

	// ── Reference frame dimensions (for normalizing bbox coords) ────
	RefWidth  int32 `bson:"ref_width"  json:"refWidth"`
	RefHeight int32 `bson:"ref_height" json:"refHeight"`

	// ── Aggregate counts ─────────────────────────────────────────────
	VehicleCount int32 `bson:"vehicle_count" json:"vehicleCount"`
	PeopleCount  int32 `bson:"people_count"  json:"peopleCount"`

	// ── Detections ───────────────────────────────────────────────────
	Objects []*Detection `bson:"objects" json:"objects,omitempty"`
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
