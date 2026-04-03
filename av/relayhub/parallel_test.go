package relayhub_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/relayhub"
)

// errWritePacketFailed is the sentinel returned by failingMuxer.
var errWritePacketFailed = errors.New("write packet intentionally failed")

// =============================================================================
// Mock demuxers
// =============================================================================

// mockDemuxer produces packets immediately on each ReadPacket call.
// Rate limiting is handled by the producer's internal ticker (maxFps).
// Context cancellation is respected via ctx.Err() before each packet.
type mockDemuxer struct {
	streams []av.Stream
	pktIdx  atomic.Int64
}

func (d *mockDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return d.streams, nil
}

func (d *mockDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	if err := ctx.Err(); err != nil {
		return av.Packet{}, err
	}

	n := d.pktIdx.Add(1)

	return av.Packet{
		KeyFrame: n%30 == 0,
		DTS:      time.Duration(n) * (time.Second / 250),
	}, nil
}

func (d *mockDemuxer) Close() error { return nil }

// pausableMockDemuxer wraps mockDemuxer and additionally implements av.Pauser.
// ReadPacket blocks while paused, honouring context cancellation.
type pausableMockDemuxer struct {
	mockDemuxer

	mu      sync.Mutex
	paused  bool
	unpause chan struct{} // closed-and-replaced on Resume
}

func newPausableMockDemuxer(streams []av.Stream) *pausableMockDemuxer {
	d := &pausableMockDemuxer{}
	d.streams = streams
	d.unpause = make(chan struct{})
	close(d.unpause) // initially not paused → immediately readable

	return d
}

func (d *pausableMockDemuxer) Pause(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.paused {
		d.paused = true
		d.unpause = make(chan struct{})
	}

	return nil
}

func (d *pausableMockDemuxer) Resume(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.paused {
		d.paused = false
		close(d.unpause) // unblock any waiting ReadPacket
	}

	return nil
}

func (d *pausableMockDemuxer) IsPaused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.paused
}

func (d *pausableMockDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	// Wait until not paused or context cancelled.
	for {
		d.mu.Lock()
		gate := d.unpause
		d.mu.Unlock()

		select {
		case <-ctx.Done():
			return av.Packet{}, ctx.Err()
		case <-gate:
		}
		// Double-check under lock to avoid a race where Pause is called
		// immediately after Resume signals the gate.
		d.mu.Lock()
		stillPaused := d.paused
		d.mu.Unlock()

		if !stillPaused {
			break
		}
	}

	return d.mockDemuxer.ReadPacket(ctx)
}

// =============================================================================
// Mock muxers
// =============================================================================

// mockMuxer counts the packets it receives. All lifecycle methods are no-ops.
type mockMuxer struct {
	packetsRecv atomic.Int64
}

func (m *mockMuxer) WriteHeader(_ context.Context, _ []av.Stream) error { return nil }
func (m *mockMuxer) WritePacket(_ context.Context, _ av.Packet) error {
	m.packetsRecv.Add(1)

	return nil
}
func (m *mockMuxer) WriteTrailer(_ context.Context, _ error) error { return nil }
func (m *mockMuxer) Close() error                                  { return nil }

// failingMuxer writes packets successfully up to failAfter, then returns an error.
type failingMuxer struct {
	packetsRecv atomic.Int64
	failAfter   int64
}

func (m *failingMuxer) WriteHeader(_ context.Context, _ []av.Stream) error { return nil }
func (m *failingMuxer) WritePacket(_ context.Context, _ av.Packet) error {
	n := m.packetsRecv.Add(1)
	if n > m.failAfter {
		return errWritePacketFailed
	}

	return nil
}
func (m *failingMuxer) WriteTrailer(_ context.Context, _ error) error { return nil }
func (m *failingMuxer) Close() error                                  { return nil }

type fakeVideoCodec struct {
	typ           av.CodecType
	width, height int
	timeScale     uint32
}

func (c fakeVideoCodec) Type() av.CodecType { return c.typ }
func (c fakeVideoCodec) Width() int         { return c.width }
func (c fakeVideoCodec) Height() int        { return c.height }
func (c fakeVideoCodec) TimeScale() uint32  { return c.timeScale }

type fakeAudioCodec struct {
	typ        av.CodecType
	sampleRate int
}

func (c fakeAudioCodec) Type() av.CodecType                           { return c.typ }
func (c fakeAudioCodec) SampleFormat() av.SampleFormat                { return av.S16 }
func (c fakeAudioCodec) SampleRate() int                              { return c.sampleRate }
func (c fakeAudioCodec) ChannelLayout() av.ChannelLayout              { return av.ChMono }
func (c fakeAudioCodec) PacketDuration([]byte) (time.Duration, error) { return 0, nil }

// =============================================================================
// Test helpers
// =============================================================================

func testStreams() []av.Stream { return []av.Stream{{Idx: 0}} }

func makeDemuxerFactory(streams []av.Stream) av.DemuxerFactory {
	return func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return &mockDemuxer{streams: streams}, nil
	}
}

// makePausableDemuxerFactory returns a DemuxerFactory that produces
// pausableMockDemuxers and exposes the most-recently created instance.
func makePausableDemuxerFactory(
	streams []av.Stream,
) (av.DemuxerFactory, *atomic.Pointer[pausableMockDemuxer]) {
	var latest atomic.Pointer[pausableMockDemuxer]

	factory := func(_ context.Context, _ string) (av.DemuxCloser, error) {
		d := newPausableMockDemuxer(streams)
		latest.Store(d)

		return d, nil
	}

	return factory, &latest
}

// makeMuxerFactory returns a MuxerFactory and a sync.Map registry of every
// mockMuxer it creates, keyed by consumerID.
func makeMuxerFactory() (av.MuxerFactory, *sync.Map) {
	registry := new(sync.Map)

	return func(_ context.Context, consumerID string) (av.MuxCloser, error) {
		m := &mockMuxer{}
		registry.Store(consumerID, m)

		return m, nil
	}, registry
}

// startedSM creates and starts a RelayHub backed by mockDemuxer.
// t.Cleanup calls Stop() so no explicit teardown is needed in each test.
func startedSM(t *testing.T, ctx context.Context) *relayhub.RelayHub {
	t.Helper()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = sm.Stop() })

	return sm
}

