package grpc

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/vtpl1/vrtc-sdk/av"
)

// Compile-time interface checks.
var (
	_ av.DemuxCloser = (*ServerDemuxer)(nil)
	_ av.Pauser      = (*ServerDemuxer)(nil)
)

// ServerDemuxer implements av.DemuxCloser on the server side for a pushed stream.
// The gRPC PushStream handler feeds packets into this demuxer via its internal channel.
// It also implements av.Pauser so RelayHub can pause delivery.
type ServerDemuxer struct {
	sourceID string
	packets  chan av.Packet
	codecsMu sync.RWMutex
	codecs   []av.Stream
	codecsCh chan struct{} // closed once header has been received
	errMu    sync.Mutex
	err      error // terminal error from gRPC handler
	closed   atomic.Bool

	// Pause support: ReadPacket blocks while paused.
	pauseMu sync.Mutex
	pauseCh chan struct{} // nil when not paused; closed to unblock when resumed
	paused  atomic.Bool
}

func newServerDemuxer(sourceID string, bufSize int) *ServerDemuxer {
	return &ServerDemuxer{
		sourceID: sourceID,
		packets:  make(chan av.Packet, bufSize),
		codecsCh: make(chan struct{}),
	}
}

// updateCodecs replaces the codec list for mid-stream codec changes.
func (d *ServerDemuxer) updateCodecs(streams []av.Stream) {
	d.codecsMu.Lock()
	d.codecs = streams
	d.codecsMu.Unlock()
}

// setCodecsAndSignal sets the initial codecs and unblocks GetCodecs.
// Must be called exactly once.
func (d *ServerDemuxer) setCodecsAndSignal(streams []av.Stream) {
	d.codecsMu.Lock()
	d.codecs = streams
	d.codecsMu.Unlock()
	close(d.codecsCh)
}

// setError records a terminal error.
func (d *ServerDemuxer) setError(err error) {
	d.errMu.Lock()
	if d.err == nil {
		d.err = err
	}
	d.errMu.Unlock()
}

// GetCodecs blocks until the remote client sends the StreamHeader.
func (d *ServerDemuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.codecsCh:
		d.codecsMu.RLock()
		c := d.codecs
		d.codecsMu.RUnlock()

		return c, nil
	}
}

// ReadPacket returns the next packet pushed by the remote client.
// Returns io.EOF when the stream ends cleanly. Blocks while paused.
func (d *ServerDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	// If paused, block until resumed or context cancelled.
	if d.paused.Load() {
		d.pauseMu.Lock()
		ch := d.pauseCh
		d.pauseMu.Unlock()

		if ch != nil {
			select {
			case <-ctx.Done():
				return av.Packet{}, ctx.Err()
			case <-ch:
				// Resumed — fall through to read.
			}
		}
	}

	select {
	case <-ctx.Done():
		return av.Packet{}, ctx.Err()
	case pkt, ok := <-d.packets:
		if !ok {
			d.errMu.Lock()
			err := d.err
			d.errMu.Unlock()

			if err != nil {
				return av.Packet{}, err
			}

			return av.Packet{}, io.EOF
		}

		return pkt, nil
	}
}

// Pause implements av.Pauser. Causes ReadPacket to block until Resume is called.
func (d *ServerDemuxer) Pause(_ context.Context) error {
	if d.paused.CompareAndSwap(false, true) {
		d.pauseMu.Lock()
		d.pauseCh = make(chan struct{})
		d.pauseMu.Unlock()
	}

	return nil
}

// Resume implements av.Pauser. Unblocks ReadPacket.
func (d *ServerDemuxer) Resume(_ context.Context) error {
	if d.paused.CompareAndSwap(true, false) {
		d.pauseMu.Lock()
		ch := d.pauseCh
		d.pauseCh = nil
		d.pauseMu.Unlock()

		if ch != nil {
			close(ch)
		}
	}

	return nil
}

// IsPaused implements av.Pauser.
func (d *ServerDemuxer) IsPaused() bool {
	return d.paused.Load()
}

// Close marks this demuxer as closed. If GetCodecs has not been called yet, it
// is unblocked with io.ErrClosedPipe. The packets channel is closed by the
// PushStream handler's defer, not by Close; ReadPacket will return io.EOF once
// the channel is drained and closed.
func (d *ServerDemuxer) Close() error {
	if d.closed.CompareAndSwap(false, true) {
		// Unblock paused readers.
		_ = d.Resume(context.Background())

		// Ensure codecsCh is closed so GetCodecs doesn't block forever.
		select {
		case <-d.codecsCh:
		default:
			d.setError(io.ErrClosedPipe)
			close(d.codecsCh)
		}
	}

	return nil
}
