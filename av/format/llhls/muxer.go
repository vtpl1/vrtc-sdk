// Package llhls implements a Low-Latency HLS muxer per Apple's LL-HLS spec
// (https://developer.apple.com/streaming/). Media is packaged as CMAF
// (Chunked MP4) fragments called "parts". Multiple parts form a full segment.
//
// Call flow:
//
//	m := llhls.NewMuxer(llhls.DefaultConfig())
//	http.Handle("/hls/", m.Handler("/hls"))
//	m.WriteHeader(ctx, streams)
//	for each packet: m.WritePacket(ctx, pkt)
//	m.WriteTrailer(ctx, err)
//
// Clients reach the playlist at /hls/index.m3u8.
package llhls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

// ── errors ────────────────────────────────────────────────────────────────────

var (
	// ErrHeaderAlreadyWritten is returned on a second WriteHeader call.
	ErrHeaderAlreadyWritten = errors.New("llhls: WriteHeader already called")
	// ErrTrailerAlreadyWritten is returned on a second WriteTrailer call.
	ErrTrailerAlreadyWritten = errors.New("llhls: WriteTrailer already called")
	// ErrHeaderNotWritten is returned if WritePacket is called before WriteHeader.
	ErrHeaderNotWritten = errors.New("llhls: WriteHeader not called")

	errMuxerClosed           = errors.New("muxer closed")
	errBlockingReloadTimeout = errors.New("blocking reload timeout")
)

// ── configuration ─────────────────────────────────────────────────────────────

// Config holds tunable LL-HLS parameters.
type Config struct {
	// PartTarget is the target duration for each partial segment (part).
	// Apple recommends ≤ EXT-X-TARGETDURATION / 10. Default: 200ms.
	PartTarget time.Duration

	// SegTarget is the target duration for a complete segment.
	// Becomes EXT-X-TARGETDURATION. Default: 2s.
	SegTarget time.Duration

	// SegBufferCount is how many complete segments to keep in the ring buffer.
	// Clients that lag further than this will miss segments. Default: 5.
	SegBufferCount int

	// BlockingReloadTimeout is the maximum time the server waits on a blocking
	// playlist reload (_HLS_msn / _HLS_part) before returning 503. Default: 10s.
	BlockingReloadTimeout time.Duration
}

// DefaultConfig returns production-ready LL-HLS defaults.
func DefaultConfig() Config {
	return Config{
		PartTarget:            200 * time.Millisecond,
		SegTarget:             2 * time.Second,
		SegBufferCount:        5,
		BlockingReloadTimeout: 10 * time.Second,
	}
}

// ── data model ────────────────────────────────────────────────────────────────

// Part is one LL-HLS partial segment: a single moof+mdat fMP4 fragment.
type Part struct {
	SegSeqNo    uint64  // parent segment sequence number
	PartIdx     int     // 0-based index within the segment
	Independent bool    // first sample is a video keyframe (INDEPENDENT=YES)
	Duration    float64 // actual duration in seconds
	Data        []byte  // moof+mdat bytes
}

// uri returns the relative URL of this part.
func (p *Part) uri() string {
	return fmt.Sprintf("part%d_%d.mp4", p.SegSeqNo, p.PartIdx)
}

// Segment is a complete HLS segment made up of one or more Parts.
type Segment struct {
	SeqNo    uint64
	Duration float64
	WallTime time.Time
	Parts    []*Part
	Data     []byte // concatenated part data (set on completion)
}

// uri returns the relative URL of this segment.
func (s *Segment) uri() string {
	return fmt.Sprintf("seg%d.mp4", s.SeqNo)
}

// ── Muxer ─────────────────────────────────────────────────────────────────────

// Muxer implements av.Muxer and serves the resulting LL-HLS stream over HTTP.
type Muxer struct {
	cfg Config

	// fMP4 fragment writer (set after WriteHeader).
	fw       *fmp4.FragmentWriter
	initData []byte

	// streams holds the current stream list; updated by WriteCodecChange.
	// Only accessed from the WritePacket goroutine (no lock needed).
	streams []av.Stream

	// current-part accumulation (not locked; only written by WritePacket goroutine).
	partDurAccum time.Duration // duration accumulated in the in-progress part
	segDurAccum  float64       // duration (s) from completed parts in current segment

	// published data – all fields below are protected by mu.
	mu       sync.Mutex
	cond     *sync.Cond
	curSeg   *Segment   // segment being built (parts appended as they complete)
	compSegs []*Segment // completed segments (ring buffer, oldest first)

	// maxSegDur is the maximum actual segment duration (seconds) seen so far.
	// Updated by finaliseSegment; used for EXT-X-TARGETDURATION.
	maxSegDur float64

	// seenFirstKey gates WritePacket: packets are dropped until the first video
	// keyframe so every segment starts at an IDR and spans a full keyframe interval.
	seenFirstKey bool

	// lifecycle
	written     bool
	closed      bool
	terminating bool // set by Close() so waitForPart exits immediately
}