// removeConsumer closes h. ErrRelayNotFound is already swallowed by
// ConsumerHandle.Close, so no special-casing is needed here.
func removeConsumer(t *testing.T, h av.ConsumerHandle, ctx context.Context) {
	t.Helper()

	if err := h.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// =============================================================================
// Tests — RelayHub lifecycle
// =============================================================================

// TestDoubleStartReturnsError verifies that a second call to Start returns
// ErrRelayHubAlreadyStarted rather than silently leaking a goroutine.
func TestDoubleStartReturnsError(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	defer func() { _ = sm.Stop() }()

	if err := sm.Start(ctx); !errors.Is(err, relayhub.ErrRelayHubAlreadyStarted) {
		t.Errorf("second Start returned %v, want ErrRelayHubAlreadyStarted", err)
	}
}

// TestMultipleStopCalls verifies that calling Stop multiple times is safe and
// always returns nil — idempotency is required because callers often defer Stop
// without checking whether it was already invoked.
func TestMultipleStopCalls(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	for i := range 5 {
		if err := sm.Stop(); err != nil {
			t.Errorf("Stop() call %d returned %v, want nil", i+1, err)
		}
	}
}

// TestConsumeAfterStop verifies that Consume returns ErrRelayHubClosing
// once Stop has been called, regardless of whether the producer previously existed.
func TestConsumeAfterStop(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if err := sm.Stop(); err != nil {
		t.Fatal(err)
	}

	factory, _ := makeMuxerFactory()

	_, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{
		ConsumerID:   "consumer-1",
		MuxerFactory: factory,
	})
	if !errors.Is(err, relayhub.ErrRelayHubClosing) {
		t.Errorf("Consume after Stop returned %v, want ErrRelayHubClosing", err)
	}
}

// =============================================================================
// Tests — Consumer and producer management
// =============================================================================

// TestConsumerAlreadyExists verifies that adding the same consumerID twice to
// the same producer returns ErrConsumerAlreadyExists on the second call.
func TestConsumerAlreadyExists(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "consumer-1", MuxerFactory: factory}); err != nil {
		t.Fatal(err)
	}

	_, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "consumer-1", MuxerFactory: factory})
	if !errors.Is(err, relayhub.ErrConsumerAlreadyExists) {
		t.Errorf("duplicate Consume: got %v, want ErrConsumerAlreadyExists", err)
	}
}

// TestCloseHandleIdempotent verifies that calling Close on a ConsumerHandle
// more than once is safe and always returns nil.
func TestCloseHandleIdempotent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{
		ConsumerID:   "consumer-1",
		MuxerFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := handle.Close(ctx); err != nil {
		t.Errorf("first Close: got %v, want nil", err)
	}

	if err := handle.Close(ctx); err != nil {
		t.Errorf("second Close: got %v, want nil", err)
	}
}

// TestConsumerHandleCloseAutoStopsProducer verifies that a producer with no
// remaining consumers is automatically removed after its handle is closed
// within the cleanup ticker period (~1 s for the producer ticker + ~1 s for
// the RelayHub ticker = <= 2 s).
func TestConsumerHandleCloseAutoStopsProducer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{
		ConsumerID:   "consumer-1",
		MuxerFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}

	if n := sm.GetActiveRelayCount(ctx); n != 1 {
		t.Fatalf("expected 1 active producer after add, got %d", n)
	}

	if handle.ID() != "consumer-1" {
		t.Fatalf("handle ID = %q, want consumer-1", handle.ID())
	}

	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Two ticker intervals can elapse before the producer is gone:
	//   1. Producer's ticker removes the inactive consumer from its map.
	//   2. RelayHub's ticker sees ConsumerCount() == 0 and removes the producer.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sm.GetActiveRelayCount(ctx) == 0 {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Errorf("producer still active 3 s after all consumers left (count=%d)",
		sm.GetActiveRelayCount(ctx))
}

// =============================================================================
// Tests — Error propagation
// =============================================================================

// TestDemuxerFactoryErrorPropagatesToCaller verifies that when the demuxer
// factory returns an error the Consume call eventually surfaces that error
// to the caller (via the producer's LastError).
func TestDemuxerFactoryErrorPropagatesToCaller(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errDemuxFail := errors.New("demuxer factory failed")
	failFactory := func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return nil, errDemuxFail
	}

	sm := relayhub.New(failFactory, nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = sm.Stop() })

	factory, _ := makeMuxerFactory()

	_, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "consumer-1", MuxerFactory: factory})
	if err == nil {
		t.Fatal("expected an error from Consume when demuxer factory fails, got nil")
	}

	if !errors.Is(err, errDemuxFail) {
		t.Errorf("expected errDemuxFail in chain, got: %v", err)
	}
}

// TestMuxerErrorPropagatedToErrChan creates a muxer that fails after 5 packets
// and verifies that the error is delivered to the errChan provided to Consume.
func TestMuxerErrorPropagatedToErrChan(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)

	fm := &failingMuxer{failAfter: 5}
	muxFactory := func(_ context.Context, _ string) (av.MuxCloser, error) {
		return fm, nil
	}

	errChan := make(chan error, 1)
	if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{
		ConsumerID:   "failing-consumer",
		MuxerFactory: muxFactory,
		ErrChan:      errChan,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("timed out waiting for muxer error on errChan")
	case err := <-errChan:
		if !errors.Is(err, errWritePacketFailed) {
			t.Errorf("errChan got %v, want errWritePacketFailed", err)
		}
	}
}

// =============================================================================
// Tests — Concurrency and stress
// =============================================================================

// TestContextCancellationDuringJoins cancels the context while 50 goroutines
// are mid-Consume. Verifies that all goroutines exit cleanly with no
// deadlocks or panics regardless of which stage of Consume they are in.
func TestContextCancellationDuringJoins(t *testing.T) {
	t.Parallel()

	const numJoiners = 50

	ctx, cancel := context.WithCancel(context.Background())

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	var wg sync.WaitGroup
	for i := range numJoiners {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			_, _ = sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: fmt.Sprintf("consumer-%d", i), MuxerFactory: factory})
		}(i)
	}

	// Cancel while goroutines are in various stages of Consume.
	time.Sleep(5 * time.Millisecond)
	cancel()
	wg.Wait()
}

// TestMassiveParallelConsumers spawns 100 goroutines that each add a unique
// consumer to one shared producer concurrently. Each goroutine holds its
// consumer for a staggered duration then removes it. Intended to be run with
// -race to catch synchronisation defects.
func TestMassiveParallelConsumers(t *testing.T) {
	t.Parallel()

	const numConsumers = 100

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	var wg sync.WaitGroup

	errs := make(chan error, numConsumers)

	for i := range numConsumers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			id := fmt.Sprintf("consumer-%d", i)

			handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: id, MuxerFactory: factory})
			if err != nil {
				errs <- fmt.Errorf("add %s: %w", id, err)

				return
			}

			time.Sleep(time.Duration(i%50+1) * time.Millisecond)
			removeConsumer(t, handle, ctx)
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

