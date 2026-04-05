package pva

import (
	"context"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// WallClockStampingDemuxer decorates an av.DemuxCloser and sets WallClockTime
// on every packet whose WallClockTime is zero. The wall-clock is computed as:
//
//	segmentStart + (pkt.DTS - baseDTS)
//
// where baseDTS is the DTS of the first packet read (to handle non-zero
// starting DTS after an intra-segment keyframe seek).
//
// Packets that already carry a WallClockTime (e.g. from the live packet
// buffer) pass through unchanged.
//
// This demuxer wraps individual segment demuxers BEFORE the ChainingDemuxer
// adjusts DTS offsets, so pkt.DTS is relative to the segment start.
type WallClockStampingDemuxer struct {
	inner        av.DemuxCloser
	segmentStart time.Time
	baseDTS      time.Duration
	baseDTSSet   bool
}

// NewWallClockStampingDemuxer wraps inner so that packets gain a WallClockTime
// derived from segmentStart and their DTS offset within the segment.
func NewWallClockStampingDemuxer(
	inner av.DemuxCloser,
	segmentStart time.Time,
) *WallClockStampingDemuxer {
	return &WallClockStampingDemuxer{
		inner:        inner,
		segmentStart: segmentStart,
	}
}

// GetCodecs delegates to the inner demuxer.
func (d *WallClockStampingDemuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	return d.inner.GetCodecs(ctx)
}

// ReadPacket reads from the inner demuxer and stamps WallClockTime when absent.
func (d *WallClockStampingDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	pkt, err := d.inner.ReadPacket(ctx)
	if err != nil {
		return pkt, err
	}

	if pkt.WallClockTime.IsZero() && !d.segmentStart.IsZero() {
		if !d.baseDTSSet {
			d.baseDTS = pkt.DTS
			d.baseDTSSet = true
		}

		pkt.WallClockTime = d.segmentStart.Add(pkt.DTS - d.baseDTS)
	}

	return pkt, nil
}

// Close closes the inner demuxer.
func (d *WallClockStampingDemuxer) Close() error {
	return d.inner.Close()
}
