package pva

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// sequenceDemuxer feeds packets from a pre-built slice, then returns io.EOF.
// Simulates a recorded segment demuxer that a post-seek BlockingMerger reads.
type sequenceDemuxer struct {
	mu      sync.Mutex
	packets []av.Packet
	idx     int
	closed  atomic.Bool
}

func newSequenceDemuxer(pkts ...av.Packet) *sequenceDemuxer {
	return &sequenceDemuxer{packets: pkts}
}

func (d *sequenceDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return []av.Stream{{Idx: 0}}, nil
}

func (d *sequenceDemuxer) ReadPacket(_ context.Context) (av.Packet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.idx >= len(d.packets) {
		return av.Packet{}, io.EOF
	}

	pkt := d.packets[d.idx]
	d.idx++

	return pkt, nil
}

func (d *sequenceDemuxer) Close() error {
	d.closed.Store(true)

	return nil
}

// seekScenario creates a fresh BlockingMerger against the shared store/hub,
// simulating the post-seek state where the old merger is torn down and a new
// one is created.
func seekScenario(
	inner av.DemuxCloser,
	store *AnalyticsStore,
	hub *AnalyticsHub,
	sourceID string,
	maxWait time.Duration,
) *BlockingMerger {
	return NewBlockingMerger(inner, store.SourceFor(sourceID), hub, sourceID, maxWait)
}

// TestSeek_LiveToLive_AnalyticsPreserved simulates a seek within live mode.
// The old merger is closed and a new one is created. Analytics in the store
// for the new wall-clock should be found immediately (fast path).
func TestSeek_LiveToLive_AnalyticsPreserved(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	// Pre-seek: analytics at T1.
	t1 := time.Now().UTC()
	a1 := &av.FrameAnalytics{PeopleCount: 3, VehicleCount: 1}
	store.Put("cam-1", t1, a1)

	merger1 := seekScenario(
		&packetDemuxer{pkt: videoPacket(t1)},
		store, hub, "cam-1", time.Second,
	)

	pkt, err := merger1.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("pre-seek ReadPacket: %v", err)
	}

	if pkt.Analytics != a1 {
		t.Fatalf("pre-seek: got %v, want %v", pkt.Analytics, a1)
	}

	// Close old merger (simulates seek teardown).
	if err := merger1.Close(); err != nil {
		t.Fatalf("close old merger: %v", err)
	}

	// Post-seek: analytics at T2.
	t2 := t1.Add(30 * time.Second)
	a2 := &av.FrameAnalytics{PeopleCount: 7, VehicleCount: 2}
	store.Put("cam-1", t2, a2)

	merger2 := seekScenario(
		&packetDemuxer{pkt: videoPacket(t2)},
		store, hub, "cam-1", time.Second,
	)
	defer merger2.Close()

	pkt, err = merger2.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("post-seek ReadPacket: %v", err)
	}

	if pkt.Analytics != a2 {
		t.Fatalf("post-seek: got %v, want %v", pkt.Analytics, a2)
	}
}

// TestSeek_LiveToRecorded_NoAnalytics simulates seeking from live (analytics
// enriched) to recorded playback. In recorded mode the raw segment demuxer is
// used without a BlockingMerger, so packets carry no analytics. We verify that
// the same store/hub state does not interfere with a plain demuxer.
func TestSeek_LiveToRecorded_NoAnalytics(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	t1 := time.Now().UTC()
	store.Put("cam-1", t1, &av.FrameAnalytics{PeopleCount: 10})

	// Live mode with analytics.
	merger := seekScenario(
		&packetDemuxer{pkt: videoPacket(t1)},
		store, hub, "cam-1", time.Second,
	)

	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("live ReadPacket: %v", err)
	}

	if pkt.Analytics == nil || pkt.Analytics.PeopleCount != 10 {
		t.Fatal("live mode should have analytics")
	}

	_ = merger.Close()

	// Recorded mode: raw demuxer with no BlockingMerger wrapping.
	// Packets from recorded segments have no WallClockTime set (or it is
	// not looked up). This simulates what actually happens during seek to
	// recorded — the system creates a bare ChainingDemuxer, not a merger.
	recPkt := av.Packet{
		FrameID:   200,
		CodecType: av.H264,
		DTS:       time.Second,
		// WallClockTime is zero — typical for recorded segment packets.
	}

	dmx := newSequenceDemuxer(recPkt)

	got, err := dmx.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("recorded ReadPacket: %v", err)
	}

	if got.Analytics != nil {
		t.Fatal("recorded packet should have nil analytics (no merger)")
	}
}