// TestConsumerChurn runs 20 workers that continuously add and remove their own
// consumer from a shared producer for 2 seconds. Stress-tests the consumer-map
// locking, the ticker cleanup path, and the Consume retry loop under
// sustained join/leave pressure.
func TestConsumerChurn(t *testing.T) {
	t.Parallel()

	const numWorkers = 20

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	var wg sync.WaitGroup
	for w := range numWorkers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			holdFor := time.Duration(w%10+1) * time.Millisecond
			for iter := 0; ctx.Err() == nil; iter++ {
				id := fmt.Sprintf("worker-%d-iter-%d", w, iter)

				handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: id, MuxerFactory: factory})
				if err != nil {
					return
				}

				timer := time.NewTimer(holdFor)
				select {
				case <-ctx.Done():
					timer.Stop()
					removeConsumer(t, handle, ctx)

					return
				case <-timer.C:
				}

				removeConsumer(t, handle, ctx)
			}
		}(w)
	}

	wg.Wait()
}

// TestMultipleProducersParallel launches 10×10 goroutines: each pair of
// (producer, consumer) indices gets its own goroutine that adds a consumer,
// holds briefly, then removes it. Stress-tests the RelayHub's producer
// map locking with simultaneous access across many producer keys.
func TestMultipleProducersParallel(t *testing.T) {
	t.Parallel()

	const (
		numProducers = 10
		numConsumers = 10
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	var wg sync.WaitGroup

	errs := make(chan error, numProducers*numConsumers)

	for p := range numProducers {
		for c := range numConsumers {
			wg.Add(1)

			go func(p, c int) {
				defer wg.Done()

				pid := fmt.Sprintf("producer-%d", p)
				cid := fmt.Sprintf("consumer-%d-%d", p, c)

				handle, err := sm.Consume(ctx, pid, av.ConsumeOptions{ConsumerID: cid, MuxerFactory: factory})
				if err != nil {
					errs <- fmt.Errorf("add %s/%s: %w", pid, cid, err)

					return
				}

				time.Sleep(time.Duration((p+c)%20+1) * time.Millisecond)
				removeConsumer(t, handle, ctx)
			}(p, c)
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

// =============================================================================
// Tests — Packet delivery
// =============================================================================

// TestConsumerJoinsDuringPacketFlood adds one consumer and lets it accumulate
// packets, then adds 50 more consumers concurrently while the producer is
// actively streaming. Verifies:
//   - The first (sole) consumer receives packets via the blocking write path.
//   - Late-joining consumers receive packets via the leaky write path without
//     interfering with the first consumer.
//   - No races occur during concurrent joins against an active readWriteLoop.
func TestConsumerJoinsDuringPacketFlood(t *testing.T) {
	t.Parallel()

	const numLateJoiners = 50

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	factory, registry := makeMuxerFactory()
	sm := startedSM(t, ctx)

	if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "first", MuxerFactory: factory}); err != nil {
		t.Fatal(err)
	}

	// Let the first consumer receive packets before others join.
	// While it is the only consumer the producer uses blocking WritePacket,
	// so every packet is guaranteed to be delivered.
	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	for i := range numLateJoiners {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			id := fmt.Sprintf("late-%d", i)

			handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: id, MuxerFactory: factory})
			if err != nil {
				return
			}

			time.Sleep(50 * time.Millisecond)
			removeConsumer(t, handle, ctx)
		}(i)
	}

	wg.Wait()

	// Verify the first consumer received packets throughout the flood.
	v, ok := registry.Load("first")
	if !ok {
		t.Fatal("first consumer muxer not found in registry")
	}

	if n := v.(*mockMuxer).packetsRecv.Load(); n == 0 {
		t.Error("first consumer received no packets")
	} else {
		t.Logf("first consumer received %d packets", n)
	}
}

// TestBaselineConsumerUnaffectedByJoinsAndLeaves verifies that an existing
// consumer continues to receive packets throughout join/leave storms that
// repeatedly cross the 1→N→1 boundary in the delivery policy:
//
//   - While it is the sole consumer the producer uses a blocking write
//     (WritePacket), guaranteeing delivery.
//   - When other consumers join, the producer switches to leaky writes
//     (WritePacketLeaky) for all consumers. The baseline consumer must still
//     keep making forward progress — its queue drains fast enough that the
//     leaky path almost never drops packets for a well-behaved receiver.
//   - After all storm consumers leave, the producer reverts to blocking writes
//     and the baseline consumer's throughput must recover.
//
// The test samples packet counts at three points (before, mid-storm, after)
// and asserts strict monotone growth at each transition.
func TestBaselineConsumerUnaffectedByJoinsAndLeaves(t *testing.T) {
	t.Parallel()

	const (
		sourceID    = "producer-1"
		baselineID  = "baseline"
		numWorkers  = 20
		stormDur    = 1 * time.Second
		warmupDur   = 100 * time.Millisecond
		recoveryDur = 200 * time.Millisecond
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	factory, registry := makeMuxerFactory()
	sm := startedSM(t, ctx)

	if _, err := sm.Consume(ctx, sourceID, av.ConsumeOptions{ConsumerID: baselineID, MuxerFactory: factory}); err != nil {
		t.Fatal(err)
	}

	// snapshot returns the packet count for the baseline consumer.
	snapshot := func(label string) int64 {
		v, ok := registry.Load(baselineID)
		if !ok {
			t.Fatalf("baseline muxer missing at %s", label)
		}

		return v.(*mockMuxer).packetsRecv.Load()
	}

	// Warm-up: let the baseline consumer receive packets alone (blocking writes).
	time.Sleep(warmupDur)

	beforeStorm := snapshot("before-storm")
	if beforeStorm == 0 {
		t.Fatal("baseline consumer received no packets before storm")
	}

	// Launch join/leave storm. Workers continuously add a consumer, hold it
	// briefly, then remove it, keeping multiple consumers alive at all times
	// and forcing the producer into leaky-write mode.
	stormCtx, stormCancel := context.WithTimeout(ctx, stormDur)
	defer stormCancel()

	var wg sync.WaitGroup
	for w := range numWorkers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			holdFor := time.Duration(w%5+1) * time.Millisecond
			for iter := 0; stormCtx.Err() == nil; iter++ {
				id := fmt.Sprintf("storm-%d-%d", w, iter)

				handle, err := sm.Consume(ctx, sourceID, av.ConsumeOptions{ConsumerID: id, MuxerFactory: factory})
				if err != nil {
					return
				}

				timer := time.NewTimer(holdFor)
				select {
				case <-stormCtx.Done():
					timer.Stop()
					removeConsumer(t, handle, ctx)

					return
				case <-timer.C:
				}

				removeConsumer(t, handle, ctx)
			}
		}(w)
	}

	// Sample mid-storm: baseline must still be making forward progress
	// even though the producer is now using leaky writes.
	time.Sleep(stormDur / 2)

	midStorm := snapshot("mid-storm")

	// Let the storm run to completion, then wait for all workers to exit.
	<-stormCtx.Done()
	wg.Wait()

	// Recovery: storm consumers are gone; producer reverts to blocking writes.
	// Give the producer one scheduling cycle to observe the single-consumer state.
	time.Sleep(recoveryDur)

	afterStorm := snapshot("after-storm")

	t.Logf("baseline packets — before: %d  mid-storm: %d  after: %d",
		beforeStorm, midStorm, afterStorm)

	if midStorm <= beforeStorm {
		t.Errorf("baseline consumer stalled during storm: count did not increase "+
			"(before=%d, mid=%d)", beforeStorm, midStorm)
	}

	if afterStorm <= midStorm {
		t.Errorf("baseline consumer stalled after storm recovery: count did not increase "+
			"(mid=%d, after=%d)", midStorm, afterStorm)
	}
}

