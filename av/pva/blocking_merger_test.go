package pva

import (
	"context"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// packetDemuxer returns a fixed video packet on every ReadPacket call.
type packetDemuxer struct {
	pkt av.Packet
}

func (d *packetDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return []av.Stream{{Idx: 0}}, nil
}

func (d *packetDemuxer) ReadPacket(_ context.Context) (av.Packet, error) {
	return d.pkt, nil
}

func (d *packetDemuxer) Close() error { return nil }

func videoPacket(wallClock time.Time) av.Packet {
	return av.Packet{
		FrameID:       100,
		WallClockTime: wallClock,
		CodecType:     av.H264,
	}
}

func TestBlockingMergerFastPath(t *testing.T) {
	t.Parallel()

	wallClock := time.Now().UTC()
	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()
	want := &av.FrameAnalytics{PeopleCount: 5}

	store.Put("cam-1", wallClock, want)

	merger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(wallClock)},
		store.SourceFor("cam-1"),
		hub,
		"cam-1",
		time.Second, // maxWait irrelevant — fast path should hit
	)

	start := time.Now()

	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	elapsed := time.Since(start)

	if pkt.Analytics != want {
		t.Fatalf("Analytics: got %v, want %v", pkt.Analytics, want)
	}

	if elapsed > 50*time.Millisecond {
		t.Fatalf("fast path took %v, expected < 50ms", elapsed)
	}
}

func TestBlockingMergerSlowPathHubNotification(t *testing.T) {
	t.Parallel()

	wallClock := time.Now().UTC()
	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()
	want := &av.FrameAnalytics{PeopleCount: 3}

	merger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(wallClock)},
		store.SourceFor("cam-1"),
		hub,
		"cam-1",
		2*time.Second, // long maxWait — should not be reached
	)

	type result struct {
		pkt av.Packet
		err error
	}

	ch := make(chan result, 1)

	go func() {
		pkt, err := merger.ReadPacket(context.Background())
		ch <- result{pkt, err}
	}()

	// Give ReadPacket time to enter the slow path and subscribe.
	time.Sleep(20 * time.Millisecond)

	// Simulate the analytics tool sending a result.
	store.Put("cam-1", wallClock, want)
	hub.Broadcast("cam-1", want)

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadPacket: %v", r.err)
		}

		if r.pkt.Analytics != want {
			t.Fatalf("Analytics: got %v, want %v", r.pkt.Analytics, want)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadPacket did not return after hub notification")
	}
}

func TestBlockingMergerTimeoutPassthrough(t *testing.T) {
	t.Parallel()

	wallClock := time.Now().UTC()
	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()
	maxWait := 50 * time.Millisecond

	merger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(wallClock)},
		store.SourceFor("cam-1"), // empty store — no analytics
		hub,
		"cam-1",
		maxWait,
	)

	start := time.Now()

	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	elapsed := time.Since(start)

	if pkt.Analytics != nil {
		t.Fatalf("Analytics: got %v, want nil (no analytics available)", pkt.Analytics)
	}

	// Should have waited approximately maxWait before giving up.
	if elapsed < maxWait {
		t.Fatalf("returned too early: %v < maxWait %v", elapsed, maxWait)
	}

	// Generous upper bound — should not take much longer than maxWait.
	if elapsed > maxWait+100*time.Millisecond {
		t.Fatalf("returned too late: %v, expected ~%v", elapsed, maxWait)
	}
}

func TestBlockingMergerAudioPassthrough(t *testing.T) {
	t.Parallel()

	wallClock := time.Now().UTC()
	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()
	maxWait := time.Second // long maxWait — audio should never wait

	audioPkt := av.Packet{
		FrameID:       200,
		WallClockTime: wallClock,
		CodecType:     av.AAC,
	}

	merger := NewBlockingMerger(
		&packetDemuxer{pkt: audioPkt},
		store.SourceFor("cam-1"),
		hub,
		"cam-1",
		maxWait,
	)

	start := time.Now()

	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	elapsed := time.Since(start)

	if pkt.Analytics != nil {
		t.Fatalf("Analytics: got %v, want nil for audio packet", pkt.Analytics)
	}

	if elapsed > 50*time.Millisecond {
		t.Fatalf("audio packet blocked for %v, expected immediate passthrough", elapsed)
	}
}

