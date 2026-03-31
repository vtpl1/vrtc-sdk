package mse

import (
	"context"
	"io"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

// Demuxer implements av.DemuxCloser for MSE binary streams.
//
// MSE muxing writes fMP4 init/media segments on the binary channel and optional
// JSON metadata on the text channel. Demuxer consumes only the binary bytes,
// which are sufficient to recover codecs, packets, codec changes, and analytics
// (from emsg boxes).
type Demuxer struct {
	inner *fmp4.Demuxer
}

// NewDemuxer creates an MSE Demuxer from a stream of binary MSE bytes.
func NewDemuxer(binary io.Reader) *Demuxer {
	return &Demuxer{inner: fmp4.NewDemuxer(binary)}
}

// GetCodecs reads the init segment and returns the initial streams.
func (d *Demuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	return d.inner.GetCodecs(ctx)
}

// ReadPacket returns the next packet from the binary MSE stream.
func (d *Demuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	return d.inner.ReadPacket(ctx)
}

// Close releases the underlying reader if it implements io.Closer.
func (d *Demuxer) Close() error {
	return d.inner.Close()
}