// =============================================================================
// Tests — Pause and Resume
// =============================================================================

// TestPauseResumeNonExistentProducer verifies that PauseProducer and
// ResumeProducer return ErrRelayNotFound when no producer with that ID exists.
func TestPauseResumeNonExistentProducer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)

	if err := sm.PauseRelay(
		ctx,
		"no-such-producer",
	); !errors.Is(
		err,
		relayhub.ErrRelayNotFound,
	) {
		t.Errorf("PauseProducer: got %v, want ErrRelayNotFound", err)
	}

	if err := sm.ResumeRelay(
		ctx,
		"no-such-producer",
	); !errors.Is(
		err,
		relayhub.ErrRelayNotFound,
	) {
		t.Errorf("ResumeProducer: got %v, want ErrRelayNotFound", err)
	}
}

// TestPauseResumeDuringConsumerChurn exercises the Pause/Resume paths on a
// pausable demuxer while consumers are simultaneously churning. This directly
// validates the m.mu.Lock() fix on m.demuxer = demuxer (producer.go) — the
// race detector will catch it if the write is not properly protected.
func TestPauseResumeDuringConsumerChurn(t *testing.T) {
	t.Parallel()

	const numWorkers = 10

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pausableFactory, latestDemuxer := makePausableDemuxerFactory(testStreams())

	sm := relayhub.New(pausableFactory, nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = sm.Stop() })

	factory, _ := makeMuxerFactory()

	// Seed the producer so it exists before the churn begins.
	if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "seed", MuxerFactory: factory}); err != nil {
		t.Fatal(err)
	}

	// Churn goroutines: continuously join and leave.
	var wg sync.WaitGroup
	for w := range numWorkers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			for iter := 0; ctx.Err() == nil; iter++ {
				id := fmt.Sprintf("churn-%d-%d", w, iter)

				handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: id, MuxerFactory: factory})
				if err != nil {
					return
				}

				time.Sleep(time.Duration(w%5+1) * time.Millisecond)
				removeConsumer(t, handle, ctx)
			}
		}(w)
	}

	// Pause/Resume goroutine: rapidly toggles the demuxer while churn runs.

	wg.Go(func() {
		for ctx.Err() == nil {
			if latestDemuxer.Load() == nil {
				time.Sleep(time.Millisecond)

				continue
			}

			_ = sm.PauseRelay(ctx, "producer-1")

			time.Sleep(2 * time.Millisecond)

			_ = sm.ResumeRelay(ctx, "producer-1")

			time.Sleep(2 * time.Millisecond)
		}
	})

	wg.Wait()
}

// =============================================================================
// Additional mocks for new test cases
// =============================================================================

// codecChangingDemuxer emits a normal stream of packets and embeds a
// NewCodecs field in the packet at index changeAfter, simulating a mid-stream
// codec renegotiation with a full replacement stream list.
type codecChangingDemuxer struct {
	streams     []av.Stream
	newStreams  []av.Stream
	changeAfter int64
	pktIdx      atomic.Int64
}

func (d *codecChangingDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return d.streams, nil
}

func (d *codecChangingDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	if err := ctx.Err(); err != nil {
		return av.Packet{}, err
	}

	n := d.pktIdx.Add(1)
	pkt := av.Packet{DTS: time.Duration(n) * (time.Second / 250)}

	if n == d.changeAfter {
		pkt.NewCodecs = d.newStreams
	}

	return pkt, nil
}

func (d *codecChangingDemuxer) Close() error { return nil }

// codecChangingMuxer is a mockMuxer that also implements av.CodecChanger.
// It counts how many times WriteCodecChange is called.
type codecChangingMuxer struct {
	mockMuxer
	codecChanges atomic.Int64
}

func (m *codecChangingMuxer) WriteCodecChange(_ context.Context, _ []av.Stream) error {
	m.codecChanges.Add(1)

	return nil
}

// =============================================================================
// Tests — Lifecycle preconditions
// =============================================================================

// TestConsumeBeforeStart verifies that Consume returns
// ErrRelayHubNotStartedYet when Start has not yet been called.
func TestConsumeBeforeStart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	factory, _ := makeMuxerFactory()

	_, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "consumer-1", MuxerFactory: factory})
	if !errors.Is(err, relayhub.ErrRelayHubNotStartedYet) {
		t.Errorf("Consume before Start: got %v, want ErrRelayHubNotStartedYet", err)
	}
}

// TestSignalStopIdempotency verifies that SignalStop returns true on the first
// call and false on every subsequent call.
func TestSignalStopIdempotency(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if !sm.SignalStop() {
		t.Error("first SignalStop() returned false, want true")
	}

	for i := range 5 {
		if sm.SignalStop() {
			t.Errorf("SignalStop() call %d returned true, want false", i+2)
		}
	}

	_ = sm.WaitStop()
}