func TestBlockingMergerContextCancellation(t *testing.T) {
	t.Parallel()

	wallClock := time.Now().UTC()
	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()
	maxWait := 5 * time.Second // long maxWait — context should cancel first

	merger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(wallClock)},
		store.SourceFor("cam-1"),
		hub,
		"cam-1",
		maxWait,
	)

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

	// Cancel after a short delay — well before maxWait.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadPacket: %v", r.err)
		}

		if r.pkt.Analytics != nil {
			t.Fatalf("Analytics: got %v, want nil on cancellation", r.pkt.Analytics)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadPacket did not return after context cancellation")
	}
}

func TestBlockingMergerCrashRecovery(t *testing.T) {
	t.Parallel()

	wallClock := time.Now().UTC()
	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()
	maxWait := 50 * time.Millisecond

	healthy := &av.FrameAnalytics{PeopleCount: 10}
	recovered := &av.FrameAnalytics{PeopleCount: 20}

	// ── Phase 1: healthy — analytics available in store ────────────────────
	store.Put("cam-1", wallClock, healthy)

	merger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(wallClock)},
		store.SourceFor("cam-1"),
		hub,
		"cam-1",
		maxWait,
	)

	pkt, err := merger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("phase 1 ReadPacket: %v", err)
	}

	if pkt.Analytics != healthy {
		t.Fatalf("phase 1: got %v, want %v", pkt.Analytics, healthy)
	}

	// ── Phase 2: crash — no analytics, different wall-clock ───────────────
	crashClock := wallClock.Add(10 * time.Second)
	crashMerger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(crashClock)},
		store.SourceFor("cam-1"), // store has no entry for crashClock
		hub,
		"cam-1",
		maxWait,
	)

	start := time.Now()

	pkt, err = crashMerger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("phase 2 ReadPacket: %v", err)
	}

	if pkt.Analytics != nil {
		t.Fatalf("phase 2: got %v, want nil (tool crashed)", pkt.Analytics)
	}

	if time.Since(start) < maxWait {
		t.Fatal("phase 2: returned before maxWait expired")
	}

	// ── Phase 3: recovery — analytics tool reconnects ─────────────────────
	recoverClock := wallClock.Add(20 * time.Second)
	store.Put("cam-1", recoverClock, recovered)

	recoverMerger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(recoverClock)},
		store.SourceFor("cam-1"),
		hub,
		"cam-1",
		maxWait,
	)

	pkt, err = recoverMerger.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("phase 3 ReadPacket: %v", err)
	}

	if pkt.Analytics != recovered {
		t.Fatalf("phase 3: got %v, want %v (tool recovered)", pkt.Analytics, recovered)
	}
}

func TestBlockingMergerHubChannelClosed(t *testing.T) {
	t.Parallel()

	wallClock := time.Now().UTC()
	store := NewAnalyticsStore(time.Minute)
	hub := NewAnalyticsHub()
	maxWait := 2 * time.Second // long maxWait — closed channel should unblock first

	merger := NewBlockingMerger(
		&packetDemuxer{pkt: videoPacket(wallClock)},
		store.SourceFor("cam-1"),
		hub,
		"cam-1",
		maxWait,
	)

	type result struct {
		pkt av.Packet
		err error
	}

	ch := make(chan result, 1)

	go func() {
		pkt, err := merger.ReadPacket(context.Background())
		ch <- result{pkt, err}
	}()

	// Give ReadPacket time to subscribe to the hub.
	time.Sleep(20 * time.Millisecond)

	// Close all subscriber channels for this source (simulates hub teardown).
	hub.mu.Lock()
	for _, sub := range hub.subs["cam-1"] {
		close(sub)
	}

	hub.subs["cam-1"] = nil
	hub.mu.Unlock()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadPacket: %v", r.err)
		}

		if r.pkt.Analytics != nil {
			t.Fatalf("Analytics: got %v, want nil on hub channel close", r.pkt.Analytics)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadPacket did not return after hub channel was closed")
	}
}
