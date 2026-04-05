package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

func TestWriter_EnqueueAndFlush(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	w := NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	now := time.Now().UTC()
	a := &av.FrameAnalytics{
		SiteID:       1,
		ChannelID:    10,
		FramePTS:     12345,
		CaptureMS:    now.UnixMilli(),
		CaptureEndMS: now.Add(33 * time.Millisecond).UnixMilli(),
		InferenceMS:  now.Add(50 * time.Millisecond).UnixMilli(),
		RefWidth:     1920,
		RefHeight:    1080,
		VehicleCount: 2,
		PeopleCount:  3,
		Objects: []*av.Detection{
			{X: 100, Y: 200, W: 50, H: 80, ClassID: 0, Confidence: 95, TrackID: 1, IsEvent: false},
			{X: 400, Y: 300, W: 100, H: 60, ClassID: 1, Confidence: 87, TrackID: 2, IsEvent: true},
		},
	}

	w.Enqueue("cam-1", now, a)

	// Wait for flush (flushInterval = 1s, give some margin).
	time.Sleep(2 * time.Second)

	// Verify data was persisted using a Reader.
	reader := NewReader(dbm)

	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	frames, total, err := reader.QueryFrames(context.Background(), "cam-1", from, to, QueryOpts{})
	if err != nil {
		t.Fatalf("QueryFrames: %v", err)
	}

	if total != 1 {
		t.Fatalf("expected 1 frame, got %d", total)
	}

	f := frames[0]

	if f.SiteID != 1 {
		t.Errorf("SiteID = %d, want 1", f.SiteID)
	}

	if f.VehicleCount != 2 {
		t.Errorf("VehicleCount = %d, want 2", f.VehicleCount)
	}

	if f.PeopleCount != 3 {
		t.Errorf("PeopleCount = %d, want 3", f.PeopleCount)
	}

	if f.ObjectCount != 2 {
		t.Errorf("ObjectCount = %d, want 2", f.ObjectCount)
	}

	if !f.HasEvent {
		t.Error("HasEvent = false, want true")
	}

	if len(f.Detections) != 2 {
		t.Fatalf("expected 2 detections, got %d", len(f.Detections))
	}

	d := f.Detections[0]
	if d.ClassID != 0 || d.Confidence != 95 || d.TrackID != 1 {
		t.Errorf("detection[0] unexpected: classID=%d confidence=%d trackID=%d",
			d.ClassID, d.Confidence, d.TrackID)
	}

	d1 := f.Detections[1]
	if !d1.IsEvent {
		t.Error("detection[1] IsEvent = false, want true")
	}
}

func TestWriter_MultipleChannels(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	w := NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	now := time.Now().UTC()

	for i := range 5 {
		a := &av.FrameAnalytics{
			SiteID:    1,
			ChannelID: int32(i),
			CaptureMS: now.Add(time.Duration(i) * time.Second).UnixMilli(),
			FramePTS:  int64(i),
		}
		w.Enqueue("cam-1", now.Add(time.Duration(i)*time.Second), a)
	}

	for i := range 3 {
		a := &av.FrameAnalytics{
			SiteID:    1,
			ChannelID: int32(100 + i),
			CaptureMS: now.Add(time.Duration(i) * time.Second).UnixMilli(),
			FramePTS:  int64(i),
		}
		w.Enqueue("cam-2", now.Add(time.Duration(i)*time.Second), a)
	}

	time.Sleep(2 * time.Second)

	reader := NewReader(dbm)
	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	frames1, total1, err := reader.QueryFrames(context.Background(), "cam-1", from, to, QueryOpts{})
	if err != nil {
		t.Fatalf("QueryFrames cam-1: %v", err)
	}

	if total1 != 5 {
		t.Errorf("cam-1: expected 5 frames, got %d", total1)
	}

	if len(frames1) != 5 {
		t.Errorf("cam-1: expected 5 frame items, got %d", len(frames1))
	}

	frames2, total2, err := reader.QueryFrames(context.Background(), "cam-2", from, to, QueryOpts{})
	if err != nil {
		t.Fatalf("QueryFrames cam-2: %v", err)
	}

	if total2 != 3 {
		t.Errorf("cam-2: expected 3 frames, got %d", total2)
	}

	if len(frames2) != 3 {
		t.Errorf("cam-2: expected 3 frame items, got %d", len(frames2))
	}
}

func TestWriter_Prune(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	// Very short retention for testing.
	w := NewWriter(dbm, 1*time.Second, 1_000_000)
	defer w.Close()

	old := time.Now().UTC().Add(-5 * time.Second)
	a := &av.FrameAnalytics{
		SiteID:    1,
		ChannelID: 1,
		CaptureMS: old.UnixMilli(),
		FramePTS:  1,
		Objects: []*av.Detection{
			{X: 10, Y: 20, W: 30, H: 40, ClassID: 0, Confidence: 90},
		},
	}

	w.Enqueue("cam-1", old, a)
	time.Sleep(2 * time.Second)

	// Manually trigger prune (normally runs every 10min).
	w.prune(context.Background())

	reader := NewReader(dbm)
	from := old.Add(-time.Minute)
	to := time.Now().UTC().Add(time.Minute)

	_, total, err := reader.QueryFrames(context.Background(), "cam-1", from, to, QueryOpts{})
	if err != nil {
		t.Fatalf("QueryFrames: %v", err)
	}

	if total != 0 {
		t.Errorf("expected 0 frames after prune, got %d", total)
	}
}