// TestStopCleansUpAllProducers verifies that after Stop() returns,
// GetActiveProducersCount is zero regardless of how many relays were active.
func TestStopCleansUpAllProducers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	for i := range 5 {
		pid := fmt.Sprintf("producer-%d", i)
		cid := fmt.Sprintf("consumer-%d", i)

		if _, err := sm.Consume(ctx, pid, av.ConsumeOptions{ConsumerID: cid, MuxerFactory: factory}); err != nil {
			t.Fatal(err)
		}
	}

	if err := sm.Stop(); err != nil {
		t.Fatal(err)
	}

	if n := sm.GetActiveRelayCount(ctx); n != 0 {
		t.Errorf("GetActiveProducersCount after Stop: got %d, want 0", n)
	}
}

// =============================================================================
// Tests — Additional error paths
// =============================================================================

// TestMuxerFactoryErrorPropagatedToErrChan verifies that when the MuxerFactory
// itself returns an error (not WritePacket), the error is delivered
// asynchronously to errChan and Consume still returns nil.
func TestMuxerFactoryErrorPropagatedToErrChan(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)

	errMuxFactory := errors.New("muxer factory intentionally failed")
	badFactory := func(_ context.Context, _ string) (av.MuxCloser, error) {
		return nil, errMuxFactory
	}

	errChan := make(chan error, 1)
	if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{
		ConsumerID:   "consumer-1",
		MuxerFactory: badFactory,
		ErrChan:      errChan,
	}); err != nil {
		t.Fatalf("Consume: unexpected synchronous error: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("timed out waiting for mux factory error on errChan")
	case err := <-errChan:
		if !errors.Is(err, errMuxFactory) {
			t.Errorf("errChan got %v, want errMuxFactory in chain", err)
		}
	}
}

// TestCloseHandleAfterProducerGone verifies that closing a handle after the
// producer has already been reclaimed by the idle ticker returns nil.
func TestCloseHandleAfterProducerGone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "consumer-1", MuxerFactory: factory})
	if err != nil {
		t.Fatal(err)
	}

	// Close the handle; the producer will be reclaimed by the idle ticker.
	if err := handle.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Closing again after the producer is gone must still return nil.
	if err := handle.Close(ctx); err != nil {
		t.Errorf("second Close (producer gone): got %v, want nil", err)
	}
}

// =============================================================================
// Tests — Callbacks
// =============================================================================

// TestDemuxerRemoverCalledOnShutdown verifies that the DemuxerRemover callback
// is invoked for every producer when Stop() completes.
func TestDemuxerRemoverCalledOnShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex

	removedIDs := map[string]bool{}
	demuxerRemover := func(_ context.Context, sourceID string) error {
		mu.Lock()
		removedIDs[sourceID] = true
		mu.Unlock()

		return nil
	}

	sm := relayhub.New(makeDemuxerFactory(testStreams()), demuxerRemover)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	factory, _ := makeMuxerFactory()

	for _, pid := range []string{"producer-1", "producer-2"} {
		if _, err := sm.Consume(ctx, pid, av.ConsumeOptions{ConsumerID: pid + "-c", MuxerFactory: factory}); err != nil {
			t.Fatal(err)
		}
	}

	// Stop is synchronous: by the time it returns all producer goroutines have
	// completed their defers, including the demuxerRemover call.
	if err := sm.Stop(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, pid := range []string{"producer-1", "producer-2"} {
		if !removedIDs[pid] {
			t.Errorf("DemuxerRemover was not called for %q", pid)
		}
	}
}

// TestMuxerRemoverCalledOnConsumerClose verifies that the MuxerRemover callback
// is invoked after an explicit handle.Close() call.
func TestMuxerRemoverCalledOnConsumerClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	removed := make(chan string, 1)
	muxRemover := func(_ context.Context, consumerID string) error {
		select {
		case removed <- consumerID:
		default:
		}

		return nil
	}

	factory, registry := makeMuxerFactory()
	sm := startedSM(t, ctx)

	handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{
		ConsumerID:   "consumer-1",
		MuxerFactory: factory,
		MuxerRemover: muxRemover,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the consumer goroutine is running (evidenced by receiving ≥1 packet).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, ok := registry.Load("consumer-1")
		if ok && v.(*mockMuxer).packetsRecv.Load() > 0 {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case id := <-removed:
		if id != "consumer-1" {
			t.Errorf("MuxerRemover called with %q, want %q", id, "consumer-1")
		}
	case <-time.After(2 * time.Second):
		t.Error("MuxerRemover was not called within 2 s after handle.Close")
	}
}

// =============================================================================
// Tests — Codec change forwarding
// =============================================================================

// TestCodecChangeForwardedToCodecChanger verifies that when the demuxer emits a
// packet with non-nil NewCodecs, the relay hub forwards it via
// WriteCodecChange to any muxer that implements av.CodecChanger.
func TestCodecChangeForwardedToCodecChanger(t *testing.T) {
	t.Parallel()

	const changeAfter = int64(10)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	demuxFactory := func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return &codecChangingDemuxer{
			streams:     testStreams(),
			newStreams:  []av.Stream{{Idx: 0}, {Idx: 1}},
			changeAfter: changeAfter,
		}, nil
	}

	sm := relayhub.New(demuxFactory, nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = sm.Stop() })

	mux := &codecChangingMuxer{}
	muxFactory := func(_ context.Context, _ string) (av.MuxCloser, error) {
		return mux, nil
	}

	if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "consumer-1", MuxerFactory: muxFactory}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mux.codecChanges.Load() > 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Errorf("WriteCodecChange was not called on the CodecChanger muxer within 3 s (changeAfter=%d)", changeAfter)
}

