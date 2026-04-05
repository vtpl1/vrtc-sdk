package pva

import (
	"context"
	"fmt"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/packetbuf"
)

// PacketBufferProvider returns the near-live replay buffer for a source.
// Implemented by *relayhub.RelayHub.
type PacketBufferProvider interface {
	PacketBuffer(sourceID string) *packetbuf.Buffer
}

// ProxyMuxDemuxer bridges the recording infrastructure (disk segments + live
// packet buffer) to the analytics hub. It reads near-real-time older frames
// via a ChainingDemuxer from RecordedDemuxerFactory or, when no recordings
// exist, directly from the live packet buffer.
type ProxyMuxDemuxer struct {
	inner av.DemuxCloser
}

// GetCodecs delegates to the inner demuxer.
func (p *ProxyMuxDemuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	return p.inner.GetCodecs(ctx)
}

// ReadPacket delegates to the inner demuxer.
func (p *ProxyMuxDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	return p.inner.ReadPacket(ctx)
}

// Close closes the inner demuxer.
func (p *ProxyMuxDemuxer) Close() error {
	return p.inner.Close()
}

// RecordedDemuxerFactoryFunc matches the signature of
// edgeview.Service.RecordedDemuxerFactory.
type RecordedDemuxerFactoryFunc func(channelID string, from, to time.Time) av.DemuxerFactory

// NewAnalyticsDemuxerFactory returns an av.DemuxerFactory for the analytics
// relay hub. Each call creates a ProxyMuxDemuxer backed by the recording
// infrastructure (disk segments → live buffer transition), wrapped with a
// BlockingMerger that waits for analytics.
//
// recordedFactory is typically edgeview.Service.RecordedDemuxerFactory.
// bufProv is the live relay hub (*relayhub.RelayHub), used as a fallback
// when no recordings exist.
func NewAnalyticsDemuxerFactory(
	recordedFactory RecordedDemuxerFactoryFunc,
	bufProv PacketBufferProvider,
	store *AnalyticsStore,
	hub *AnalyticsHub,
	delay time.Duration,
	maxWait time.Duration,
) av.DemuxerFactory {
	return func(ctx context.Context, sourceID string) (av.DemuxCloser, error) {
		from := time.Now().Add(-delay)

		// Try the recording path first (disk segments → live buffer transition).
		factory := recordedFactory(sourceID, from, time.Time{}) // follow mode

		inner, err := factory(ctx, sourceID)
		if err != nil {
			// Fallback: read directly from the live packet buffer.
			if bufProv == nil {
				return nil, fmt.Errorf("pva: analytics demuxer %q: %w", sourceID, err)
			}

			buf := bufProv.PacketBuffer(sourceID)
			if buf == nil {
				return nil, fmt.Errorf("pva: no stream for %q: %w", sourceID, err)
			}

			inner = buf.Demuxer(from)
		}

		source := store.SourceFor(sourceID)
		merger := NewBlockingMerger(inner, source, hub, sourceID, maxWait)

		return &ProxyMuxDemuxer{inner: merger}, nil
	}
}
