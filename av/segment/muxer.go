package segment

import (
	"bytes"
	"context"
	"errors"
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
	hasVideoTracks  bool
	frag            pendingFragment

	// analytics flags — set during WritePacket
	hasMotion   bool
	hasObjects  bool
	hasKeyframe bool // true after the first keyframe is written
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
		target = &teeWriter{disk: w}
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
	m.hasVideoTracks = false

	for _, s := range streams {
		if s.Codec.Type().IsVideo() {
			m.hasVideoTracks = true

			break
		}
	}

	return m.inner.WriteHeader(ctx, streams)
}

func (m *SegmentMuxer) WritePacket(ctx context.Context, pkt av.Packet) error {
	// Size-based rotation: signal at the first keyframe after threshold,
	// but only after at least one keyframe has been written to this segment.
	if m.maxSegmentBytes > 0 && pkt.KeyFrame && m.hasKeyframe &&
		m.writer.BytesWritten() >= m.maxSegmentBytes {
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

	var flushed pendingFragment

	flushesBeforeAppend := m.hasVideoTracks && pkt.KeyFrame && m.frag.valid
	if flushesBeforeAppend {
		flushed = m.frag
	}

	tw, _ := m.tee.(*teeWriter)
	if tw != nil {
		tw.ResetCapture()
	}

	if err := m.inner.WritePacket(ctx, pkt); err != nil {
		return err
	}

	if !m.hasVideoTracks {
		m.frag.observe(pkt)

		if tw != nil {
			m.pushFragment(tw.CapturedBytes(), m.frag)
		}

		m.frag = pendingFragment{}

		return nil
	}

	if flushesBeforeAppend {
		if tw != nil {
			m.pushFragment(tw.CapturedBytes(), flushed)
		}

		m.frag = pendingFragment{}
	}

	m.frag.observe(pkt)

	return nil
}

func (m *SegmentMuxer) WriteTrailer(ctx context.Context, upstreamErr error) error {
	tw, _ := m.tee.(*teeWriter)
	if tw != nil {
		tw.ResetCapture()
	}

	if err := m.inner.WriteTrailer(ctx, upstreamErr); err != nil {
		return err
	}

	if tw != nil {
		m.pushFragment(tw.CapturedBytes(), m.frag)
	}

	m.frag = pendingFragment{}

	return nil
}

// Close flushes remaining data, writes a sidx box for seek support, validates
// the segment, and calls onClose. Safe to call multiple times; subsequent calls
// are no-ops.
func (m *SegmentMuxer) Close() error {
	if m.closed {
		return nil
	}

	m.closed = true
	endTime := time.Now().UTC()

	if err := m.WriteTrailer(context.Background(), nil); err != nil && !errors.Is(err, fmp4.ErrTrailerAlreadyWritten) {
		return err
	}

	// Write sidx box at the end of the segment for frame-accurate seek.
	m.writeSidx()

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

// writeSidx writes a sidx box to the segment file using the fragment index
// accumulated by the inner fMP4 muxer. The sidx maps PTS ranges to byte
// offsets, enabling O(log N) seek within the segment.
func (m *SegmentMuxer) writeSidx() {
	fragIndex := m.inner.FragIndex()
	if len(fragIndex) == 0 {
		return
	}

	// Determine the video track's timescale for the sidx box.
	timescale := uint32(90000) // default

	tw, _ := m.tee.(*teeWriter)
	if tw != nil {
		tw.ResetCapture()
	}

	sidx := fmp4.BuildSidx(fragIndex, timescale)
	if sidx == nil {
		return
	}

	if tw != nil {
		_, _ = tw.disk.Write(sidx)
	} else {
		_, _ = m.writer.Write(sidx)
	}
}

// BytesWritten returns the total bytes written to disk so far.
func (m *SegmentMuxer) BytesWritten() int64 {
	return m.writer.BytesWritten()
}

// teeWriter copies all writes to disk and captures bytes written during a
// single muxer call so SegmentMuxer can push complete fragments to the ring.
type teeWriter struct {
	disk *AdaptiveWriter
	buf  bytes.Buffer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.disk.Write(p)
	if err != nil {
		return n, err
	}

	if n > 0 {
		_, _ = t.buf.Write(p[:n])
	}

	return n, nil
}

func (t *teeWriter) ResetCapture() {
	t.buf.Reset()
}

func (t *teeWriter) CapturedBytes() []byte {
	if t.buf.Len() == 0 {
		return nil
	}

	return append([]byte(nil), t.buf.Bytes()...)
}

// Close delegates to the underlying AdaptiveWriter so that fmp4.Muxer.Close()
// properly flushes, fsyncs, and truncates the segment file.
func (t *teeWriter) Close() error {
	return t.disk.Close()
}

type pendingFragment struct {
	valid     bool
	dts       time.Duration
	duration  time.Duration
	keyFrame  bool
	timestamp time.Time
}

func (p *pendingFragment) observe(pkt av.Packet) {
	if !p.valid {
		p.valid = true
		p.dts = pkt.DTS
		p.keyFrame = pkt.KeyFrame

		p.timestamp = pkt.WallClockTime
		if p.timestamp.IsZero() {
			p.timestamp = time.Now()
		}
	}

	p.duration += pkt.Duration
}

func (m *SegmentMuxer) pushFragment(data []byte, frag pendingFragment) {
	if m.ring == nil || len(data) == 0 || !frag.valid {
		return
	}

	m.ring.Push(Fragment{
		DTS:       frag.dts,
		Duration:  frag.duration,
		KeyFrame:  frag.keyFrame,
		Data:      data,
		Timestamp: frag.timestamp,
	})
}