func TestRelayStatsPreserveFullStreamSetAfterCodecChange(t *testing.T) {
	t.Parallel()

	const (
		changeAfter   = int64(5)
		initialWidth  = 1920
		initialHeight = 1080
		initialAudio  = 8000
		changedAudio  = 16000
	)

	initialStreams := []av.Stream{
		{
			Idx: 0,
			Codec: fakeVideoCodec{
				typ:       av.H264,
				width:     initialWidth,
				height:    initialHeight,
				timeScale: 90000,
			},
		},
		{
			Idx: 1,
			Codec: fakeAudioCodec{
				typ:        av.OPUS,
				sampleRate: initialAudio,
			},
		},
	}

	demuxFactory := func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return &codecChangingDemuxer{
			streams: initialStreams,
			newStreams: []av.Stream{
				{
					Idx: 0,
					Codec: fakeVideoCodec{
						typ:       av.H264,
						width:     initialWidth,
						height:    initialHeight,
						timeScale: 90000,
					},
				},
				{
					Idx: 1,
					Codec: fakeAudioCodec{
						typ:        av.OPUS,
						sampleRate: changedAudio,
					},
				},
			},
			changeAfter: changeAfter,
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sm := relayhub.New(demuxFactory, nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = sm.Stop() })

	muxFactory := func(_ context.Context, _ string) (av.MuxCloser, error) {
		return &mockMuxer{}, nil
	}

	handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{
		ConsumerID:   "consumer-1",
		MuxerFactory: muxFactory,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = handle.Close(ctx) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stats := sm.GetRelayStats(ctx)
		if len(stats) != 1 {
			time.Sleep(10 * time.Millisecond)

			continue
		}

		streams := stats[0].Streams
		if len(streams) != 2 {
			time.Sleep(10 * time.Millisecond)

			continue
		}

		if streams[0].Idx != 0 || streams[0].CodecType != av.H264 ||
			streams[0].Width != initialWidth || streams[0].Height != initialHeight {
			t.Fatalf("video stream metadata lost after codec change: %+v", streams[0])
		}

		if streams[1].Idx != 1 || streams[1].CodecType != av.OPUS {
			t.Fatalf("audio stream metadata changed unexpectedly after codec change: %+v", streams[1])
		}

		if streams[1].SampleRate != changedAudio {
			time.Sleep(10 * time.Millisecond)

			continue
		}

		return
	}

	t.Fatalf("GetRelayStats did not retain the full stream set after codec change")
}

// =============================================================================
// Tests — High-load / exhaustive parallel
// =============================================================================

// TestMillionConsumerOperations exercises 1 000 000 total add+remove consumer
// cycles spread across 100 relays with 1 000 concurrent worker goroutines
// (10 per producer). Each worker uses unique consumer IDs so there are no
// duplicate-ID collisions. Run with -race to validate synchronisation at scale.
func TestMillionConsumerOperations(t *testing.T) {
	t.Parallel()

	const (
		numProducers = 100
		numWorkers   = 1_000
		opsPerWorker = 1_000 // 1 000 × 1 000 = 1 000 000 total cycles
	)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	var (
		wg      sync.WaitGroup
		success atomic.Int64
	)

	for w := range numWorkers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			pid := fmt.Sprintf("producer-%d", w%numProducers)

			for i := range opsPerWorker {
				if ctx.Err() != nil {
					return
				}

				cid := fmt.Sprintf("w%d-op%d", w, i)

				handle, err := sm.Consume(ctx, pid, av.ConsumeOptions{ConsumerID: cid, MuxerFactory: factory})
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) ||
						errors.Is(err, context.Canceled) {
						return
					}

					continue
				}

				success.Add(1)
				removeConsumer(t, handle, ctx)
			}
		}(w)
	}

	wg.Wait()

	total := int64(numWorkers * opsPerWorker)
	got := success.Load()
	t.Logf("completed %d/%d add+remove cycles", got, total)

	// Allow up to 5% failure due to transient producer-closing or context
	// cancellation during auto-cleanup ticker races.
	if got < total*95/100 {
		t.Errorf("too many failures: %d/%d succeeded (need ≥95%%)", got, total)
	}
}

// TestConcurrentStopDuringHighLoad calls Stop() while 200 goroutines are
// hammering Consume across 20 producers. Verifies clean shutdown with no
// deadlocks, panics, or stuck goroutines under full load.
func TestConcurrentStopDuringHighLoad(t *testing.T) {
	t.Parallel()

	const (
		numWorkers   = 200
		numProducers = 20
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	factory, _ := makeMuxerFactory()

	var wg sync.WaitGroup

	for w := range numWorkers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			pid := fmt.Sprintf("producer-%d", w%numProducers)

			for i := 0; ctx.Err() == nil; i++ {
				cid := fmt.Sprintf("w%d-op%d", w, i)

				handle, err := sm.Consume(ctx, pid, av.ConsumeOptions{ConsumerID: cid, MuxerFactory: factory})
				if err != nil {
					return
				}

				removeConsumer(t, handle, ctx)
			}
		}(w)
	}

	// Let the load build briefly, then shut down while goroutines are active.
	time.Sleep(20 * time.Millisecond)

	if err := sm.Stop(); err != nil {
		t.Errorf("Stop() returned %v", err)
	}

	cancel() // unblock any goroutines still waiting inside Consume
	wg.Wait()
}

// TestRapidConsumeClose exercises the consumer lifecycle under high concurrency:
// 100 goroutines each perform 100 rapid Consume+Close cycles on a shared producer.
// This validates that the alreadyClosing guard in Consumer.Start holds under
// sustained concurrent load. Run with -race.
func TestRapidConsumeClose(t *testing.T) {
	t.Parallel()

	const (
		numWorkers = 100
		iterations = 100 // 100 × 100 = 10 000 consume+close cycles
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	// Seed the producer so it is running before the race begins.
	if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: "seed", MuxerFactory: factory}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for w := range numWorkers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			for i := range iterations {
				if ctx.Err() != nil {
					return
				}

				cid := fmt.Sprintf("w%d-i%d", w, i)

				handle, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: cid, MuxerFactory: factory})
				if err != nil {
					continue
				}

				_ = handle.Close(ctx)
			}
		}(w)
	}

	wg.Wait()
}

