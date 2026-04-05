package pva

import (
	"context"

	"github.com/vtpl1/vrtc-sdk/av"
)

// MetadataMerger is a decorator around av.DemuxCloser that attaches *FrameAnalytics
// to each av.Packet via av.Packet.Analytics. The demuxer and muxer layers are
// completely unaware of analytics; the merger is the single injection point.
//
// Usage:
//
//	inner, err := avgrabber.NewDemuxer(cfg)
//	dmx := pva.NewMetadataMerger(inner, myAnalyticsSource)
//	// dmx is now an av.DemuxCloser; pass it to streammanager3.
type MetadataMerger struct {
	inner  av.DemuxCloser
	source Source
}

// NewMetadataMerger wraps inner with a metadata injection layer.
// source is called per packet; use NilSource{} when analytics are not connected.
func NewMetadataMerger(inner av.DemuxCloser, source Source) *MetadataMerger {
	return &MetadataMerger{inner: inner, source: source}
}

// GetCodecs passes through to the inner demuxer.
func (m *MetadataMerger) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	return m.inner.GetCodecs(ctx)
}

// ReadPacket reads from the inner demuxer and injects *FrameAnalytics into
// pkt.Analytics when the source has analytics for the packet's FrameID.
func (m *MetadataMerger) ReadPacket(ctx context.Context) (av.Packet, error) {
	pkt, err := m.inner.ReadPacket(ctx)
	if err != nil {
		return pkt, err
	}

	if fa := m.source.Fetch(pkt.FrameID, pkt.WallClockTime); fa != nil {
		pkt.Analytics = fa
	}

	return pkt, nil
}

// Close closes the inner demuxer.
func (m *MetadataMerger) Close() error {
	return m.inner.Close()
}