// NewMuxer creates an LL-HLS Muxer with the given configuration.
func NewMuxer(cfg Config) *Muxer {
	m := &Muxer{
		cfg:          cfg,
		fw:           nil,
		initData:     nil,
		streams:      nil,
		partDurAccum: 0,
		segDurAccum:  0,
		mu:           sync.Mutex{},
		cond:         nil,
		curSeg:       nil,
		compSegs:     nil,
		written:      false,
		closed:       false,
		terminating:  false,
	}
	m.cond = sync.NewCond(&m.mu)

	return m
}

// WriteHeader initialises the fMP4 fragment writer and publishes the init
// segment. It must be called exactly once before WritePacket.
func (m *Muxer) WriteHeader(_ context.Context, streams []av.Stream) error {
	if m.written {
		return ErrHeaderAlreadyWritten
	}

	fw, init, err := fmp4.NewFragmentWriter(streams)
	if err != nil {
		return err
	}

	m.fw = fw
	m.initData = init

	m.streams = append([]av.Stream(nil), streams...) // own copy

	m.mu.Lock()
	m.curSeg = &Segment{
		SeqNo:    0,
		Duration: 0,
		WallTime: time.Now(),
		Parts:    nil,
		Data:     nil,
	}
	m.mu.Unlock()

	m.written = true

	return nil
}

// WritePacket buffers pkt. Parts are flushed when a video keyframe arrives or
// when the accumulated part duration reaches PartTarget. A new segment is
// started at a keyframe boundary when SegTarget has been reached.
func (m *Muxer) WritePacket(_ context.Context, pkt av.Packet) error {
	if !m.written {
		return ErrHeaderNotWritten
	}

	if m.closed {
		return ErrTrailerAlreadyWritten
	}

	isVideoKey := pkt.KeyFrame && pkt.CodecType.IsVideo() && m.fw.HasVideo()

	// Drop everything until the first video keyframe so that every segment
	// starts at an IDR boundary and spans a full camera keyframe interval.
	// This also ensures TARGETDURATION reflects a complete GOP, not a partial one.
	if !m.seenFirstKey {
		if !isVideoKey {
			return nil
		}

		m.seenFirstKey = true
	}

	// Decide whether to flush the current part before adding this packet.
	shouldFlush := m.fw.HasSamples() && (isVideoKey || m.partDurAccum >= m.cfg.PartTarget)

	if shouldFlush {
		m.emitPart(isVideoKey)
	}

	m.fw.WritePacket(pkt)
	m.partDurAccum += pkt.Duration

	return nil
}

// WriteTrailer flushes any buffered part and finalises the current segment.
func (m *Muxer) WriteTrailer(_ context.Context, _ error) error {
	if m.closed {
		return ErrTrailerAlreadyWritten
	}

	m.closed = true

	if m.fw != nil && m.fw.HasSamples() {
		m.emitPart(false)
	}

	// Complete the current segment even if it didn't hit SegTarget.
	m.mu.Lock()
	if m.curSeg != nil && len(m.curSeg.Parts) > 0 {
		m.finaliseSegment()
	}

	m.cond.Broadcast()
	m.mu.Unlock()

	return nil
}

// Close implements av.MuxCloser. It performs a best-effort WriteTrailer (if not
// already done) and wakes any goroutines blocked on a playlist reload.
func (m *Muxer) Close() error {
	if m.written && !m.closed {
		_ = m.WriteTrailer(context.Background(), nil)
	}

	m.mu.Lock()
	m.terminating = true
	m.cond.Broadcast() // release any lingering blocked-reload goroutines
	m.mu.Unlock()

	return nil
}

