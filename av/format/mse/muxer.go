// Package mse implements an av.MuxCloser that multiplexes fMP4 fragments to
// WebSocket clients for consumption by the browser's Media Source Extensions API.
//
// Protocol (per connection):
//
//  1. Client sends:  {"type":"mse","value":""}
//  2. Server sends:  {"type":"mse","value":"video/mp4; codecs=\"hvc1.1.6.L153.B0,flac\""} (text)
//  3. Server sends:  fMP4 init segment (binary)
//  4. Server sends:  fMP4 media fragments (binary) as they are produced
//  5. If a packet carries Analytics, the analytics object is JSON-marshalled and
//     sent as an additional text frame.
package mse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

var (
	// ErrFailedToCreateBinaryWriter is returned when BinaryWriterFactory produces
	// a nil writer or returns an error.
	ErrFailedToCreateBinaryWriter = errors.New("failed to create binary writer")

	// ErrFailedToCreateJSONWriter is returned when TextWriterFactory produces
	// a nil writer or returns an error.
	ErrFailedToCreateJSONWriter = errors.New("failed to create JSON writer")
)

type (
	// BinaryWriterFactory creates a WriteCloser for binary (fMP4 fragment) output.
	// In factory mode the MSEWriter calls it once per outgoing binary frame, so
	// each frame can be sent over a fresh WebSocket message or HTTP chunk.
	BinaryWriterFactory func() (io.WriteCloser, error)

	// TextWriterFactory creates a WriteCloser for text (JSON metadata) output.
	// Behaves the same as BinaryWriterFactory but for text frames.
	TextWriterFactory func() (io.WriteCloser, error)
)

// messageKind distinguishes binary (media) from text (JSON metadata) output
// frames without importing a WebSocket library.
type messageKind int

const (
	messageBinary messageKind = iota
	messageText
)

// wsMessage is the JSON envelope used for all text-channel messages.
type wsMessage struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// outFrame is a single output frame queued for delivery to a client.
type outFrame struct {
	kind messageKind
	data []byte
}

// MSEWriter is an av.MuxCloser that emits WebSocket-friendly text and binary
// frames through caller-provided writers.
//
// Create it with NewFromWriters or NewFromFactories, then drive it like any
// other av.MuxCloser: WriteHeader -> WritePacket* -> WriteTrailer -> Close.
type MSEWriter struct {
	binaryWriter  io.WriteCloser
	binaryFactory BinaryWriterFactory

	jsonWriter  io.WriteCloser
	jsonFactory TextWriterFactory

	// mu serialises fmp4.Muxer writes and the shared buf.
	mu      sync.Mutex
	buf     bytes.Buffer
	mux     *fmp4.Muxer
	streams []av.Stream // current codec state (after PCM→FLAC substitution)

	// pcmEncoders maps stream Idx → FLAC frame encoder for G.711 µ/A-law streams.
	// Populated in WriteHeader and updated in WriteCodecChange.
	pcmEncoders map[uint16]func([]byte) []byte

	// codecsReady is closed exactly once when WriteHeader succeeds.
	// Clients block on it until codec info is available.
	codecsReady chan struct{}

	// codecStr and initSeg are set by WriteHeader and updated by WriteCodecChange.
	codecsMu sync.RWMutex
	codecStr string
	initSeg  []byte

	closed    chan struct{}
	closeOnce sync.Once
}

// NewFromWriters creates an MSEWriter that writes all binary and text frames to
// the supplied pre-opened writers. Suitable when the connection is established
// before codec negotiation (e.g. a long-lived WebSocket connection).
func NewFromWriters(binaryWriter, jsonWriter io.WriteCloser) (*MSEWriter, error) {
	m := &MSEWriter{
		binaryWriter: binaryWriter,
		jsonWriter:   jsonWriter,
		codecsReady:  make(chan struct{}),
		closed:       make(chan struct{}),
	}
	m.mux = fmp4.NewMuxer(&m.buf)

	return m, nil
}

// NewFromFactories creates an MSEWriter that opens a fresh writer from the
// given factories for each outgoing frame. Suitable for HTTP chunked responses
// or connection-per-frame transports.
func NewFromFactories(
	binaryFactory BinaryWriterFactory,
	jsonFactory TextWriterFactory,
) (*MSEWriter, error) {
	m := &MSEWriter{
		binaryFactory: binaryFactory,
		jsonFactory:   jsonFactory,
		codecsReady:   make(chan struct{}),
		closed:        make(chan struct{}),
	}
	m.mux = fmp4.NewMuxer(&m.buf)

	return m, nil
}

// ── PCM→FLAC transcoding ──────────────────────────────────────────────────────

