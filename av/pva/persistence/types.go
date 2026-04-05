package persistence

import "time"

// StoredFrame represents a persisted FrameAnalytics row from the frames table.
type StoredFrame struct {
	ID           int64     `json:"id"`
	SiteID       int32     `json:"siteId"`
	ChannelID    int32     `json:"channelId"`
	FramePTS     int64     `json:"framePts"`
	CaptureMS    int64     `json:"captureMs"`
	CaptureEndMS int64     `json:"captureEndMs"`
	InferenceMS  int64     `json:"inferenceMs"`
	RefWidth     int32     `json:"refWidth"`
	RefHeight    int32     `json:"refHeight"`
	VehicleCount int32     `json:"vehicleCount"`
	PeopleCount  int32     `json:"peopleCount"`
	ObjectCount  int       `json:"objectCount"`
	HasEvent     bool      `json:"hasEvent"`
	Timestamp    time.Time `json:"timestamp"`
}

// StoredDetection represents a persisted Detection row from the detections table.
type StoredDetection struct {
	ID         int64 `json:"id"`
	FrameID    int64 `json:"frameId"`
	X          int   `json:"x"`
	Y          int   `json:"y"`
	W          int   `json:"w"`
	H          int   `json:"h"`
	ClassID    int   `json:"classId"`
	Confidence int   `json:"confidence"`
	TrackID    int64 `json:"trackId"`
	IsEvent    bool  `json:"isEvent"`
}

// FrameWithDetections is a StoredFrame together with its detections.
type FrameWithDetections struct {
	StoredFrame

	Detections []StoredDetection `json:"detections"`
}

// CountBucket holds an aggregated count for a time interval.
type CountBucket struct {
	BucketMS     int64 `json:"bucketMs"`
	FrameCount   int   `json:"frameCount"`
	VehicleCount int64 `json:"vehicleCount"`
	PeopleCount  int64 `json:"peopleCount"`
	ObjectCount  int64 `json:"objectCount"`
	EventCount   int   `json:"eventCount"`
}

// QueryOpts provides optional filters for analytics queries.
type QueryOpts struct {
	Limit         int
	Offset        int
	ClassID       *int
	MinConfidence *int
	EventsOnly    bool
}
