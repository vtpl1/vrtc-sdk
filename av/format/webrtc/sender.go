// Package webrtc provides a WebRTC sender that implements av.MuxCloser.
//
// This is a placeholder. The full implementation will use pion/webrtc v3 to
// RTP-packetise and send av.Packet data to a browser or WebRTC peer.
//
// Implementation guide (see internal/avgrabber/docs/go/06-muxer-webrtc.md):
//
//   - WriteHeader: store []av.Stream; build SDP offer using stored codec info
//     (H.264 → MimeTypeH264, H.265 → MimeTypeH265, Opus → MimeTypeOpus,
//     G.711µ → MimeTypePCMU, G.711A → MimeTypePCMA).
//   - WritePacket: for video (H.264/H.265) pass Annex-B data directly to
//     webrtc.TrackLocalStaticSample.WriteSample; for audio strip ADTS if AAC.
//     Compute sample Duration from consecutive DTS deltas.
//   - WriteCodecChange: trigger re-negotiation or reconnect the track.
//   - RTCP / PLI: read from RTP sender; call session.Stop+Resume to force IDR.
//   - Discontinuity: tolerate RTP timestamp jump (pion handles wrap).
//
// Dependencies (add when implementing):
//
//	go get github.com/pion/webrtc/v3
//	go get github.com/pion/rtp
package webrtc

import (
	"context"
	"errors"

	"github.com/vtpl1/vrtc-sdk/av"
)

// ErrNotImplemented is returned by all Sender methods until the full
// pion/webrtc implementation is wired in.
var ErrNotImplemented = errors.New("webrtc: not implemented")

// Sender is a stub av.MuxCloser that will send media over WebRTC.
// Construct with NewSender and pass a *webrtc.PeerConnection once implemented.
type Sender struct {
	streams []av.Stream
}

// NewSender returns an unimplemented Sender stub.
// Replace the body with a real pion PeerConnection when implementing.
func NewSender() *Sender {
	return &Sender{}
}

// WriteHeader stores the stream descriptors for later SDP construction.
// Implements av.Muxer.
func (s *Sender) WriteHeader(_ context.Context, streams []av.Stream) error {
	s.streams = streams

	return nil
}

// WritePacket is not yet implemented.
// Implements av.Muxer.
func (s *Sender) WritePacket(_ context.Context, _ av.Packet) error {
	return ErrNotImplemented
}

// WriteTrailer is a no-op for WebRTC (no container trailer needed).
// Implements av.Muxer.
func (s *Sender) WriteTrailer(_ context.Context, _ error) error {
	return nil
}

// WriteCodecChange is not yet implemented.
// Implements av.CodecChanger.
func (s *Sender) WriteCodecChange(_ context.Context, changed []av.Stream) error {
	s.streams = changed

	return nil
}

// Close is a no-op until the PeerConnection is wired in.
// Implements av.MuxCloser.
func (s *Sender) Close() error {
	return nil
}