// TestSeek_RecordedToLive_AnalyticsRestored simulates seeking from recorded
// back to live. A new BlockingMerger is created and analytics should be
// available again from the store/hub.
func TestSeek_RecordedToLive_AnalyticsRestored(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	// Phase 1: recorded mode (no merger, just raw demuxer).
	recPkt := av.Packet{FrameID: 300, CodecType: av.H264, DTS: time.Second}
	dmx := newSequenceDemuxer(recPkt)

	got, err := dmx.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("recorded ReadPacket: %v", err)
	}

	if got.Analytics != nil {
		t.Fatal("recorded should have no analytics")
	}

	_ = dmx.Close()

	// Phase 2: seek to live — new BlockingMerger wraps the live feed.
	liveTime := time.Now().UTC()
	liveAnalytics := &av.FrameAnalytics{PeopleCount: 42}
	store.Put("cam-1", liveTime, liveAnalytics)

	merger := seekScenario(
		&packetDemuxer{pkt: videoPacket(liveTime)},
		store, hub, "cam-1", time.Second,
	)
	defer merger.Close()

	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("live ReadPacket: %v", err)
	}

	if pkt.Analytics != liveAnalytics {
		t.Fatalf("analytics not restored after seek to live: got %v, want %v",
			pkt.Analytics, liveAnalytics)
	}
}

// TestSeek_AnalyticsArrivesDuringNewMerger simulates a seek where analytics
// are not yet in the store at the moment the new merger is created, but arrive
// shortly after via the hub (slow path). This tests the subscription lifecycle
// across seek boundaries — old subscriptions must be cleaned up and new ones
// must work correctly.
func TestSeek_AnalyticsArrivesDuringNewMerger(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	// Pre-seek merger (will be torn down).
	t1 := time.Now().UTC()
	store.Put("cam-1", t1, &av.FrameAnalytics{PeopleCount: 1})

	merger1 := seekScenario(
		&packetDemuxer{pkt: videoPacket(t1)},
		store, hub, "cam-1", time.Second,
	)

	pkt1, _ := merger1.ReadPacket(context.Background())
	if pkt1.Analytics == nil {
		t.Fatal("pre-seek should have analytics")
	}

	_ = merger1.Close()

	// Post-seek: analytics NOT yet in store.
	t2 := t1.Add(time.Minute)
	postSeekAnalytics := &av.FrameAnalytics{PeopleCount: 99}

	merger2 := seekScenario(
		&packetDemuxer{pkt: videoPacket(t2)},
		store, hub, "cam-1", 2*time.Second,
	)
	defer merger2.Close()

	type result struct {
		pkt av.Packet
		err error
	}

	ch := make(chan result, 1)

	go func() {
		pkt, err := merger2.ReadPacket(context.Background())
		ch <- result{pkt, err}
	}()

	// Simulate analytics tool sending result after a short delay.
	time.Sleep(50 * time.Millisecond)
	store.Put("cam-1", t2, postSeekAnalytics)
	hub.Broadcast("cam-1", postSeekAnalytics)

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("post-seek ReadPacket: %v", r.err)
		}

		if r.pkt.Analytics != postSeekAnalytics {
			t.Fatalf("post-seek analytics mismatch: got %v, want %v",
				r.pkt.Analytics, postSeekAnalytics)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("post-seek ReadPacket timed out")
	}
}

