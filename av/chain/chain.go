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

// ErrSeekNotSupported is returned when Seek is called on a source that does
// not implement SeekableSegmentSource.
var ErrSeekNotSupported = errors.New("chain: source does not support seeking")

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

// GapDetector is an optional interface that a SegmentSource may implement to
// report wall-clock gaps between consecutive segments. When implemented,
// ChainingDemuxer sets IsDiscontinuity on the first packet after a gap.
type GapDetector interface {
	// LastGap returns the wall-clock gap detected at the most recent segment
	// transition (i.e. the last call to Next). Returns zero if no significant
	// gap exists.
	LastGap() time.Duration
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
	source        SegmentSource
	cur           av.DemuxCloser
	dtsOff        time.Duration // cumulative DTS offset for current segment
	lastEnd       time.Duration // DTS + Duration of the last emitted packet (after offset)
	discontinuity bool          // set after a gap-detected segment transition

	// prevStreams holds the codec state from the previous segment so that
	// codec changes at segment boundaries can be detected and propagated
	// via Packet.NewCodecs. Nil until the first GetCodecs call.
	prevStreams      []av.Stream
	pendingNewCodecs []av.Stream // non-nil when a codec change was detected at a segment boundary
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
// It also stores the returned streams for codec-change detection at the next
// segment boundary.
func (c *ChainingDemuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	streams, err := c.cur.GetCodecs(ctx)
	if err != nil {
		return nil, err
	}

	c.prevStreams = cloneStreams(streams)

	return streams, nil
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

			// Mark the first packet after a gap-detected segment transition.
			if c.discontinuity {
				pkt.IsDiscontinuity = true
				c.discontinuity = false
			}

			// Propagate codec change detected at the last segment boundary.
			if c.pendingNewCodecs != nil {
				pkt.NewCodecs = c.pendingNewCodecs
				c.pendingNewCodecs = nil
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

		// Check if the source reports a wall-clock gap at this transition.
		if gd, ok := c.source.(GapDetector); ok && gd.LastGap() > 0 {
			c.discontinuity = true
		}

		// Read the init segment (GetCodecs) from the new segment and
		// compare with the previous segment's codecs to detect changes.
		newStreams, gerr := c.cur.GetCodecs(ctx)
		if gerr != nil {
			return av.Packet{}, gerr
		}

		if c.prevStreams != nil && streamsChanged(c.prevStreams, newStreams) {
			c.pendingNewCodecs = cloneStreams(newStreams)
		}

		c.prevStreams = cloneStreams(newStreams)
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
func (c *ChainingDemuxer) Seek(
	ctx context.Context,
	wallTime time.Time,
	seekPTS time.Duration,
) error {
	ss, ok := c.source.(SeekableSegmentSource)
	if !ok {
		return ErrSeekNotSupported
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
	newStreams, gerr := dmx.GetCodecs(ctx)
	if gerr != nil {
		_ = dmx.Close()

		return gerr
	}

	// Detect codec changes between the previous position and the seek target.
	if c.prevStreams != nil && streamsChanged(c.prevStreams, newStreams) {
		c.pendingNewCodecs = cloneStreams(newStreams)
	}

	c.prevStreams = cloneStreams(newStreams)

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

// tagger is implemented by codec data types that produce an RFC 6381 codec tag
// (e.g. "avc1.64001E" for H.264 High Profile). Used for fine-grained codec
// change detection beyond just the CodecType.
type tagger interface{ Tag() string }

// streamsChanged reports whether the codec configuration has meaningfully
// changed between two stream sets. It checks stream count, indices, codec
// types, codec tags (profile/level), and video dimensions.
func streamsChanged(prev, next []av.Stream) bool {
	if len(prev) != len(next) {
		return true
	}

	for i := range prev {
		if prev[i].Idx != next[i].Idx {
			return true
		}

		if prev[i].Codec.Type() != next[i].Codec.Type() {
			return true
		}

		// Check codec-specific parameters via RFC 6381 tag (profile/level/etc).
		pt, ptOK := prev[i].Codec.(tagger)
		nt, ntOK := next[i].Codec.(tagger)

		if ptOK && ntOK && pt.Tag() != nt.Tag() {
			return true
		}

		// Check video dimensions.
		pv, pvOK := prev[i].Codec.(av.VideoCodecData)
		nv, nvOK := next[i].Codec.(av.VideoCodecData)

		if pvOK && nvOK {
			if pv.Width() != nv.Width() || pv.Height() != nv.Height() {
				return true
			}
		}
	}

	return false
}

// cloneStreams returns a shallow copy of the stream slice.
func cloneStreams(ss []av.Stream) []av.Stream {
	c := make([]av.Stream, len(ss))
	copy(c, ss)

	return c
}
