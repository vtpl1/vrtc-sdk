package pva

import (
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/pva/persistence"
)

// seedAnalytics inserts test analytics via the persistence Writer and waits
// for the flush to complete.
func seedAnalytics(t *testing.T, w *persistence.Writer, channelID string, count int, baseTime time.Time) {
	t.Helper()

	for i := range count {
		ts := baseTime.Add(time.Duration(i) * 500 * time.Millisecond)
		a := &av.FrameAnalytics{
			SiteID:       1,
			ChannelID:    10,
			FramePTS:     int64(i),
			CaptureMS:    ts.UnixMilli(),
			CaptureEndMS: ts.Add(33 * time.Millisecond).UnixMilli(),
			RefWidth:     1920,
			RefHeight:    1080,
			VehicleCount: int32(i % 3),
			PeopleCount:  int32(i + 1),
			Objects: []*av.Detection{
				{X: 100, Y: 200, W: 50, H: 80, ClassID: 0, Confidence: 90, TrackID: int64(i)},
			},
		}
		w.Enqueue(channelID, ts, a)
	}

	time.Sleep(2 * time.Second) // wait for flush
}

func TestPersistenceSource_Fetch_Hit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbm := persistence.NewDBManager(dir)
	defer dbm.Close()

	w := persistence.NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	base := time.Now().UTC().Truncate(time.Millisecond)
	seedAnalytics(t, w, "cam-1", 5, base)

	reader := persistence.NewReader(dbm)
	src := NewPersistenceSource(reader, "cam-1")

	// Fetch at exact time of first entry.
	fa := src.Fetch(0, base)
	if fa == nil {
		t.Fatal("expected analytics at exact time, got nil")
	}

	if fa.PeopleCount != 1 {
		t.Fatalf("PeopleCount = %d, want 1", fa.PeopleCount)
	}

	// Fetch at second entry (base + 500ms).
	fa = src.Fetch(1, base.Add(500*time.Millisecond))
	if fa == nil {
		t.Fatal("expected analytics at T+500ms, got nil")
	}

	if fa.PeopleCount != 2 {
		t.Fatalf("PeopleCount = %d, want 2", fa.PeopleCount)
	}
}

func TestPersistenceSource_Fetch_Miss(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbm := persistence.NewDBManager(dir)
	defer dbm.Close()

	w := persistence.NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	base := time.Now().UTC().Truncate(time.Millisecond)
	seedAnalytics(t, w, "cam-1", 3, base)

	reader := persistence.NewReader(dbm)
	src := NewPersistenceSource(reader, "cam-1")

	// Fetch far from any entry (well beyond 200ms tolerance).
	fa := src.Fetch(0, base.Add(1*time.Hour))
	if fa != nil {
		t.Fatal("expected nil analytics for time far from any entry")
	}
}

func TestPersistenceSource_Fetch_Tolerance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbm := persistence.NewDBManager(dir)
	defer dbm.Close()

	w := persistence.NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	base := time.Now().UTC().Truncate(time.Millisecond)
	seedAnalytics(t, w, "cam-1", 1, base)

	reader := persistence.NewReader(dbm)
	src := NewPersistenceSource(reader, "cam-1")

	// Within 200ms tolerance.
	fa := src.Fetch(0, base.Add(150*time.Millisecond))
	if fa == nil {
		t.Fatal("expected analytics within 200ms tolerance, got nil")
	}

	// Beyond 200ms tolerance.
	fa = src.Fetch(0, base.Add(250*time.Millisecond))
	if fa != nil {
		t.Fatal("expected nil analytics beyond 200ms tolerance")
	}
}

func TestPersistenceSource_CacheReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbm := persistence.NewDBManager(dir)
	defer dbm.Close()

	w := persistence.NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	base := time.Now().UTC().Truncate(time.Millisecond)
	// Seed 3 entries at base, base+500ms, base+1s.
	seedAnalytics(t, w, "cam-1", 3, base)

	// Seed 3 entries far away at base+20s, base+20.5s, base+21s.
	seedAnalytics(t, w, "cam-1", 3, base.Add(20*time.Second))

	reader := persistence.NewReader(dbm)
	src := NewPersistenceSource(reader, "cam-1")

	// First fetch near base (cache loads window around base).
	fa := src.Fetch(0, base)
	if fa == nil {
		t.Fatal("expected analytics near base")
	}

	// Fetch at base+20s should trigger a cache reload.
	fa = src.Fetch(0, base.Add(20*time.Second))
	if fa == nil {
		t.Fatal("expected analytics near base+20s after cache reload")
	}
}