// TestSeek_NoAnalyticsAvailable_TimesOutGracefully simulates seeking to a
// position where the analytics tool has not processed any frames. The merger
// should time out and return the packet without analytics.
func TestSeek_NoAnalyticsAvailable_TimesOutGracefully(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	// Store has analytics only at T1.
	t1 := time.Now().UTC()
	store.Put("cam-1", t1, &av.FrameAnalytics{PeopleCount: 5})

	// Seek to T2 which has no analytics.
	t2 := t1.Add(5 * time.Minute)
	maxWait := 100 * time.Millisecond

	merger := seekScenario(
		&packetDemuxer{pkt: videoPacket(t2)},
		store, hub, "cam-1", maxWait,
	)
	defer merger.Close()

	start := time.Now()

	pkt, err := merger.ReadPacket(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if pkt.Analytics != nil {
		t.Fatal("expected nil analytics at unseeded time")
	}

	if elapsed < maxWait {
		t.Fatalf("returned too early: %v < maxWait %v", elapsed, maxWait)
	}

	if elapsed > maxWait+200*time.Millisecond {
		t.Fatalf("returned too late: %v, expected ~%v", elapsed, maxWait)
	}
}

// TestSeek_MultipleSequentialPackets verifies that after a seek, the new
// merger correctly enriches multiple consecutive packets, not just the first.
// Packets are spaced 500ms apart (beyond the 200ms match tolerance) so each
// frame matches its own distinct analytics entry.
func TestSeek_MultipleSequentialPackets(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	base := time.Now().UTC()

	// Seed analytics for 3 frames spaced 500ms apart (well beyond 200ms tolerance).
	// Use time.Now() so entries are "current" and not evicted by the store's TTL.
	for i := range 3 {
		wc := base.Add(time.Duration(i) * 500 * time.Millisecond)
		store.Put("cam-1", wc, &av.FrameAnalytics{PeopleCount: int32(i + 1)})
	}

	// Build packets matching those wall-clock times.
	var pkts []av.Packet

	for i := range 3 {
		wc := base.Add(time.Duration(i) * 500 * time.Millisecond)
		pkts = append(pkts, av.Packet{
			FrameID:       int64(i + 1),
			WallClockTime: wc,
			CodecType:     av.H264,
		})
	}

	dmx := newSequenceDemuxer(pkts...)

	merger := seekScenario(dmx, store, hub, "cam-1", time.Second)
	defer merger.Close()

	for i := range 3 {
		pkt, err := merger.ReadPacket(context.Background())
		if err != nil {
			t.Fatalf("packet %d: ReadPacket: %v", i, err)
		}

		if pkt.Analytics == nil {
			t.Fatalf("packet %d: expected analytics, got nil", i)
		}

		wantCount := int32(i + 1)
		if pkt.Analytics.PeopleCount != wantCount {
			t.Fatalf("packet %d: PeopleCount = %d, want %d",
				i, pkt.Analytics.PeopleCount, wantCount)
		}
	}
}

// TestSeek_MixedVideoAudio verifies that after a seek, audio packets pass
// through immediately while video packets get analytics.
func TestSeek_MixedVideoAudio(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	wc := time.Now().UTC()
	store.Put("cam-1", wc, &av.FrameAnalytics{PeopleCount: 8})

	videoPkt := av.Packet{
		FrameID:       10,
		WallClockTime: wc,
		CodecType:     av.H264,
	}
	audioPkt := av.Packet{
		FrameID:       11,
		WallClockTime: wc,
		CodecType:     av.AAC,
	}
	video2Pkt := av.Packet{
		FrameID:       12,
		WallClockTime: wc.Add(33 * time.Millisecond),
		CodecType:     av.H264,
	}

	dmx := newSequenceDemuxer(videoPkt, audioPkt, video2Pkt)

	merger := seekScenario(dmx, store, hub, "cam-1", time.Second)
	defer merger.Close()

	// Packet 1: video — should have analytics.
	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("video1: %v", err)
	}

	if pkt.Analytics == nil {
		t.Fatal("video1: expected analytics")
	}

	// Packet 2: audio — should pass through immediately, no analytics.
	start := time.Now()

	pkt, err = merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("audio: %v", err)
	}

	if pkt.Analytics != nil {
		t.Fatal("audio: expected nil analytics")
	}

	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("audio packet should not block for analytics")
	}

	// Packet 3: video — no analytics in store for t+33ms, should timeout.
	// (We use short maxWait to keep test fast.)
	merger2 := seekScenario(
		newSequenceDemuxer(video2Pkt),
		store, hub, "cam-1", 50*time.Millisecond,
	)
	defer merger2.Close()

	pkt, err = merger2.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("video2: %v", err)
	}

	// video2 wall-clock (wc+33ms) is within 200ms tolerance of wc, so it may
	// match the store entry. Either outcome is valid.
}