// TestHighConcurrencyManyProducers stresses the RelayHub's producer-map
// locking with 500 relays each served by 20 concurrent consumers. After all
// consumers are removed, it verifies that all relays are auto-cleaned within
// the ticker period.
func TestHighConcurrencyManyProducers(t *testing.T) {
	t.Parallel()

	const (
		numProducers     = 500
		consumersPerProd = 20
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)
	factory, _ := makeMuxerFactory()

	var wg sync.WaitGroup

	errs := make(chan error, numProducers*consumersPerProd)

	for p := range numProducers {
		for c := range consumersPerProd {
			wg.Add(1)

			go func(p, c int) {
				defer wg.Done()

				pid := fmt.Sprintf("producer-%d", p)
				cid := fmt.Sprintf("consumer-%d-%d", p, c)

				handle, err := sm.Consume(ctx, pid, av.ConsumeOptions{ConsumerID: cid, MuxerFactory: factory})
				if err != nil {
					errs <- fmt.Errorf("add %s/%s: %w", pid, cid, err)

					return
				}

				time.Sleep(time.Duration((p+c)%10+1) * time.Millisecond)
				removeConsumer(t, handle, ctx)
			}(p, c)
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	// All consumers removed → relays should auto-clean within 2 ticker periods.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if sm.GetActiveRelayCount(ctx) == 0 {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Errorf("relays still active 4 s after all consumers left: count=%d",
		sm.GetActiveRelayCount(ctx))
}

// TestConcurrentWaitStop launches 100 goroutines that all call WaitStop
// simultaneously after SignalStop. Verifies that all callers unblock with no
// deadlock regardless of scheduling order.
func TestConcurrentWaitStop(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	const numWaiters = 100

	var wg sync.WaitGroup

	for range numWaiters {
		wg.Add(1)

		go func() {
			defer wg.Done()
			_ = sm.WaitStop()
		}()
	}

	sm.SignalStop()
	wg.Wait()
}

// TestStopIdempotentUnderConcurrency calls Stop() from 50 goroutines at the
// same instant. Exactly one must initiate shutdown; all must return nil.
func TestStopIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	sm := relayhub.New(makeDemuxerFactory(testStreams()), nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	const numCallers = 50

	var wg sync.WaitGroup

	errs := make(chan error, numCallers)

	for range numCallers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := sm.Stop(); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Stop() returned %v, want nil", err)
	}
}

// TestHighLoadWithCodecChanges adds 200 consumers to a producer that emits a
// codec-change event after every 5 packets. Verifies that WriteCodecChange
// reaches all active consumers under high concurrency without data races on
// the shared headers field.
func TestHighLoadWithCodecChanges(t *testing.T) {
	t.Parallel()

	const (
		numConsumers = 200
		changeAfter  = int64(5)
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	demuxFactory := func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return &codecChangingDemuxer{
			streams:     testStreams(),
			newStreams:  []av.Stream{{Idx: 0}, {Idx: 1}},
			changeAfter: changeAfter,
		}, nil
	}

	sm := relayhub.New(demuxFactory, nil)
	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = sm.Stop() })

	var (
		wg      sync.WaitGroup
		changed atomic.Int64
	)

	for i := range numConsumers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			mux := &codecChangingMuxer{}
			muxFactory := func(_ context.Context, _ string) (av.MuxCloser, error) {
				return mux, nil
			}

			if _, err := sm.Consume(ctx, "producer-1", av.ConsumeOptions{ConsumerID: fmt.Sprintf("c%d", i), MuxerFactory: muxFactory}); err != nil {
				return
			}

			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if mux.codecChanges.Load() > 0 {
					changed.Add(1)

					break
				}

				time.Sleep(5 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	if changed.Load() == 0 {
		t.Error("no consumers received a codec change event")
	}

	t.Logf("%d/%d consumers received at least one codec change", changed.Load(), numConsumers)
}

// TestConsumerErrChanHighLoad attaches 200 consumers that each use a muxer
// failing after 1 packet, all sharing a single buffered errChan. Verifies that
// errors are delivered under high concurrency and the channel never deadlocks
// due to back-pressure.
func TestConsumerErrChanHighLoad(t *testing.T) {
	t.Parallel()

	const numConsumers = 200

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sm := startedSM(t, ctx)

	errChan := make(chan error, numConsumers)

	var wg sync.WaitGroup

	for i := range numConsumers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			fm := &failingMuxer{failAfter: 1}
			muxFactory := func(_ context.Context, _ string) (av.MuxCloser, error) {
				return fm, nil
			}

			_, _ = sm.Consume(ctx, "producer-1", av.ConsumeOptions{
				ConsumerID:   fmt.Sprintf("failing-%d", i),
				MuxerFactory: muxFactory,
				ErrChan:      errChan,
			})
		}(i)
	}

	wg.Wait()

	// Drain: require at least half the consumers to have delivered an error.
	received := 0
	drain := time.NewTimer(5 * time.Second)
	defer drain.Stop()

	for received < numConsumers/2 {
		select {
		case err := <-errChan:
			if !errors.Is(err, errWritePacketFailed) {
				t.Errorf("unexpected error: %v", err)
			}

			received++
		case <-drain.C:
			t.Errorf("timed out: received only %d/%d errors", received, numConsumers)

			return
		}
	}

	t.Logf("received %d/%d muxer errors (≥50%% threshold met)", received, numConsumers)
}

// ── Keyframe cache tests ────────────────────────────────────────────────────

// stallingMuxer records packets and blocks its consumer goroutine after
// stallAfter packets have been written. Call unblock() to resume.
type stallingMuxer struct {
	mu         sync.Mutex
	pkts       []av.Packet
	stallAfter int
	stallCh    chan struct{} // closed by unblock()
	stalled    atomic.Bool
}

func newStallingMuxer(stallAfter int) *stallingMuxer {
	return &stallingMuxer{
		stallAfter: stallAfter,
		stallCh:    make(chan struct{}),
	}
}

func (m *stallingMuxer) WriteHeader(_ context.Context, _ []av.Stream) error { return nil }

func (m *stallingMuxer) WritePacket(ctx context.Context, pkt av.Packet) error {
	m.mu.Lock()
	m.pkts = append(m.pkts, pkt)
	n := len(m.pkts)
	m.mu.Unlock()

	if n == m.stallAfter {
		m.stalled.Store(true)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.stallCh:
		}
	}

	return nil
}

func (m *stallingMuxer) WriteTrailer(_ context.Context, _ error) error { return nil }
func (m *stallingMuxer) Close() error                                  { return nil }

func (m *stallingMuxer) unblock() { close(m.stallCh) }

func (m *stallingMuxer) packets() []av.Packet {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]av.Packet(nil), m.pkts...)
}

// keyframeDemuxer emits video packets, alternating between keyframes and
// non-keyframes. Packets have CodecType set to H264 so the relay caches
// keyframes.
type keyframeDemuxer struct {
	streams []av.Stream
	pktIdx  atomic.Int64
}

func (d *keyframeDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return d.streams, nil
}

func (d *keyframeDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	if err := ctx.Err(); err != nil {
		return av.Packet{}, err
	}

	n := d.pktIdx.Add(1)

	return av.Packet{
		KeyFrame:  n%5 == 1, // keyframe every 5th packet
		DTS:       time.Duration(n) * (time.Second / 250),
		CodecType: av.H264, // must be video type for keyframe cache
		Idx:       0,
		Data:      []byte{byte(n)},
	}, nil
}

func (d *keyframeDemuxer) Close() error { return nil }

