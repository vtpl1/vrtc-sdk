package packetbuf_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/packetbuf"
)

func makeTestStreams() []av.Stream {
	return []av.Stream{{Idx: 0}}
}

func makeTestPacket(wallTime time.Time, data byte) av.Packet {
	return av.Packet{
		KeyFrame:      true,
		Idx:           0,
		CodecType:     av.H264,
		Data:          []byte{data},
		WallClockTime: wallTime,
		DTS:           time.Duration(data) * time.Millisecond,
	}
}

func TestBuffer_WriteAndDemuxer(t *testing.T) {
	t.Parallel()

	buf := packetbuf.New(10 * time.Second)
	defer buf.Close()

	streams := makeTestStreams()
	buf.WriteHeader(streams)

	now := time.Now()
	buf.WritePacket(makeTestPacket(now.Add(-2*time.Second), 1))
	buf.WritePacket(makeTestPacket(now.Add(-1*time.Second), 2))
	buf.WritePacket(makeTestPacket(now, 3))

	dmx := buf.Demuxer(now.Add(-1500 * time.Millisecond))
	defer func() { _ = dmx.Close() }()

	ctx := context.Background()
	got, err := dmx.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(got))
	}

	// Should get packets 2 and 3 (wall clock >= since).
	pkt, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket(1): %v", err)
	}
	if pkt.Data[0] != 2 {
		t.Errorf("expected data=2, got %d", pkt.Data[0])
	}

	pkt, err = dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket(2): %v", err)
	}
	if pkt.Data[0] != 3 {
		t.Errorf("expected data=3, got %d", pkt.Data[0])
	}
}

func TestBuffer_DemuxerFollowsNewPackets(t *testing.T) {
	t.Parallel()

	buf := packetbuf.New(10 * time.Second)
	defer buf.Close()

	buf.WriteHeader(makeTestStreams())

	now := time.Now()
	dmx := buf.Demuxer(now)
	defer func() { _ = dmx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Write a packet after a short delay.
	var wg sync.WaitGroup
	wg.Go(func() {
		time.Sleep(50 * time.Millisecond)
		buf.WritePacket(makeTestPacket(now.Add(100*time.Millisecond), 42))
	})

	pkt, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if pkt.Data[0] != 42 {
		t.Errorf("expected data=42, got %d", pkt.Data[0])
	}

	wg.Wait()
}

func TestBuffer_CloseWakesDemuxer(t *testing.T) {
	t.Parallel()

	buf := packetbuf.New(10 * time.Second)
	buf.WriteHeader(makeTestStreams())

	dmx := buf.Demuxer(time.Now())
	defer func() { _ = dmx.Close() }()

	var wg sync.WaitGroup
	wg.Go(func() {
		time.Sleep(50 * time.Millisecond)
		buf.Close()
	})

	ctx := context.Background()
	_, err := dmx.ReadPacket(ctx)
	if err != io.EOF {
		t.Errorf("expected io.EOF after Close, got %v", err)
	}

	wg.Wait()
}

func TestBuffer_Eviction(t *testing.T) {
	t.Parallel()

	buf := packetbuf.New(500 * time.Millisecond)
	defer buf.Close()

	buf.WriteHeader(makeTestStreams())

	old := time.Now().Add(-1 * time.Second)
	buf.WritePacket(makeTestPacket(old, 1))

	now := time.Now()
	buf.WritePacket(makeTestPacket(now, 2))

	// The old packet should be evicted. Demuxer from old should only see packet 2.
	dmx := buf.Demuxer(old)
	defer func() { _ = dmx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	pkt, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if pkt.Data[0] != 2 {
		t.Errorf("expected data=2 (old evicted), got %d", pkt.Data[0])
	}
}

func TestBuffer_WallClockAutoSet(t *testing.T) {
	t.Parallel()

	buf := packetbuf.New(10 * time.Second)
	defer buf.Close()

	buf.WriteHeader(makeTestStreams())

	// Packet with zero WallClockTime should get it auto-set.
	pkt := av.Packet{KeyFrame: true, Idx: 0, Data: []byte{1}}
	buf.WritePacket(pkt)

	dmx := buf.Demuxer(time.Now().Add(-1 * time.Second))
	defer func() { _ = dmx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	got, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if got.WallClockTime.IsZero() {
		t.Error("expected WallClockTime to be auto-set, got zero")
	}
}

// TestBuffer_EvictionDoesNotSkipPackets verifies that a slow demuxer reader
// does not silently skip packets when evictLocked rebuilds the underlying
// slice. The reader's cursor must stay in sync with the buffer's baseOffset.
func TestBuffer_EvictionDoesNotSkipPackets(t *testing.T) {
	t.Parallel()

	// Short maxAge so eviction kicks in quickly.
	buf := packetbuf.New(200 * time.Millisecond)
	defer buf.Close()

	buf.WriteHeader(makeTestStreams())

	// Start a demuxer before any packets exist so it follows from the start.
	dmx := buf.Demuxer(time.Time{})
	defer func() { _ = dmx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const totalPackets = 100

	// Writer goroutine: writes 100 packets spaced 10ms apart.
	// With maxAge=200ms, roughly 20 packets fit before eviction starts —
	// so the reader MUST survive many eviction cycles.
	var wg sync.WaitGroup

	wg.Go(func() {
		for i := range totalPackets {
			buf.WritePacket(av.Packet{
				KeyFrame:      true,
				Idx:           0,
				CodecType:     av.H264,
				Data:          []byte{byte(i)},
				WallClockTime: time.Now(),
				DTS:           time.Duration(i) * 10 * time.Millisecond,
			})

			time.Sleep(10 * time.Millisecond)
		}

		buf.Close()
	})

	// Reader: read all packets and verify none are skipped.
	var received []byte

	for {
		pkt, err := dmx.ReadPacket(ctx)
		if err != nil {
			break
		}

		received = append(received, pkt.Data[0])
	}

	wg.Wait()

	if len(received) != totalPackets {
		t.Fatalf("expected %d packets, got %d", totalPackets, len(received))
	}

	for i, b := range received {
		if b != byte(i) {
			t.Fatalf("packet[%d]: expected data=%d, got %d (skipped by eviction)", i, i, b)
		}
	}
}

func TestBuffer_GetCodecsEmpty(t *testing.T) {
	t.Parallel()

	buf := packetbuf.New(10 * time.Second)
	defer buf.Close()

	// No WriteHeader called.
	dmx := buf.Demuxer(time.Now())
	defer func() { _ = dmx.Close() }()

	_, err := dmx.GetCodecs(context.Background())
	if err != io.EOF {
		t.Errorf("expected io.EOF for empty streams, got %v", err)
	}
}

func TestBuffer_ContextCancellation(t *testing.T) {
	t.Parallel()

	buf := packetbuf.New(10 * time.Second)
	defer buf.Close()

	buf.WriteHeader(makeTestStreams())

	dmx := buf.Demuxer(time.Now())
	defer func() { _ = dmx.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := dmx.ReadPacket(ctx)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
