package segment

import (
	"context"
	"io"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
	"github.com/vtpl1/vrtc-sdk/av/relayhub"
)

// SegmentCloseInfo is passed to the onClose callback when a SegmentMuxer is
// closed. It summarises the completed segment for index insertion.
type SegmentCloseInfo struct {
	Path            string
	Start           time.Time
	End             time.Time
	SizeBytes       int64
	HasMotion       bool
	HasObjects      bool
	ValidationError error // non-nil if segment failed structural validation
}

// SegmentMuxer wraps an fmp4.Muxer writing to an AdaptiveWriter.
// It satisfies av.MuxCloser and tracks per-segment analytics flags.
// Use NewSegmentMuxer to create one directly, or let a recording manager
// manage the full lifecycle automatically.
//
// When maxSegmentBytes > 0, the muxer returns relayhub.ErrMuxerRotate from
// WritePacket at the first keyframe after the threshold is crossed. The
// consumer catches this and opens a new muxer via the MuxerFactory.
type SegmentMuxer struct {
	inner           *fmp4.Muxer
	writer          *AdaptiveWriter
	ring            *RingBuffer // optional; nil if ring buffer disabled
	tee             io.Writer   // the writer passed to fmp4.Muxer (tee or adaptive)
	path            string
	start           time.Time
	maxSegmentBytes int64 // 0 = no size limit
	onClose         func(SegmentCloseInfo)
	closed          bool

	// analytics flags — set during WritePacket
	hasMotion    bool
	hasObjects   bool
	hasKeyframe  bool // true after the first keyframe is written
}

// NewSegmentMuxer creates the output file at path with storage-optimised
// buffering and returns the muxer. If ring is non-nil, fragment bytes
// are tee'd to both disk and the ring buffer.
//
// maxSegmentBytes sets the target file size for rotation. When > 0, the muxer
// returns relayhub.ErrMuxerRotate from WritePacket at the first keyframe after
// this threshold is crossed. The consumer catches the error and opens a new
// segment via the MuxerFactory. Set to 0 to disable size-based rotation.
//
// preallocBytes requests disk preallocation (effective on HDD/SAN profiles).
// For size-based rotation, pass maxSegmentBytes + 12% headroom as preallocBytes.
func NewSegmentMuxer(
	path string,
	startTime time.Time,
	profile StorageProfile,
	maxSegmentBytes int64,
	preallocBytes int64,
	ring *RingBuffer,
	onClose func(SegmentCloseInfo),
) (*SegmentMuxer, error) {
	fixedSize := maxSegmentBytes > 0 && preallocBytes > 0
	w, err := NewAdaptiveWriter(path, profile, preallocBytes, fixedSize)
	if err != nil {
		return nil, err
	}

	var target io.Writer = w

	if ring != nil {
		target = &teeWriter{disk: w, ring: ring}
	}

	return &SegmentMuxer{
		inner:           fmp4.NewMuxer(target),
		writer:          w,
		ring:            ring,
		tee:             target,
		path:            path,
		start:           startTime,
		maxSegmentBytes: maxSegmentBytes,
		onClose:         onClose,
	}, nil
}

func (m *SegmentMuxer) WriteHeader(ctx context.Context, streams []av.Stream) error {
	return m.inner.WriteHeader(ctx, streams)
}

func (m *SegmentMuxer) WritePacket(ctx context.Context, pkt av.Packet) error {
	// Size-based rotation: signal at the first keyframe after threshold,
	// but only after at least one keyframe has been written to this segment.
	if m.maxSegmentBytes > 0 && pkt.KeyFrame && m.hasKeyframe && m.writer.BytesWritten() >= m.maxSegmentBytes {
		return relayhub.ErrMuxerRotate
	}
	if pkt.KeyFrame {
		m.hasKeyframe = true
	}

	if pkt.Analytics != nil {
		m.hasMotion = true

		if len(pkt.Analytics.Objects) > 0 {
			m.hasObjects = true
		}
	}

	return m.inner.WritePacket(ctx, pkt)
}

func (m *SegmentMuxer) WriteTrailer(ctx context.Context, upstreamErr error) error {
	return m.inner.WriteTrailer(ctx, upstreamErr)
}

// Close flushes remaining data, validates the segment, and calls onClose.
// Safe to call multiple times; subsequent calls are no-ops.
func (m *SegmentMuxer) Close() error {
	if m.closed {
		return nil
	}

	m.closed = true
	endTime := time.Now().UTC()

	err := m.inner.Close()

	sizeBytes := m.writer.BytesWritten()

	if m.onClose != nil {
		m.onClose(SegmentCloseInfo{
			Path:            m.path,
			Start:           m.start,
			End:             endTime,
			SizeBytes:       sizeBytes,
			HasMotion:       m.hasMotion,
			HasObjects:      m.hasObjects,
			ValidationError: ValidateSegment(m.path),
		})
	}

	return err
}

// BytesWritten returns the total bytes written to disk so far.
func (m *SegmentMuxer) BytesWritten() int64 {
	return m.writer.BytesWritten()
}

// teeWriter copies all writes to both the disk writer and the ring buffer.
// The fMP4 muxer writes each fragment (emsg + moof + mdat) as a series of
// Write calls. The tee captures each write and pushes it to the ring buffer.
type teeWriter struct {
	disk *AdaptiveWriter
	ring *RingBuffer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.disk.Write(p)
	if err != nil {
		return n, err
	}

	// Push raw bytes as a fragment to the ring buffer.
	// Each fMP4 flush writes moof+mdat as a logical unit.
	t.ring.Push(Fragment{
		Data:      append([]byte(nil), p[:n]...),
		Timestamp: time.Now(),
	})

	return n, nil
}

// Close delegates to the underlying AdaptiveWriter so that fmp4.Muxer.Close()
// properly flushes, fsyncs, and truncates the segment file.
func (t *teeWriter) Close() error {
	return t.disk.Close()
}