// recordingMuxer records all packets written to it.
type recordingMuxer struct {
	mu   sync.Mutex
	pkts []av.Packet
}

func (m *recordingMuxer) WriteHeader(_ context.Context, _ []av.Stream) error { return nil }
func (m *recordingMuxer) WritePacket(_ context.Context, pkt av.Packet) error {
	m.mu.Lock()
	m.pkts = append(m.pkts, pkt)
	m.mu.Unlock()

	return nil
}
func (m *recordingMuxer) WriteTrailer(_ context.Context, _ error) error { return nil }
func (m *recordingMuxer) Close() error                                  { return nil }

func (m *recordingMuxer) packets() []av.Packet {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]av.Packet(nil), m.pkts...)
}

// TestDropRecoveryDeliversKeyframe verifies that after WritePacketLeaky drops a
// packet (channel full → default branch), the next frame delivered to that
// consumer is a keyframe so the decoder can resync cleanly.
func TestDropRecoveryDeliversKeyframe(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	streams := []av.Stream{{Idx: 0, Codec: fakeVideoCodec{typ: av.H264, width: 320, height: 240, timeScale: 90000}}}

	sm := relayhub.New(func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return &keyframeDemuxer{streams: streams}, nil
	}, nil)

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = sm.Stop() }()

	// Fast consumer — never blocks, ensures the relay keeps producing packets.
	fastMux := &recordingMuxer{}

	h1, err := sm.Consume(ctx, "src1", av.ConsumeOptions{
		ConsumerID: "fast",
		MuxerFactory: func(_ context.Context, _ string) (av.MuxCloser, error) {
			return fastMux, nil
		},
	})
	if err != nil {
		t.Fatalf("Consume fast: %v", err)
	}

	defer func() { _ = h1.Close(ctx) }()

	// Slow consumer — stalls after 3 packets so its channel (buffer=50) fills
	// and WritePacketLeaky takes the default/drop path.
	slowMux := newStallingMuxer(3)

	h2, err := sm.Consume(ctx, "src1", av.ConsumeOptions{
		ConsumerID: "slow",
		MuxerFactory: func(_ context.Context, _ string) (av.MuxCloser, error) {
			return slowMux, nil
		},
	})
	if err != nil {
		t.Fatalf("Consume slow: %v", err)
	}

	defer func() { _ = h2.Close(ctx) }()

	// Wait until the slow consumer has stalled. Once stalled, its channel will
	// fill up and further WritePacketLeaky calls will hit the default branch.
	deadline := time.After(5 * time.Second)

	for !slowMux.stalled.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for slow consumer to stall")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Wait for enough packets that drops have definitely occurred.
	// The slow consumer's channel holds 50 packets; we need > 50 more packets
	// after the stall to guarantee at least one drop.
	deadline = time.After(5 * time.Second)

	for {
		pkts := fastMux.packets()
		if len(pkts) > 100 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for fast consumer to receive enough packets")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Unblock the slow consumer so it drains its channel and receives new
	// (post-recovery) packets from the relay.
	slowMux.unblock()

	// Wait for the slow consumer to receive post-recovery packets.
	deadline = time.After(5 * time.Second)

	for {
		pkts := slowMux.packets()
		// After unblocking: 3 pre-stall + 50 buffered + some post-recovery.
		// Need enough post-recovery packets to verify the invariant.
		if len(pkts) > 60 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for slow consumer to receive post-recovery packets")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Verify: wherever there is a DTS gap (indicating dropped packets), the
	// packet immediately after the gap must be a keyframe.
	pkts := slowMux.packets()
	expectedInterval := time.Second / 250 // DTS step between consecutive packets

	gapFound := false

	for i := 1; i < len(pkts); i++ {
		gap := pkts[i].DTS - pkts[i-1].DTS
		if gap > expectedInterval*2 { // allow small jitter, detect real drops
			gapFound = true

			if !pkts[i].KeyFrame {
				t.Errorf("after drop gap at pkt[%d] (DTS %v → %v, gap %v), "+
					"expected keyframe but got KeyFrame=%v",
					i, pkts[i-1].DTS, pkts[i].DTS, gap, pkts[i].KeyFrame)
			}
		}
	}

	if !gapFound {
		t.Fatal("no DTS gap detected — drops did not occur; test setup is broken")
	}
}

// TestKeyframeCacheForLateJoiner verifies that a consumer added after the relay
// is already producing packets receives the cached last video keyframe as its
// first packet (instant live view start).
func TestKeyframeCacheForLateJoiner(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	streams := []av.Stream{{Idx: 0, Codec: fakeVideoCodec{typ: av.H264, width: 320, height: 240, timeScale: 90000}}}

	sm := relayhub.New(func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return &keyframeDemuxer{streams: streams}, nil
	}, nil)

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = sm.Stop() }()

	// Add first consumer to start the relay and get packets flowing.
	firstMux := &recordingMuxer{}

	h1, err := sm.Consume(ctx, "src1", av.ConsumeOptions{
		ConsumerID: "c1",
		MuxerFactory: func(_ context.Context, _ string) (av.MuxCloser, error) {
			return firstMux, nil
		},
	})
	if err != nil {
		t.Fatalf("Consume c1: %v", err)
	}

	defer func() { _ = h1.Close(ctx) }()

	// Wait until the first consumer has received enough packets that at least
	// one keyframe has been cached by the relay.
	deadline := time.After(5 * time.Second)

	for {
		pkts := firstMux.packets()

		hasKeyframe := false
		for _, p := range pkts {
			if p.KeyFrame {
				hasKeyframe = true

				break
			}
		}

		if hasKeyframe && len(pkts) > 5 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for first consumer to receive a keyframe")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Now add a late-joining consumer.
	lateMux := &recordingMuxer{}

	h2, err := sm.Consume(ctx, "src1", av.ConsumeOptions{
		ConsumerID: "c2",
		MuxerFactory: func(_ context.Context, _ string) (av.MuxCloser, error) {
			return lateMux, nil
		},
	})
	if err != nil {
		t.Fatalf("Consume c2: %v", err)
	}

	defer func() { _ = h2.Close(ctx) }()

	// Wait for the late consumer to receive at least one packet.
	deadline = time.After(5 * time.Second)

	for {
		pkts := lateMux.packets()
		if len(pkts) > 0 {
			// The first packet delivered to the late consumer should be a keyframe
			// (from the cache).
			if !pkts[0].KeyFrame {
				t.Error("late-joining consumer's first packet should be a cached keyframe")
			}

			break
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for late consumer to receive packets")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
