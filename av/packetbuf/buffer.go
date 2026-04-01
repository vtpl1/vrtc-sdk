// Package packetbuf provides a time-limited packet ring buffer with DemuxCloser
// replay capability. It is designed for near-live playback: a recording muxer
// pushes packets into the buffer, and playback consumers obtain a Demuxer that
// replays buffered packets then follows new ones in real time.
package packetbuf

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// Buffer is a thread-safe sliding-window ring buffer of av.Packets.
//
// Write side: call WriteHeader once, then WritePacket for each packet.
// Read side:  call Demuxer(since) to get a DemuxCloser that replays
// buffered packets from the given wall-clock time, then follows live.
type Buffer struct {
	mu      sync.RWMutex
	streams []av.Stream
	pkts    []av.Packet
	maxAge  time.Duration
	closed  atomic.Bool
	notify  chan struct{} // replaced on each push
}

// New creates a Buffer that retains packets for at most maxAge.
func New(maxAge time.Duration) *Buffer {
	return &Buffer{
		maxAge: maxAge,
		notify: make(chan struct{}),
	}
}

// WriteHeader stores codec headers. Safe to call multiple times (e.g. on
// segment rotation); the latest headers are used for new Demuxers.
func (b *Buffer) WriteHeader(streams []av.Stream) {
	b.mu.Lock()
	b.streams = streams
	b.mu.Unlock()
}

// WritePacket appends a packet and evicts expired entries.
func (b *Buffer) WritePacket(pkt av.Packet) {
	if pkt.WallClockTime.IsZero() {
		pkt.WallClockTime = time.Now()
	}

	b.mu.Lock()
	b.pkts = append(b.pkts, pkt)
	b.evictLocked()
	ch := b.notify
	b.notify = make(chan struct{})
	b.mu.Unlock()

	close(ch) // wake all waiting Demuxers
}

// Close signals that no more packets will be written. All waiting Demuxers
// will receive io.EOF.
func (b *Buffer) Close() {
	if b.closed.CompareAndSwap(false, true) {
		b.mu.Lock()
		ch := b.notify
		b.notify = make(chan struct{})
		b.mu.Unlock()
		close(ch)
	}
}

// Demuxer returns a DemuxCloser that replays buffered packets with
// WallClockTime >= since, then follows new packets in real time until
// the Buffer is closed or the Demuxer's context is cancelled.
func (b *Buffer) Demuxer(since time.Time) av.DemuxCloser {
	return &bufDemuxer{buf: b, since: since}
}

func (b *Buffer) evictLocked() {
	cutoff := time.Now().Add(-b.maxAge)

	i := 0
	for i < len(b.pkts) && b.pkts[i].WallClockTime.Before(cutoff) {
		i++
	}

	if i > 0 {
		b.pkts = b.pkts[i:]
	}
}

// --- DemuxCloser implementation ---

type bufDemuxer struct {
	buf     *Buffer
	since   time.Time
	cursor  int // index into snapshot; -1 = not started
	started bool
	done    atomic.Bool
}

func (d *bufDemuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	d.buf.mu.RLock()
	defer d.buf.mu.RUnlock()

	if len(d.buf.streams) == 0 {
		return nil, io.EOF
	}

	return d.buf.streams, nil
}

func (d *bufDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	if d.done.Load() {
		return av.Packet{}, io.EOF
	}

	for {
		d.buf.mu.RLock()
		pkts := d.buf.pkts
		notify := d.buf.notify
		closed := d.buf.closed.Load()
		d.buf.mu.RUnlock()

		if !d.started {
			// Find first packet >= since.
			d.cursor = 0
			for d.cursor < len(pkts) && pkts[d.cursor].WallClockTime.Before(d.since) {
				d.cursor++
			}

			d.started = true
		}

		if d.cursor < len(pkts) {
			pkt := pkts[d.cursor]
			d.cursor++

			return pkt, nil
		}

		if closed {
			return av.Packet{}, io.EOF
		}

		// Wait for new packets.
		select {
		case <-ctx.Done():
			return av.Packet{}, ctx.Err()
		case <-notify:
			// New packets available — loop back to read.
		}
	}
}

func (d *bufDemuxer) Close() error {
	d.done.Store(true)

	return nil
}
