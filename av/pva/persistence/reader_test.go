package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// seedFrames inserts test data directly via the writer and waits for flush.
func seedFrames(t *testing.T, w *Writer, channelID string, count int, baseTime time.Time) {
	t.Helper()

	for i := range count {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		a := &av.FrameAnalytics{
			SiteID:       1,
			ChannelID:    10,
			FramePTS:     int64(i),
			CaptureMS:    ts.UnixMilli(),
			CaptureEndMS: ts.Add(33 * time.Millisecond).UnixMilli(),
			RefWidth:     1920,
			RefHeight:    1080,
			VehicleCount: int32(i % 3),
			PeopleCount:  int32(i % 5),
			Objects: []*av.Detection{
				{X: 100, Y: 200, W: 50, H: 80, ClassID: 0, Confidence: 90, TrackID: int64(i)},
				{
					X: 400, Y: 300, W: 100, H: 60, ClassID: 1, Confidence: 70, TrackID: int64(i + 100),
					IsEvent: i%2 == 0,
				},
			},
		}
		w.Enqueue(channelID, ts, a)
	}

	time.Sleep(2 * time.Second) // wait for flush
}

func TestReader_QueryFrames_Pagination(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	w := NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	now := time.Now().UTC()
	seedFrames(t, w, "cam-1", 10, now)

	reader := NewReader(dbm)
	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	// First page.
	frames, total, err := reader.QueryFrames(context.Background(), "cam-1", from, to,
		QueryOpts{Limit: 3, Offset: 0})
	if err != nil {
		t.Fatalf("QueryFrames: %v", err)
	}

	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}

	if len(frames) != 3 {
		t.Errorf("page size = %d, want 3", len(frames))
	}

	// Second page.
	frames2, total2, err := reader.QueryFrames(context.Background(), "cam-1", from, to,
		QueryOpts{Limit: 3, Offset: 3})
	if err != nil {
		t.Fatalf("QueryFrames page 2: %v", err)
	}

	if total2 != 10 {
		t.Errorf("total page 2 = %d, want 10", total2)
	}

	if len(frames2) != 3 {
		t.Errorf("page 2 size = %d, want 3", len(frames2))
	}

	// Frames should be different.
	if frames[0].ID == frames2[0].ID {
		t.Error("page 1 and page 2 returned same first frame")
	}
}

func TestReader_QueryFrames_ClassFilter(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	w := NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	now := time.Now().UTC()
	seedFrames(t, w, "cam-1", 5, now)

	reader := NewReader(dbm)
	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	// Filter by classID=0 (person).
	classID := 0
	frames, total, err := reader.QueryFrames(context.Background(), "cam-1", from, to,
		QueryOpts{ClassID: &classID})
	if err != nil {
		t.Fatalf("QueryFrames classID filter: %v", err)
	}

	// All frames have a classID=0 detection.
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}

	if len(frames) != 5 {
		t.Errorf("len = %d, want 5", len(frames))
	}
}

func TestReader_CountsByInterval(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	w := NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	now := time.Now().UTC()
	seedFrames(t, w, "cam-1", 10, now)

	reader := NewReader(dbm)
	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	buckets, err := reader.CountsByInterval(context.Background(), "cam-1", from, to, 60)
	if err != nil {
		t.Fatalf("CountsByInterval: %v", err)
	}

	if len(buckets) == 0 {
		t.Fatal("expected at least 1 bucket")
	}

	// All 10 frames are within a ~10s window, so should be in 1 bucket at 60s interval.
	totalFrames := 0
	for _, b := range buckets {
		totalFrames += b.FrameCount
	}

	if totalFrames != 10 {
		t.Errorf("total frames across buckets = %d, want 10", totalFrames)
	}
}

func TestReader_SearchByTrackID(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	w := NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	now := time.Now().UTC()
	seedFrames(t, w, "cam-1", 5, now)

	reader := NewReader(dbm)
	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	// Track ID 0 is in frame 0 (first detection of seedFrames).
	frames, total, err := reader.SearchByTrackID(context.Background(), "cam-1", 0, from, to, QueryOpts{})
	if err != nil {
		t.Fatalf("SearchByTrackID: %v", err)
	}

	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}

	if len(frames) != 1 {
		t.Errorf("len = %d, want 1", len(frames))
	}
}

func TestReader_SearchEvents(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	w := NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	now := time.Now().UTC()
	seedFrames(t, w, "cam-1", 10, now)

	reader := NewReader(dbm)
	from := now.Add(-time.Minute)
	to := now.Add(time.Minute)

	frames, total, err := reader.SearchEvents(context.Background(), "cam-1", from, to, QueryOpts{})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}

	// In seedFrames, every even-indexed frame has IsEvent=true on the second detection.
	if total != 5 {
		t.Errorf("total = %d, want 5 (even-indexed frames)", total)
	}

	if len(frames) != 5 {
		t.Errorf("len = %d, want 5", len(frames))
	}

	for _, f := range frames {
		if !f.HasEvent {
			t.Errorf("frame %d has HasEvent=false, want true", f.ID)
		}
	}
}

func TestReader_EmptyChannel(t *testing.T) {
	dir := t.TempDir()
	dbm := NewDBManager(dir)
	defer dbm.Close()

	reader := NewReader(dbm)
	now := time.Now().UTC()

	frames, total, err := reader.QueryFrames(context.Background(), "nonexistent",
		now.Add(-time.Hour), now, QueryOpts{})
	if err != nil {
		t.Fatalf("QueryFrames empty: %v", err)
	}

	if total != 0 || len(frames) != 0 {
		t.Errorf("expected empty result, got total=%d len=%d", total, len(frames))
	}
}