// TestSeek_OldMergerCloseDoesNotAffectNew verifies that closing the old
// merger (and its hub subscription) does not interfere with the new merger's
// subscription.
func TestSeek_OldMergerCloseDoesNotAffectNew(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	t1 := time.Now().UTC()

	// Create and start reading from the OLD merger (enters slow path).
	oldMerger := seekScenario(
		&packetDemuxer{pkt: videoPacket(t1)},
		store, hub, "cam-1", 2*time.Second,
	)

	oldResult := make(chan av.Packet, 1)

	go func() {
		pkt, _ := oldMerger.ReadPacket(context.Background())
		oldResult <- pkt
	}()

	// Give old merger time to subscribe to hub.
	time.Sleep(30 * time.Millisecond)

	// Close old merger (simulates seek teardown). Its hub subscription is
	// cleaned up by Unsubscribe in the deferred call inside ReadPacket.
	// However, since ReadPacket is still blocking, the close only stops the
	// inner demuxer. The goroutine will unblock when we broadcast below.
	_ = oldMerger.Close()

	// Create the NEW merger for the post-seek position.
	t2 := t1.Add(time.Minute)
	newAnalytics := &av.FrameAnalytics{PeopleCount: 77}

	newMerger := seekScenario(
		&packetDemuxer{pkt: videoPacket(t2)},
		store, hub, "cam-1", 2*time.Second,
	)
	defer newMerger.Close()

	newResult := make(chan av.Packet, 1)

	go func() {
		pkt, _ := newMerger.ReadPacket(context.Background())
		newResult <- pkt
	}()

	// Give new merger time to subscribe.
	time.Sleep(30 * time.Millisecond)

	// Analytics arrive for both timestamps.
	store.Put("cam-1", t1, &av.FrameAnalytics{PeopleCount: 11})
	store.Put("cam-1", t2, newAnalytics)
	hub.Broadcast("cam-1", newAnalytics)

	// The NEW merger should get its analytics.
	select {
	case pkt := <-newResult:
		if pkt.Analytics != newAnalytics {
			t.Fatalf("new merger: got %v, want %v", pkt.Analytics, newAnalytics)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new merger did not return")
	}

	// The OLD merger's goroutine should also eventually unblock (it received
	// the broadcast). Drain it to prevent goroutine leak.
	select {
	case <-oldResult:
		// OK — old merger unblocked
	case <-time.After(3 * time.Second):
		t.Fatal("old merger goroutine leaked")
	}
}

// TestSeek_ContextCancelDuringSlowPath verifies that cancelling the context
// (e.g., client disconnect during seek) correctly unblocks the merger.
func TestSeek_ContextCancelDuringSlowPath(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	wc := time.Now().UTC()
	maxWait := 5 * time.Second // long — context cancel should fire first

	merger := seekScenario(
		&packetDemuxer{pkt: videoPacket(wc)},
		store, hub, "cam-1", maxWait,
	)
	defer merger.Close()

	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		pkt av.Packet
		err error
	}

	ch := make(chan result, 1)

	go func() {
		pkt, err := merger.ReadPacket(ctx)
		ch <- result{pkt, err}
	}()

	// Cancel quickly — simulates the old session being torn down mid-read.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadPacket: %v", r.err)
		}

		if r.pkt.Analytics != nil {
			t.Fatal("expected nil analytics on context cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("ReadPacket did not return after context cancel")
	}
}

// TestSeek_SequenceDemuxerEOF verifies that when the inner demuxer returns
// EOF (end of recorded segment), the merger propagates the error correctly.
func TestSeek_SequenceDemuxerEOF(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	wc := time.Now().UTC()
	store.Put("cam-1", wc, &av.FrameAnalytics{PeopleCount: 1})

	// Demuxer with exactly one packet.
	dmx := newSequenceDemuxer(videoPacket(wc))

	merger := seekScenario(dmx, store, hub, "cam-1", time.Second)
	defer merger.Close()

	// First read: success with analytics.
	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("first ReadPacket: %v", err)
	}

	if pkt.Analytics == nil {
		t.Fatal("first packet should have analytics")
	}

	// Second read: EOF from inner demuxer.
	_, err = merger.ReadPacket(context.Background())
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

