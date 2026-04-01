# RTSP Demuxer Plan

## Goal

Build a production-grade RTSP demuxer in `vrtc-sdk` that can replace the CGO
grabber for the supported camera classes.

The target is not "every RTSP feature in every RFC". The target is:

- reliable camera interop
- mixed audio/video track support
- RTCP/NTP-based wall-clock timing
- reconnect, keepalive, and discontinuity signaling
- clean `av.Packet` output for relay and muxer pipelines
- good automated coverage for real camera SDP/RTP patterns

## Current State

Today the RTSP implementation is limited to:

- RTSP over TCP interleaved RTP
- H.264 / H.265 video only
- no RTCP processing
- no audio depacketization
- no reconnect / keepalive loop
- narrow test coverage

## Phase 1

Scope:

- refactor the RTSP demuxer enough to support multiple media kinds
- extend SDP parsing to return audio tracks
- support PCMU / PCMA / Opus track setup and packet output
- introduce RTSP-specific track metadata for audio codecs
- improve RTSP test coverage for mixed SDP parsing and audio packet handling

Files:

- `av/codec/sdp.go`
- `av/codec/sdp_test.go`
- `av/codec/rtsp_audio.go`
- `av/format/rtsp/demuxer.go`
- `av/format/rtsp/errors.go`
- `av/format/rtsp/demuxer_test.go`
- `av/format/rtsp/aac_rtp_decoder.go`

Acceptance:

- `codec.SdpToCodecs()` returns video and supported audio tracks from mixed SDP
- RTSP `GetCodecs()` exposes audio streams when present
- PCMU / PCMA / Opus RTP packets produce `av.Packet` output with correct
  `CodecType`, `DTS`, and `Duration`

## Phase 2

Scope:

- add AAC `MPEG4-GENERIC` RTP depacketization
- parse RTCP Sender Reports
- map RTP timestamps to wall-clock time
- populate `Packet.WallClockTime`
- stop ignoring RTCP interleaved channels

Files:

- `av/format/rtsp/demuxer.go`
- `av/format/rtsp/aac_rtp_decoder.go`
- `av/format/rtsp/rtcp.go`
- `av/format/rtsp/clock.go`
- `av/format/rtsp/*_test.go`

Acceptance:

- mixed H.264/H.265 + AAC streams demux correctly
- RTCP SR updates packet wall clock
- audio/video tracks align on a shared wall-clock timeline

## Phase 3

Scope:

- reconnect and backoff strategy
- keepalive (`SET_PARAMETER`) for long-lived sessions
- discontinuity signaling on reconnect / clock resets
- `av.Pauser` support on the RTSP demuxer

Files:

- `av/format/rtsp/demuxer.go`
- `av/format/rtsp/reconnect.go`
- `av/format/rtsp/keepalive.go`
- `av/format/rtsp/errors.go`
- `av/format/rtsp/*_test.go`

Acceptance:

- long-lived idle sessions do not time out
- stream recovers after transport failure
- first packet after recovery sets `IsDiscontinuity`

## Phase 4

Scope:

- UDP unicast transport
- optional multicast
- better jitter / reordering handling
- transport-level timeout tuning

Files:

- `av/format/rtsp/udp_transport.go`
- `av/format/rtsp/demuxer.go`
- `av/format/rtsp/*_test.go`

Acceptance:

- same stream can be received over TCP interleaved or UDP unicast
- RTCP works for UDP transport too

## Phase 5

Scope:

- extend codec coverage as required by production cameras
- G.722 / G.726 / L16
- real `rtsps` TLS support
- broader fixture and replay coverage

Acceptance:

- remaining CGO-only camera profiles have a migration path

## Implementation Order

Recommended next coding order:

1. audio-capable SDP parsing
2. audio RTSP track setup
3. PCMU / PCMA / Opus output
4. AAC RTP depacketization
5. RTCP / wall-clock mapping
6. reconnect / keepalive
7. UDP transport

## Notes

- Keep the existing `codec.SdpToCodecs()` entry point for now, but extend it in
  a backward-compatible way by returning RTSP-aware audio codec wrappers.
- Keep transport/session logic out of codec parsing.
- Prefer TCP interleaved first; add UDP only after timing and reconnect logic is
  correct.