func TestPersistenceSource_EmptyChannel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbm := persistence.NewDBManager(dir)
	defer dbm.Close()

	reader := persistence.NewReader(dbm)
	src := NewPersistenceSource(reader, "cam-nonexistent")

	// Should return nil without error for a channel with no data.
	fa := src.Fetch(0, time.Now())
	if fa != nil {
		t.Fatal("expected nil analytics for nonexistent channel")
	}
}

func TestPersistenceSource_ZeroWallClock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbm := persistence.NewDBManager(dir)
	defer dbm.Close()

	reader := persistence.NewReader(dbm)
	src := NewPersistenceSource(reader, "cam-1")

	// Zero wall-clock should return nil immediately.
	fa := src.Fetch(0, time.Time{})
	if fa != nil {
		t.Fatal("expected nil for zero wallClock")
	}
}

func TestPersistenceSource_DetectionConversion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbm := persistence.NewDBManager(dir)
	defer dbm.Close()

	w := persistence.NewWriter(dbm, 24*time.Hour, 1_000_000)
	defer w.Close()

	base := time.Now().UTC().Truncate(time.Millisecond)

	a := &av.FrameAnalytics{
		SiteID:       1,
		ChannelID:    10,
		CaptureMS:    base.UnixMilli(),
		RefWidth:     1920,
		RefHeight:    1080,
		VehicleCount: 2,
		PeopleCount:  3,
		Objects: []*av.Detection{
			{X: 10, Y: 20, W: 30, H: 40, ClassID: 1, Confidence: 95, TrackID: 42, IsEvent: true},
			{X: 100, Y: 200, W: 50, H: 60, ClassID: 0, Confidence: 80, TrackID: 43},
		},
	}
	w.Enqueue("cam-1", base, a)

	time.Sleep(2 * time.Second)

	reader := persistence.NewReader(dbm)
	src := NewPersistenceSource(reader, "cam-1")

	fa := src.Fetch(0, base)
	if fa == nil {
		t.Fatal("expected analytics")
	}

	if fa.VehicleCount != 2 || fa.PeopleCount != 3 {
		t.Fatalf("counts: vehicles=%d people=%d, want 2, 3", fa.VehicleCount, fa.PeopleCount)
	}

	if len(fa.Objects) != 2 {
		t.Fatalf("got %d detections, want 2", len(fa.Objects))
	}

	d := fa.Objects[0]
	if d.X != 10 || d.Y != 20 || d.W != 30 || d.H != 40 {
		t.Fatalf("detection bbox: (%d,%d,%d,%d), want (10,20,30,40)", d.X, d.Y, d.W, d.H)
	}

	if d.ClassID != 1 || d.Confidence != 95 || d.TrackID != 42 || !d.IsEvent {
		t.Fatalf("detection fields mismatch: classID=%d conf=%d track=%d event=%v",
			d.ClassID, d.Confidence, d.TrackID, d.IsEvent)
	}
}

func TestCompositeSource_PrimaryHit(t *testing.T) {
	t.Parallel()

	want := &av.FrameAnalytics{PeopleCount: 7}
	primary := &fixedSource{fa: want}
	fallback := &fixedSource{fa: &av.FrameAnalytics{PeopleCount: 99}}

	src := &CompositeSource{Primary: primary, Fallback: fallback}

	fa := src.Fetch(0, time.Now())
	if fa != want {
		t.Fatalf("expected primary result, got %v", fa)
	}
}

func TestCompositeSource_FallbackOnNil(t *testing.T) {
	t.Parallel()

	want := &av.FrameAnalytics{PeopleCount: 42}
	primary := &fixedSource{fa: nil}
	fallback := &fixedSource{fa: want}

	src := &CompositeSource{Primary: primary, Fallback: fallback}

	fa := src.Fetch(0, time.Now())
	if fa != want {
		t.Fatalf("expected fallback result, got %v", fa)
	}
}

func TestCompositeSource_BothNil(t *testing.T) {
	t.Parallel()

	src := &CompositeSource{
		Primary:  &fixedSource{fa: nil},
		Fallback: &fixedSource{fa: nil},
	}

	fa := src.Fetch(0, time.Now())
	if fa != nil {
		t.Fatal("expected nil when both sources return nil")
	}
}

func TestCompositeSource_NilFallback(t *testing.T) {
	t.Parallel()

	src := &CompositeSource{
		Primary:  &fixedSource{fa: nil},
		Fallback: nil,
	}

	fa := src.Fetch(0, time.Now())
	if fa != nil {
		t.Fatal("expected nil with nil primary and nil fallback")
	}
}

// fixedSource is a pva.Source that always returns a fixed result.
type fixedSource struct {
	fa *av.FrameAnalytics
}

func (s *fixedSource) Fetch(_ int64, _ time.Time) *FrameAnalytics { return s.fa }