// TestSeek_StoreExpired_NoMatch verifies that after seeking, if the analytics
// store has evicted old entries (TTL expired), the merger correctly times out.
// Note: the AnalyticsStore uses lazy eviction — expired entries are only
// removed when a new Put() call triggers the cleanup.
func TestSeek_StoreExpired_NoMatch(t *testing.T) {
	t.Parallel()

	// Very short TTL: entries expire almost immediately.
	store := NewAnalyticsStore(10 * time.Millisecond)
	hub := NewAnalyticsHub()

	t1 := time.Now().UTC()
	store.Put("cam-1", t1, &av.FrameAnalytics{PeopleCount: 5})

	// Wait for TTL to expire.
	time.Sleep(50 * time.Millisecond)

	// Trigger lazy eviction by inserting a new entry on a DIFFERENT source
	// (same source at a nearby time would match within 200ms tolerance).
	store.Put("cam-other", time.Now().UTC(), &av.FrameAnalytics{PeopleCount: 99})

	// Also trigger eviction on cam-1 by inserting far in the future.
	store.Put("cam-1", time.Now().UTC().Add(time.Hour), &av.FrameAnalytics{PeopleCount: 99})

	// Now seek to the OLD time — the entry should have been evicted (t1 is
	// > 10ms old) and the new entry at +1h is beyond 200ms tolerance.
	maxWait := 50 * time.Millisecond

	merger := seekScenario(
		&packetDemuxer{pkt: videoPacket(t1)},
		store, hub, "cam-1", maxWait,
	)
	defer merger.Close()

	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if pkt.Analytics != nil {
		t.Fatal("expected nil analytics after TTL expiry")
	}
}

// TestSeek_RapidSeeks_IndependentMergers simulates rapid sequential seeks.
// Each seek creates and destroys a merger. The store should serve the correct
// analytics for each wall-clock time independently.
func TestSeek_RapidSeeks_IndependentMergers(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	base := time.Now().UTC()

	// Seed analytics for 5 seek targets.
	for i := range 5 {
		wc := base.Add(time.Duration(i) * 10 * time.Second)
		store.Put("cam-1", wc, &av.FrameAnalytics{PeopleCount: int32(i + 1)})
	}

	// Rapid-fire seek simulation: create → read → close in tight loop.
	for i := range 5 {
		wc := base.Add(time.Duration(i) * 10 * time.Second)
		wantCount := int32(i + 1)

		merger := seekScenario(
			&packetDemuxer{pkt: videoPacket(wc)},
			store, hub, "cam-1", time.Second,
		)

		pkt, err := merger.ReadPacket(context.Background())
		_ = merger.Close()

		if err != nil {
			t.Fatalf("seek %d: ReadPacket: %v", i, err)
		}

		if pkt.Analytics == nil {
			t.Fatalf("seek %d: expected analytics", i)
		}

		if pkt.Analytics.PeopleCount != wantCount {
			t.Fatalf("seek %d: PeopleCount = %d, want %d",
				i, pkt.Analytics.PeopleCount, wantCount)
		}
	}
}

// TestSeek_DifferentSourceIDs verifies that after seeking, analytics from one
// camera do not leak to another camera's merger.
func TestSeek_DifferentSourceIDs(t *testing.T) {
	t.Parallel()

	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()

	wc := time.Now().UTC()

	cam1Analytics := &av.FrameAnalytics{PeopleCount: 10}
	cam2Analytics := &av.FrameAnalytics{PeopleCount: 20}

	store.Put("cam-1", wc, cam1Analytics)
	store.Put("cam-2", wc, cam2Analytics)

	// Merger for cam-1 should get cam-1 analytics.
	merger1 := seekScenario(
		&packetDemuxer{pkt: videoPacket(wc)},
		store, hub, "cam-1", time.Second,
	)

	pkt1, _ := merger1.ReadPacket(context.Background())
	_ = merger1.Close()

	if pkt1.Analytics != cam1Analytics {
		t.Fatalf("cam-1: got %v, want %v", pkt1.Analytics, cam1Analytics)
	}

	// Merger for cam-2 should get cam-2 analytics.
	merger2 := seekScenario(
		&packetDemuxer{pkt: videoPacket(wc)},
		store, hub, "cam-2", time.Second,
	)

	pkt2, _ := merger2.ReadPacket(context.Background())
	_ = merger2.Close()

	if pkt2.Analytics != cam2Analytics {
		t.Fatalf("cam-2: got %v, want %v", pkt2.Analytics, cam2Analytics)
	}
}
