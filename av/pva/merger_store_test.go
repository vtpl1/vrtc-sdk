package pva

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

type stubDemuxer struct {
	streams []av.Stream
	packet  av.Packet
	err     error
	closed  bool
}

func (d *stubDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return d.streams, nil
}

func (d *stubDemuxer) ReadPacket(_ context.Context) (av.Packet, error) {
	return d.packet, d.err
}

func (d *stubDemuxer) Close() error {
	d.closed = true

	return nil
}

type stubSource struct {
	analytics *FrameAnalytics
	calls     int
	frameID   int64
	wallClock time.Time
}

func (s *stubSource) Fetch(frameID int64, wallClock time.Time) *FrameAnalytics {
	s.calls++
	s.frameID = frameID
	s.wallClock = wallClock

	return s.analytics
}

func TestMetadataMergerInjectsAnalytics(t *testing.T) {
	t.Parallel()

	streams := []av.Stream{{Idx: 0}}
	wallClock := time.Date(2026, 4, 5, 9, 30, 0, 0, time.UTC)
	packet := av.Packet{
		FrameID:       42,
		WallClockTime: wallClock,
		CodecType:     av.H264,
	}
	analytics := &av.FrameAnalytics{PeopleCount: 3}
	source := &stubSource{analytics: analytics}
	inner := &stubDemuxer{streams: streams, packet: packet}

	merger := NewMetadataMerger(inner, source)

	gotStreams, err := merger.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(gotStreams) != len(streams) {
		t.Fatalf("GetCodecs length: got %d, want %d", len(gotStreams), len(streams))
	}

	got, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if got.Analytics != analytics {
		t.Fatalf("Analytics pointer: got %#v, want %#v", got.Analytics, analytics)
	}

	if source.calls != 1 {
		t.Fatalf("Fetch calls: got %d, want 1", source.calls)
	}

	if source.frameID != packet.FrameID {
		t.Fatalf("Fetch frameID: got %d, want %d", source.frameID, packet.FrameID)
	}

	if !source.wallClock.Equal(packet.WallClockTime) {
		t.Fatalf("Fetch wallClock: got %v, want %v", source.wallClock, packet.WallClockTime)
	}

	if err := merger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !inner.closed {
		t.Fatal("expected inner demuxer to be closed")
	}
}

func TestMetadataMergerPropagatesReadErrorWithoutFetching(t *testing.T) {
	t.Parallel()

	readErr := io.EOF
	source := &stubSource{analytics: &av.FrameAnalytics{PeopleCount: 1}}
	merger := NewMetadataMerger(&stubDemuxer{err: readErr}, source)

	_, err := merger.ReadPacket(context.Background())
	if !errors.Is(err, readErr) {
		t.Fatalf("ReadPacket error: got %v, want %v", err, readErr)
	}

	if source.calls != 0 {
		t.Fatalf("Fetch calls: got %d, want 0", source.calls)
	}
}

func TestAnalyticsStoreSourceForMatchesWallClock(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	base := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	want := &av.FrameAnalytics{PeopleCount: 7}

	store.Put("cam-1", base, want)

	got := store.SourceFor("cam-1").Fetch(99, base)
	if got != want {
		t.Fatalf("Fetch: got %#v, want %#v", got, want)
	}
}

func TestAnalyticsStoreSourceForReturnsNearestMatchWithinTolerance(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	base := time.Date(2026, 4, 5, 10, 5, 0, 0, time.UTC)
	farther := &av.FrameAnalytics{PeopleCount: 1}
	nearer := &av.FrameAnalytics{PeopleCount: 2}

	store.Put("cam-1", base.Add(-80*time.Millisecond), farther)
	store.Put("cam-1", base.Add(20*time.Millisecond), nearer)

	got := store.SourceFor("cam-1").Fetch(123, base)
	if got != nearer {
		t.Fatalf("Fetch: got %#v, want nearest %#v", got, nearer)
	}
}

func TestAnalyticsStoreSourceForReturnsNilOutsideTolerance(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	base := time.Date(2026, 4, 5, 10, 10, 0, 0, time.UTC)

	store.Put("cam-1", base.Add(matchTolerance+time.Millisecond), &av.FrameAnalytics{PeopleCount: 4})

	got := store.SourceFor("cam-1").Fetch(5, base)
	if got != nil {
		t.Fatalf("Fetch: got %#v, want nil", got)
	}
}

func TestAnalyticsStorePutEvictsExpiredEntries(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(10 * time.Millisecond)
	oldWallClock := time.Now().Add(-time.Second)
	newWallClock := time.Now()
	oldAnalytics := &av.FrameAnalytics{PeopleCount: 1}
	newAnalytics := &av.FrameAnalytics{PeopleCount: 9}

	store.Put("cam-1", oldWallClock, oldAnalytics)
	store.Put("cam-1", newWallClock, newAnalytics)

	source := store.SourceFor("cam-1")

	if got := source.Fetch(1, oldWallClock); got != nil {
		t.Fatalf("expired Fetch: got %#v, want nil", got)
	}

	if got := source.Fetch(2, newWallClock); got != newAnalytics {
		t.Fatalf("fresh Fetch: got %#v, want %#v", got, newAnalytics)
	}
}

func TestAnalyticsStoreConcurrentPutAndFetch(t *testing.T) {
	store := NewAnalyticsStore(time.Minute)
	source := store.SourceFor("cam-1")
	base := time.Now().UTC()

	var wg sync.WaitGroup

	for writer := range 4 {
		wg.Add(1)

		go func(writer int) {
			defer wg.Done()

			for i := range 200 {
				store.Put("cam-1", base.Add(time.Duration(writer*200+i)*time.Millisecond), &av.FrameAnalytics{
					PeopleCount: int32(writer + i),
				})
			}
		}(writer)
	}

	for reader := range 4 {
		wg.Add(1)

		go func(reader int) {
			defer wg.Done()

			for i := range 200 {
				_ = source.Fetch(int64(reader*200+i), base.Add(time.Duration(i)*time.Millisecond))
			}
		}(reader)
	}

	wg.Wait()
}