// WriteCodecChange implements av.CodecChanger. It flushes the current part,
// replaces the codec for each listed stream, rebuilds the fMP4 fragment writer,
// and publishes a new init segment. Only streams listed in changed are updated.
func (m *Muxer) WriteCodecChange(_ context.Context, changed []av.Stream) error {
	if !m.written || m.closed {
		return nil
	}

	// Flush any in-progress part before the codec switch.
	if m.fw != nil && m.fw.HasSamples() {
		m.emitPart(false)
	}

	// Apply changes to our local copy of the stream list.
	for _, s := range changed {
		for i, orig := range m.streams {
			if orig.Idx == s.Idx {
				m.streams[i] = s

				break
			}
		}
	}

	// Rebuild the FragmentWriter with the updated codecs.
	fw, init, err := fmp4.NewFragmentWriter(m.streams)
	if err != nil {
		return err
	}

	m.fw = fw // safe: same goroutine as WritePacket

	// Publish the new init segment and notify HTTP clients.
	m.mu.Lock()
	m.initData = init
	m.cond.Broadcast()
	m.mu.Unlock()

	return nil
}

// ── HTTP handler ──────────────────────────────────────────────────────────────

// Handler returns an http.Handler that serves the LL-HLS stream under prefix.
// prefix should not have a trailing slash (e.g. "/hls/camera1").
//
// Served resources:
//
//	{prefix}/index.m3u8        – master playlist (supports blocking reload)
//	{prefix}/init.mp4          – fMP4 init segment (ftyp+moov)
//	{prefix}/seg{N}.mp4        – complete segment N
//	{prefix}/part{N}_{P}.mp4  – part P of segment N
func (m *Muxer) Handler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		path = strings.TrimPrefix(path, "/")

		switch {
		case path == "index.m3u8":
			m.servePlaylist(w, r)
		case path == "init.mp4":
			m.serveInit(w, r)
		case strings.HasPrefix(path, "seg"):
			m.serveSegment(w, r, path)
		case strings.HasPrefix(path, "part"):
			m.servePart(w, r, path)
		default:
			http.NotFound(w, r)
		}
	})
}

func (m *Muxer) serveInit(w http.ResponseWriter, _ *http.Request) {
	if len(m.initData) == 0 {
		http.Error(w, "not ready", http.StatusServiceUnavailable)

		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(m.initData)
}

// emitPart flushes the current fMP4 fragment as a new Part.
// segBoundary signals that a new segment should begin after this part.
func (m *Muxer) emitPart(nextIsKey bool) {
	indep := m.fw.FirstIsKeyframe()
	data := m.fw.Flush()

	if len(data) == 0 {
		return
	}

	dur := m.partDurAccum.Seconds()

	m.mu.Lock()

	part := &Part{
		SegSeqNo:    m.curSeg.SeqNo,
		PartIdx:     len(m.curSeg.Parts),
		Independent: indep,
		Duration:    dur,
		Data:        data,
	}

	m.curSeg.Parts = append(m.curSeg.Parts, part)
	m.curSeg.Duration += dur

	// Segment boundary: keyframe arriving AND accumulated segment duration ≥ target.
	if nextIsKey && m.curSeg.Duration >= m.cfg.SegTarget.Seconds() {
		m.finaliseSegment()
	}

	m.cond.Broadcast()
	m.mu.Unlock()

	m.partDurAccum = 0
	m.segDurAccum = 0
}

// finaliseSegment moves curSeg into the completed ring buffer and starts a new one.
// Must be called with m.mu held.
func (m *Muxer) finaliseSegment() {
	// Concatenate part data to form the full segment blob.
	var sb bytes.Buffer
	for _, p := range m.curSeg.Parts {
		sb.Write(p.Data)
	}

	m.curSeg.Data = sb.Bytes()

	if m.curSeg.Duration > m.maxSegDur {
		m.maxSegDur = m.curSeg.Duration
	}

	m.compSegs = append(m.compSegs, m.curSeg)

	// Trim ring buffer.
	if len(m.compSegs) > m.cfg.SegBufferCount {
		m.compSegs = m.compSegs[len(m.compSegs)-m.cfg.SegBufferCount:]
	}

	next := m.curSeg.SeqNo + 1
	m.curSeg = &Segment{
		SeqNo:    next,
		Duration: 0,
		WallTime: time.Now(),
		Parts:    nil,
		Data:     nil,
	}
}

//nolint:nestif
func (m *Muxer) servePlaylist(w http.ResponseWriter, r *http.Request) {
	// Parse blocking-reload parameters.
	msn, msnOK := parseUint64(r.URL.Query().Get("_HLS_msn"))
	pIdx, pIdxOK := parseInt(r.URL.Query().Get("_HLS_part"))

	if msnOK {
		if err := m.waitForPart(r.Context(), msn, pIdx, pIdxOK); err != nil {
			http.Error(w, "timeout waiting for part", http.StatusServiceUnavailable)

			return
		}
	} else {
		// Initial (non-blocking-reload) request: block until the first complete
		// segment is ready so clients never receive a playlist with no #EXTINF.
		// We pass hasPIdx=false to waitForPart which resolves once segment 0
		// appears in compSegs (i.e. has been fully finalised).
		m.mu.Lock()
		empty := !m.hasCompletedSegment()
		m.mu.Unlock()

		if empty {
			if err := m.waitForPart(r.Context(), 0, 0, false); err != nil {
				http.Error(w, "timeout waiting for first segment", http.StatusServiceUnavailable)

				return
			}
		}
	}

	playlist := m.buildPlaylist()

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(playlist)
}

// waitForPart blocks until segment msn / part pIdx is available, the context
// is cancelled, or the blocking-reload timeout expires.
func (m *Muxer) waitForPart(ctx context.Context, msn uint64, pIdx int, hasPIdx bool) error {
	deadline := time.Now().Add(m.cfg.BlockingReloadTimeout)

	// Arrange for cond.Broadcast when the deadline fires so the Wait loop exits.
	timer := time.AfterFunc(m.cfg.BlockingReloadTimeout, func() {
		m.cond.Broadcast()
	})
	defer timer.Stop()

	// Also broadcast when the caller's context is cancelled.
	ctxDone := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			m.cond.Broadcast()
		case <-ctxDone:
		}
	}()

	defer close(ctxDone)

	m.mu.Lock()
	defer m.mu.Unlock()

	for !m.partAvailable(msn, pIdx, hasPIdx) {
		if m.terminating {
			return errMuxerClosed
		}

		if time.Now().After(deadline) {
			return errBlockingReloadTimeout
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		m.cond.Wait()
	}

	return nil
}