// transcodePCM replaces PCM_MULAW and PCM_ALAW streams with equivalent FLAC
// streams and returns per-stream encoder functions for packet-level conversion.
// Streams that are not G.711 are passed through unchanged.
func transcodePCM(streams []av.Stream) ([]av.Stream, map[uint16]func([]byte) []byte) {
	out := make([]av.Stream, len(streams))
	encoders := make(map[uint16]func([]byte) []byte, len(streams))

	for i, s := range streams {
		switch c := s.Codec.(type) {
		case pcm.PCMMulawCodecData:
			if enc := pcm.FLACEncoder(av.PCM_MULAW, uint32(c.SampleRate())); enc != nil {
				encoders[s.Idx] = enc
				out[i] = av.Stream{
					Idx: s.Idx,
					Codec: pcm.NewFLACCodecData(
						av.PCM_MULAW,
						uint32(c.SampleRate()),
						c.ChannelLayout(),
					),
				}

				continue
			}
		case pcm.PCMAlawCodecData:
			if enc := pcm.FLACEncoder(av.PCM_ALAW, uint32(c.SampleRate())); enc != nil {
				encoders[s.Idx] = enc
				out[i] = av.Stream{
					Idx: s.Idx,
					Codec: pcm.NewFLACCodecData(
						av.PCM_ALAW,
						uint32(c.SampleRate()),
						c.ChannelLayout(),
					),
				}

				continue
			}
		}

		out[i] = s
	}

	return out, encoders
}

// ── av.MuxCloser ───────────────────────────────────────────────────────────────

// WriteHeader declares all streams, writes the fMP4 init segment, and unblocks
// any WebSocket clients that are waiting for codec information.
func (m *MSEWriter) WriteHeader(ctx context.Context, streams []av.Stream) error {
	transcoded, encoders := transcodePCM(streams)

	m.mu.Lock()
	m.streams = cloneStreams(transcoded)
	m.pcmEncoders = encoders
	m.buf.Reset()
	err := m.mux.WriteHeader(ctx, transcoded)
	data := cloneBytes(m.buf.Bytes())
	m.mu.Unlock()

	m.codecsMu.Lock()
	codecStr := buildCodecString(transcoded)
	m.codecStr = codecStr
	m.initSeg = data
	m.codecsMu.Unlock()

	// Unblock waiting clients (idempotent — select prevents double-close).
	select {
	case <-m.codecsReady: // already closed
	default:
		close(m.codecsReady)
	}

	if meta, jerr := json.Marshal(wsMessage{Type: "mse", Value: codecStr}); jerr == nil {
		if err := m.broadcast(outFrame{messageText, meta}); err != nil {
			return err
		}
	}

	if len(data) > 0 {
		if err := m.broadcast(outFrame{messageBinary, data}); err != nil {
			return err
		}
	}

	return err
}

// WritePacket buffers a sample; flushes and broadcasts a binary fMP4 fragment on
// each video keyframe (or immediately for audio-only streams). If pkt.Analytics is
// non-nil it is marshalled to JSON and broadcast as a text message.
//
// Ordering: when pkt is a keyframe it triggers a flush of the previous GOP.
// The completed fragment (binary) is sent first, followed by any per-frame
// metadata that annotates the keyframe that just opened the next segment.
func (m *MSEWriter) WritePacket(ctx context.Context, pkt av.Packet) error {
	m.mu.Lock()
	if enc, ok := m.pcmEncoders[pkt.Idx]; ok {
		pkt.Data = enc(pkt.Data)
	}

	m.buf.Reset()
	err := m.mux.WritePacket(ctx, pkt)
	data := cloneBytes(m.buf.Bytes())
	m.mu.Unlock()

	if len(data) > 0 {
		if err := m.broadcast(outFrame{messageBinary, data}); err != nil {
			return err
		}
	}

	if pkt.Analytics != nil {
		if meta, jerr := json.Marshal(pkt.Analytics); jerr == nil {
			if err := m.broadcast(outFrame{messageText, meta}); err != nil {
				return err
			}
		}
	}

	return err
}

// WriteTrailer flushes any buffered samples and broadcasts the final fragment.
func (m *MSEWriter) WriteTrailer(ctx context.Context, upstreamErr error) error {
	m.mu.Lock()
	m.buf.Reset()
	err := m.mux.WriteTrailer(ctx, upstreamErr)
	data := cloneBytes(m.buf.Bytes())
	m.mu.Unlock()

	if len(data) > 0 {
		if err := m.broadcast(outFrame{messageBinary, data}); err != nil {
			return err
		}
	}

	return err
}

