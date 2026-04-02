// Package chain provides a ChainingDemuxer that reads from multiple segment
// demuxers sequentially, adjusting DTS at each boundary so timestamps are
// monotonically non-decreasing across all segments.
package chain

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

// Compile-time interface check.
var _ av.DemuxCloser = (*ChainingDemuxer)(nil)

// SegmentSource provides demuxers one at a time. A ChainingDemuxer calls
// Next to obtain each successive segment after the initial one is exhausted.
//
// Next returns the next av.DemuxCloser to read from. It returns io.EOF when
// all segments are exhausted. Implementations may block (e.g. polling for
// new segments in follow mode); cancellation is signalled through ctx.
type SegmentSource interface {
	Next(ctx context.Context) (av.DemuxCloser, error)
}

// ChainingDemuxer chains multiple segment demuxers into a single monotonic
// av.DemuxCloser stream. DTS values are adjusted at each segment boundary
// so that timestamps are monotonically non-decreasing across all segments.
//
// The first demuxer is passed directly to the constructor so the caller can
// fail fast on a bad first segment. Subsequent demuxers are obtained lazily
// from the SegmentSource.
//
// ChainingDemuxer is not safe for concurrent use; it is designed for a
// single consumer goroutine's read loop.
type ChainingDemuxer struct {
	source  SegmentSource
	cur     av.DemuxCloser
	dtsOff  time.Duration // cumulative DTS offset for current segment
	lastEnd time.Duration // DTS + Duration of the last emitted packet (after offset)
}

// NewChainingDemuxer returns a ChainingDemuxer that reads from first and
// then obtains subsequent demuxers from source. The caller should call
// GetCodecs before ReadPacket, following the standard av.Demuxer contract.
func NewChainingDemuxer(first av.DemuxCloser, source SegmentSource) *ChainingDemuxer {
	return &ChainingDemuxer{
		source: source,
		cur:    first,
	}
}

// GetCodecs delegates to the current (initially first) demuxer's GetCodecs.
func (c *ChainingDemuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	return c.cur.GetCodecs(ctx)
}

// ReadPacket returns the next packet across all chained segments. When the
// current segment returns io.EOF, the next segment is obtained from the
// SegmentSource transparently. DTS values are offset so they are
// monotonically non-decreasing across segment boundaries.
//
// Returns io.EOF when the SegmentSource is exhausted.
func (c *ChainingDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	for {
		pkt, err := c.cur.ReadPacket(ctx)
		if err == nil {
			pkt.DTS += c.dtsOff

			if end := pkt.DTS + pkt.Duration; end > c.lastEnd {
				c.lastEnd = end
			}

			return pkt, nil
		}

		if !errors.Is(err, io.EOF) {
			return av.Packet{}, err
		}

		// Current segment exhausted — advance to the next.
		_ = c.cur.Close()
		c.cur = nil

		next, err := c.source.Next(ctx)
		if err != nil {
			return av.Packet{}, err // io.EOF propagates when source is done
		}

		c.cur = next

		// Read and discard the init segment (GetCodecs) so ReadPacket
		// starts at the first media fragment.
		if _, err = c.cur.GetCodecs(ctx); err != nil {
			return av.Packet{}, err
		}

		c.dtsOff = c.lastEnd
	}
}

// Close closes the currently active demuxer. Safe to call multiple times.
func (c *ChainingDemuxer) Close() error {
	if c.cur != nil {
		err := c.cur.Close()
		c.cur = nil

		return err
	}

	return nil
}

// SeekableSegmentSource extends SegmentSource with the ability to open the
// segment that contains a given wall-clock timestamp. Implementations that
// maintain a segment index (e.g. a MemStore with start/end times) should
// implement this interface to enable seek in ChainingDemuxer.
type SeekableSegmentSource interface {
	SegmentSource

	// OpenAt returns a DemuxCloser for the segment containing the given
	// wall-clock timestamp, and resets the iteration cursor so that
	// subsequent Next calls continue from the segment after it.
	// Returns io.EOF if no segment covers the timestamp.
	OpenAt(ctx context.Context, ts time.Time) (av.DemuxCloser, error)
}

// Seek repositions the ChainingDemuxer to the segment containing the given
// wall-clock timestamp and seeks within it to the keyframe at or before seekPTS.
// seekPTS is the PTS offset within the segment (relative to segment start).
//
// The source must implement SeekableSegmentSource; if it does not, Seek returns
// an error. After Seek, the next ReadPacket returns packets from the target position.
func (c *ChainingDemuxer) Seek(ctx context.Context, wallTime time.Time, seekPTS time.Duration) error {
	ss, ok := c.source.(SeekableSegmentSource)
	if !ok {
		return errors.New("chain: source does not support seeking")
	}

	// Close the current demuxer.
	if c.cur != nil {
		_ = c.cur.Close()
		c.cur = nil
	}

	dmx, err := ss.OpenAt(ctx, wallTime)
	if err != nil {
		return err
	}

	// Read codec headers from the new segment.
	if _, err := dmx.GetCodecs(ctx); err != nil {
		_ = dmx.Close()

		return err
	}

	// Seek within the segment if the demuxer supports it.
	if seekPTS > 0 {
		if fd, ok := dmx.(*fmp4.Demuxer); ok {
			if err := fd.SeekToKeyframe(seekPTS); err != nil {
				_ = dmx.Close()

				return err
			}
		}
	}

	c.cur = dmx
	c.dtsOff = 0
	c.lastEnd = 0

	return nil
}

// sliceSource is a SegmentSource backed by a fixed list of identifiers and
// an opener function.
type sliceSource struct {
	ids  []string
	idx  int
	open func(ctx context.Context, id string) (av.DemuxCloser, error)
}

// SliceSource returns a SegmentSource that yields demuxers by calling open
// for each element in ids, one at a time, in order. It returns io.EOF after
// the last element. The open function is called lazily — only when Next is
// invoked for that element.
func SliceSource(
	ids []string,
	open func(ctx context.Context, id string) (av.DemuxCloser, error),
) SegmentSource {
	return &sliceSource{ids: ids, open: open}
}

func (s *sliceSource) Next(ctx context.Context) (av.DemuxCloser, error) {
	if s.idx >= len(s.ids) {
		return nil, io.EOF
	}

	id := s.ids[s.idx]
	s.idx++

	return s.open(ctx, id)
}
