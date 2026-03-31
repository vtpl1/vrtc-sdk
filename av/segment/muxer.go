package segment

import (
	"context"
	"io"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
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
type SegmentMuxer struct {
	inner   *fmp4.Muxer
	writer  *AdaptiveWriter
	ring    *RingBuffer // optional; nil if ring buffer disabled
	tee     io.Writer   // the writer passed to fmp4.Muxer (tee or adaptive)
	path    string
	start   time.Time
	onClose func(SegmentCloseInfo)

	// analytics flags — set during WritePacket
	hasMotion  bool
	hasObjects bool
}

// NewSegmentMuxer creates the output file at path with storage-optimised
// buffering and returns the muxer. If ring is non-nil, fragment bytes
// are tee'd to both disk and the ring buffer.
func NewSegmentMuxer(
	path string,
	startTime time.Time,
	profile StorageProfile,
	preallocBytes int64,
	ring *RingBuffer,
	onClose func(SegmentCloseInfo),
) (*SegmentMuxer, error) {
	w, err := NewAdaptiveWriter(path, profile, preallocBytes)
	if err != nil {
		return nil, err
	}

	var target io.Writer = w

	if ring != nil {
		target = &teeWriter{disk: w, ring: ring}
	}

	return &SegmentMuxer{
		inner:   fmp4.NewMuxer(target),
		writer:  w,
		ring:    ring,
		tee:     target,
		path:    path,
		start:   startTime,
		onClose: onClose,
	}, nil
}

func (m *SegmentMuxer) WriteHeader(ctx context.Context, streams []av.Stream) error {
	return m.inner.WriteHeader(ctx, streams)
}

func (m *SegmentMuxer) WritePacket(ctx context.Context, pkt av.Packet) error {
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
func (m *SegmentMuxer) Close() error {
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