// WriteCodecChange implements av.CodecChanger. It flushes the current fragment,
// broadcasts the codec-change data to existing clients, and stores a fresh init
// segment and updated codec string for clients that connect after the change.
//
// If the resulting codec string is identical to the current one (e.g. the
// upstream source re-sends SPS/PPS in-band on every keyframe without actually
// changing them), the call is a no-op: no new init segment is written and
// nothing is sent over the WebSocket. This prevents the browser's SourceBuffer
// from being reset on every keyframe interval.
func (m *MSEWriter) WriteCodecChange(ctx context.Context, changed []av.Stream) error {
	transcodedChanged, newEncoders := transcodePCM(changed)

	// Compute the updated stream list with the changed entries merged in.
	m.mu.Lock()
	maps.Copy(m.pcmEncoders, newEncoders)

	for _, c := range transcodedChanged {
		for i, existing := range m.streams {
			if existing.Idx == c.Idx {
				m.streams[i] = c

				break
			}
		}
	}

	updatedStreams := cloneStreams(m.streams)
	m.mu.Unlock()

	newCodecStr := buildCodecString(updatedStreams)

	// Skip everything if the codec string hasn't changed — the upstream is
	// just refreshing in-band parameter sets, not changing the actual codec.
	m.codecsMu.RLock()
	unchanged := newCodecStr == m.codecStr
	m.codecsMu.RUnlock()

	if unchanged {
		return nil
	}

	// Actual codec change: flush the current fragment and write a new init segment.
	m.mu.Lock()
	m.buf.Reset()
	err := m.mux.WriteCodecChange(ctx, transcodedChanged)
	data := cloneBytes(m.buf.Bytes())
	m.mu.Unlock()

	// Rebuild a clean init-only segment for late joiners (no fragment prefix).
	var initBuf bytes.Buffer

	timeOutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tmp := fmp4.NewMuxer(&initBuf)
	if herr := tmp.WriteHeader(timeOutCtx, updatedStreams); herr == nil {
		m.codecsMu.Lock()
		m.codecStr = newCodecStr
		m.initSeg = cloneBytes(initBuf.Bytes())
		m.codecsMu.Unlock()
	}

	// Notify the connected client of the codec change before the new segment
	// data arrives so the browser can call sourceBuffer.changeType() in time.
	if meta, jerr := json.Marshal(wsMessage{Type: "mse", Value: newCodecStr}); jerr == nil {
		if err := m.broadcast(outFrame{messageText, meta}); err != nil {
			return err
		}
	}

	if err := m.broadcast(outFrame{messageBinary, data}); err != nil {
		return err
	}

	return err
}

// Close flushes remaining samples and closes pre-opened writers.
// Safe to call multiple times.
func (m *MSEWriter) Close() error {
	m.closeOnce.Do(func() {
		_ = m.WriteTrailer(context.Background(), nil)
		close(m.closed)

		// Close pre-opened writers (from NewFromWriters). Factory-created
		// writers are closed per-frame in broadcast and are not our concern.
		if m.binaryFactory == nil && m.binaryWriter != nil {
			_ = m.binaryWriter.Close()
		}

		if m.jsonFactory == nil && m.jsonWriter != nil {
			_ = m.jsonWriter.Close()
		}
	})

	return nil
}

// buildCodecString builds a MIME type + codecs string from the stream list.
// Codecs with a Tag() method (H.264, H.265, AAC) are used directly; FLAC is
// added as "flac".
func buildCodecString(streams []av.Stream) string {
	type tagger interface{ Tag() string }

	var parts []string

	for _, s := range streams {
		switch c := s.Codec.(type) {
		case tagger:
			parts = append(parts, c.Tag())
		case pcm.FLACCodecData:
			_ = c

			parts = append(parts, "flac")
		}
	}

	if len(parts) == 0 {
		return `video/mp4; codecs=""`
	}

	return `video/mp4; codecs="` + strings.Join(parts, ",") + `"`
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}

	c := make([]byte, len(b))
	copy(c, b)

	return c
}

func cloneStreams(ss []av.Stream) []av.Stream {
	c := make([]av.Stream, len(ss))
	copy(c, ss)

	return c
}

func (m *MSEWriter) broadcast(frame outFrame) error {
	switch frame.kind {
	case messageBinary:
		if m.binaryFactory != nil {
			w, err := m.binaryFactory()
			if err != nil {
				return errors.Join(ErrFailedToCreateBinaryWriter, err)
			}

			if w == nil {
				return ErrFailedToCreateBinaryWriter
			}

			m.binaryWriter = w
		}

		if _, err := m.binaryWriter.Write(frame.data); err != nil {
			return err
		}

		if m.binaryFactory != nil {
			err := m.binaryWriter.Close()
			m.binaryWriter = nil

			if err != nil {
				return err
			}
		}
	case messageText:
		if m.jsonFactory != nil {
			w, err := m.jsonFactory()
			if err != nil {
				return errors.Join(ErrFailedToCreateJSONWriter, err)
			}

			if w == nil {
				return ErrFailedToCreateJSONWriter
			}

			m.jsonWriter = w
		}

		if _, err := m.jsonWriter.Write(frame.data); err != nil {
			return err
		}

		if m.jsonFactory != nil {
			err := m.jsonWriter.Close()
			m.jsonWriter = nil

			if err != nil {
				return err
			}
		}
	}

	return nil
}