// hasCompletedSegment reports whether at least one complete segment has been
// published (i.e. is present in compSegs). Must be called with m.mu held.
func (m *Muxer) hasCompletedSegment() bool {
	return len(m.compSegs) > 0
}

// partAvailable reports whether the requested MSN/part combination is already
// published. Must be called with m.mu held.
func (m *Muxer) partAvailable(msn uint64, pIdx int, hasPIdx bool) bool {
	// Check completed segments.
	for _, s := range m.compSegs {
		if s.SeqNo == msn {
			if !hasPIdx || pIdx < len(s.Parts) {
				return true
			}
		}

		if s.SeqNo > msn {
			return true
		}
	}

	// Check current segment.
	if m.curSeg != nil && m.curSeg.SeqNo == msn {
		if !hasPIdx {
			return false // segment not yet complete
		}

		return pIdx < len(m.curSeg.Parts)
	}

	return false
}

func (m *Muxer) serveSegment(w http.ResponseWriter, _ *http.Request, path string) {
	// path = "seg{N}.mp4"
	numStr := strings.TrimPrefix(path, "seg")
	numStr = strings.TrimSuffix(numStr, ".mp4")

	seqNo, ok := parseUint64(numStr)
	if !ok {
		http.NotFound(w, nil)

		return
	}

	m.mu.Lock()
	data := m.findSegmentData(seqNo)
	m.mu.Unlock()

	if data == nil {
		http.NotFound(w, nil)

		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "max-age=3600")
	_, _ = w.Write(data)
}

// findSegmentData returns the concatenated part data for the given segment
// sequence number. Must be called with m.mu held.
func (m *Muxer) findSegmentData(seqNo uint64) []byte {
	for _, s := range m.compSegs {
		if s.SeqNo == seqNo {
			return s.Data
		}
	}

	return nil
}

