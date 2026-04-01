package relayhub_test

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/relayhub"
)

// rotatingMuxer returns ErrMuxerRotate on the Nth keyframe AFTER the first.
// The first keyframe is always accepted (it's the re-delivered rotation keyframe
// or the initial keyframe). Rotation triggers on keyframe N+1.
type rotatingMuxer struct {
	rotateAfter int // rotate after this many accepted keyframes
	keyCount    int
	packets     int
	closed      bool
}

func (m *rotatingMuxer) WriteHeader(_ context.Context, _ []av.Stream) error { return nil }

func (m *rotatingMuxer) WritePacket(_ context.Context, pkt av.Packet) error {
	if pkt.KeyFrame {
		m.keyCount++
		if m.keyCount > m.rotateAfter {
			return relayhub.ErrMuxerRotate
		}
	}
	m.packets++
	return nil
}

func (m *rotatingMuxer) WriteTrailer(_ context.Context, _ error) error { return nil }
func (m *rotatingMuxer) Close() error                                  { m.closed = true; return nil }

// simpleDemuxer emits N keyframe packets then EOF.
type simpleDemuxer struct {
	streams []av.Stream
	total   int
	sent    atomic.Int32
}

func (d *simpleDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return d.streams, nil
}

func (d *simpleDemuxer) ReadPacket(_ context.Context) (av.Packet, error) {
	sent := int(d.sent.Load())
	if sent >= d.total {
		return av.Packet{}, io.EOF
	}

	sent = int(d.sent.Add(1))

	return av.Packet{
		KeyFrame:  true,
		Idx:       0,
		CodecType: av.H264,
		DTS:       time.Duration(sent) * 33 * time.Millisecond,
		Duration:  33 * time.Millisecond,
		Data:      []byte{byte(sent)},
	}, nil
}

func (d *simpleDemuxer) Close() error { return nil }

func TestConsumer_MuxerRotation(t *testing.T) {
	t.Parallel()

	streams := []av.Stream{{Idx: 0}}
	dmx := &simpleDemuxer{streams: streams, total: 10}

	var factoryCalls atomic.Int32
	var mu sync.Mutex
	var muxers []*rotatingMuxer

	muxerFactory := func(_ context.Context, _ string) (av.MuxCloser, error) {
		factoryCalls.Add(1)
		m := &rotatingMuxer{rotateAfter: 3} // rotate every 3 keyframes
		mu.Lock()
		muxers = append(muxers, m)
		mu.Unlock()
		return m, nil
	}

	hub := relayhub.New(
		func(_ context.Context, _ string) (av.DemuxCloser, error) {
			return dmx, nil
		},
		nil,
	)

	ctx := t.Context()
	if err := hub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = hub.Stop() }()

	handle, err := hub.Consume(ctx, "test-source", av.ConsumeOptions{
		ConsumerID:   "test-consumer",
		MuxerFactory: muxerFactory,
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if int(dmx.sent.Load()) >= dmx.total {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	_ = handle.Close(ctx)

	// Verify: factory was called more than once (rotations occurred).
	calls := factoryCalls.Load()
	if calls < 2 {
		t.Errorf("expected at least 2 factory calls (1 initial + rotations), got %d", calls)
	}
	t.Logf("factory called %d times (1 initial + %d rotations)", calls, calls-1)

	mu.Lock()
	defer mu.Unlock()

	// All muxers except the last should be closed (rotation closes them).
	if len(muxers) > 1 {
		for i, m := range muxers[:len(muxers)-1] {
			if !m.closed {
				t.Errorf("muxer[%d]: expected closed after rotation", i)
			}
		}
	}

	// Total packets across all muxers should account for all delivered packets.
	totalPkts := 0
	for i, m := range muxers {
		t.Logf("muxer[%d]: packets=%d keyframes=%d closed=%v", i, m.packets, m.keyCount, m.closed)
		totalPkts += m.packets
	}
	if totalPkts < 8 {
		t.Errorf("expected at least 8 total packets, got %d", totalPkts)
	}
}
