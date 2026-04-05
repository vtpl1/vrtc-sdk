package pva

import (
	"context"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// BlockingMerger is a decorator around av.DemuxCloser that blocks on each
// video packet until analytics are available in the store, or the max wait
// expires. Unlike MetadataMerger (which does an immediate non-blocking
// lookup), BlockingMerger is designed for the analytics hub where a delay
// is acceptable.
//
// Audio packets and packets without a wall-clock time pass through without
// blocking.
type BlockingMerger struct {
	inner    av.DemuxCloser
	source   Source
	hub      *AnalyticsHub
	sourceID string
	maxWait  time.Duration
}

// NewBlockingMerger wraps inner with a blocking analytics injection layer.
func NewBlockingMerger(
	inner av.DemuxCloser,
	source Source,
	hub *AnalyticsHub,
	sourceID string,
	maxWait time.Duration,
) *BlockingMerger {
	return &BlockingMerger{
		inner:    inner,
		source:   source,
		hub:      hub,
		sourceID: sourceID,
		maxWait:  maxWait,
	}
}

// GetCodecs passes through to the inner demuxer.
func (m *BlockingMerger) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	return m.inner.GetCodecs(ctx)
}

// ReadPacket reads from the inner demuxer and blocks until analytics are
// available for video packets, or maxWait is exceeded.
func (m *BlockingMerger) ReadPacket(ctx context.Context) (av.Packet, error) {
	pkt, err := m.inner.ReadPacket(ctx)
	if err != nil {
		return pkt, err
	}

	// Only block for video packets with a wall-clock time.
	if !pkt.CodecType.IsVideo() || pkt.WallClockTime.IsZero() {
		return pkt, nil
	}

	// Fast path: analytics already in store.
	if fa := m.source.Fetch(pkt.FrameID, pkt.WallClockTime); fa != nil {
		pkt.Analytics = fa

		return pkt, nil
	}

	// Slow path: subscribe and wait for analytics notification.
	ch := m.hub.Subscribe(m.sourceID)
	defer m.hub.Unsubscribe(m.sourceID, ch)

	// Re-check after subscribing to close the race window.
	if fa := m.source.Fetch(pkt.FrameID, pkt.WallClockTime); fa != nil {
		pkt.Analytics = fa

		return pkt, nil
	}

	deadline := time.NewTimer(m.maxWait)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return pkt, nil
		case <-deadline.C:
			return pkt, nil
		case _, ok := <-ch:
			if !ok {
				return pkt, nil
			}

			if fa := m.source.Fetch(pkt.FrameID, pkt.WallClockTime); fa != nil {
				pkt.Analytics = fa

				return pkt, nil
			}
		}
	}
}

// Close closes the inner demuxer.
func (m *BlockingMerger) Close() error {
	return m.inner.Close()
}