func (m *Muxer) servePart(w http.ResponseWriter, _ *http.Request, path string) {
	// path = "part{N}_{P}.mp4"
	inner := strings.TrimPrefix(path, "part")
	inner = strings.TrimSuffix(inner, ".mp4")
	sep := strings.LastIndex(inner, "_")

	if sep < 0 {
		http.NotFound(w, nil)

		return
	}

	seqNo, ok1 := parseUint64(inner[:sep])
	pIdx, ok2 := parseInt(inner[sep+1:])

	if !ok1 || !ok2 {
		http.NotFound(w, nil)

		return
	}

	m.mu.Lock()
	data := m.findPartData(seqNo, pIdx)
	m.mu.Unlock()

	if data == nil {
		http.NotFound(w, nil)

		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "max-age=3600")
	_, _ = w.Write(data)
}

// findPartData returns the raw fMP4 data for the given segment/part indices.
// Must be called with m.mu held.
func (m *Muxer) findPartData(seqNo uint64, pIdx int) []byte {
	// Search completed segments.
	for _, s := range m.compSegs {
		if s.SeqNo == seqNo {
			if pIdx >= 0 && pIdx < len(s.Parts) {
				return s.Parts[pIdx].Data
			}

			return nil
		}
	}

	// Search current segment.
	if m.curSeg != nil && m.curSeg.SeqNo == seqNo {
		if pIdx >= 0 && pIdx < len(m.curSeg.Parts) {
			return m.curSeg.Parts[pIdx].Data
		}
	}

	return nil
}

// ── Playlist generation ───────────────────────────────────────────────────────

func (m *Muxer) buildPlaylist() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	partTarget := m.cfg.PartTarget.Seconds()

	// EXT-X-TARGETDURATION must be >= ceil(max actual segment duration) per
	// RFC 8216bis §4.4.3.1. Use the measured max; fall back to config when no
	// segments have completed yet.
	targetDur := m.cfg.SegTarget.Seconds()
	if m.maxSegDur > targetDur {
		targetDur = m.maxSegDur
	}

	segTargetInt := int(math.Ceil(targetDur))
	holdBack := partTarget * 3

	var b strings.Builder

	fmt.Fprintf(&b, "#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", segTargetInt)
	fmt.Fprintf(&b, "#EXT-X-VERSION:9\n")
	fmt.Fprintf(&b, "#EXT-X-PART-INF:PART-TARGET=%.5f\n", partTarget)
	fmt.Fprintf(&b,
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=%.5f\n",
		holdBack,
	)
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"init.mp4\"\n")

	// Media sequence = sequence number of the oldest shown segment.
	firstSeq := uint64(0)
	if len(m.compSegs) > 0 {
		firstSeq = m.compSegs[0].SeqNo
	} else if m.curSeg != nil {
		firstSeq = m.curSeg.SeqNo
	}

	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", firstSeq)

	// Write completed segments.
	for _, seg := range m.compSegs {
		if !seg.WallTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				seg.WallTime.UTC().Format("2006-01-02T15:04:05.000Z"))
		}

		for _, p := range seg.Parts {
			writePartTag(&b, p)
		}

		fmt.Fprintf(&b, "#EXTINF:%.5f,\n", seg.Duration)
		fmt.Fprintf(&b, "%s\n", seg.uri())
	}

	// Write in-progress segment parts.
	if m.curSeg != nil && len(m.curSeg.Parts) > 0 {
		if !m.curSeg.WallTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				m.curSeg.WallTime.UTC().Format("2006-01-02T15:04:05.000Z"))
		}

		for _, p := range m.curSeg.Parts {
			writePartTag(&b, p)
		}

		// Preload hint for the next part.
		nextPartIdx := len(m.curSeg.Parts)
		fmt.Fprintf(&b, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"part%d_%d.mp4\"\n",
			m.curSeg.SeqNo, nextPartIdx)
	}

	return []byte(b.String())
}

func writePartTag(b *strings.Builder, p *Part) {
	if p.Independent {
		fmt.Fprintf(b, "#EXT-X-PART:DURATION=%.5f,INDEPENDENT=YES,URI=\"%s\"\n",
			p.Duration, p.uri())
	} else {
		fmt.Fprintf(b, "#EXT-X-PART:DURATION=%.5f,URI=\"%s\"\n",
			p.Duration, p.uri())
	}
}

// ── small helpers ─────────────────────────────────────────────────────────────

func parseUint64(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}

	v, err := strconv.ParseUint(s, 10, 64)

	return v, err == nil
}

func parseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}

	v, err := strconv.Atoi(s)

	return v, err == nil
}
