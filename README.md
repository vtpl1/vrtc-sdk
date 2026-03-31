# vrtc-sdk

A Go library for building audio/video pipelines. It provides the core data model, codec utilities, container formats, and a fan-out relay hub used by the `vrtc` edge and recording services.

**Module:** `github.com/vtpl1/vrtc-sdk`  
**Version:** v0.1.1  
**Go version:** 1.26+

---

## Package map

| Package | Purpose |
|---------|---------|
| `av` | Core types: `Packet`, `Stream`, `CodecData`, `Demuxer`, `Muxer`, codec constants |
| `av/relayhub` | Fan-out coordinator: one demuxer → N muxer consumers |
| `av/codec/h264parser` | H.264 SPS/PPS extraction, AVCC↔Annex B conversion |
| `av/codec/h265parser` | H.265 VPS/SPS/PPS extraction, RTP reassembly, AVCC↔Annex B |
| `av/codec/aacparser` | MPEG-4 audio config, ADTS framing |
| `av/codec/pcm` | PCM/FLAC/µ-law/A-law codec data and transcoding |
| `av/codec/parser` | Generic NALU splitting and format detection (raw / Annex B / AVCC) |
| `av/codec` | SDP → `[]CodecData` parsing; Opus codec data |
| `av/format/fmp4` | Fragmented MP4 (ISO 14496-12) muxer and demuxer |
| `av/format/mp4` | Standard MP4 muxer and demuxer |
| `av/format/grpc` | gRPC transport: PushStream (client→server) and PullStream (server→client) with pause/seek |
| `av/format/llhls` | Low-Latency HLS (CMAF) muxer with built-in HTTP handler |
| `av/format/mse` | fMP4-over-WebSocket muxer for browser Media Source Extensions |
| `lifecycle` | `StartStopper` / `Stopper` interfaces and signal helpers |

---

## Core data model

### Packet

`av.Packet` is the unit of data flowing through the pipeline:

```go
type Packet struct {
    KeyFrame        bool
    IsDiscontinuity bool
    Idx             uint16        // matches Stream.Idx from GetCodecs
    CodecType       CodecType
    FrameID         int64
    DTS             time.Duration // monotonically non-decreasing per stream
    PTSOffset       time.Duration // PTS = DTS + PTSOffset
    Duration        time.Duration
    WallClockTime   time.Time
    Data            []byte        // AVCC for H.264/H.265; raw encoded samples for audio
    Analytics       *FrameAnalytics
    NewCodecs       []Stream      // non-nil on mid-stream codec change
}
```

**H.264 and H.265 `Data` is always in AVCC format** (ISO 14496-15): each NALU prefixed with a 4-byte big-endian length field (lengthSizeMinusOne=3). The spec allows 1- or 2-byte length fields but this library always uses 4-byte. Use `av/codec/parser` to convert between AVCC and Annex B.

### Stream and CodecData

```go
type Stream struct {
    Idx   uint16    // primary key; may be non-contiguous (e.g. MPEG-TS PIDs)
    Codec CodecData // type-assert to VideoCodecData or AudioCodecData
}
```

`VideoCodecData` adds `Width()`, `Height()`, `TimeScale()`.  
`AudioCodecData` adds `SampleFormat()`, `SampleRate()`, `ChannelLayout()`, `PacketDuration()`.

### Codec type constants

```go
// Video
av.H264, av.H265, av.MJPEG, av.JPEG, av.VP8, av.VP9, av.AV1

// Audio
av.AAC, av.OPUS, av.PCM, av.PCM_MULAW, av.PCM_ALAW, av.PCML, av.FLAC, av.MP3, av.ELD
```

---

## Interfaces

### Demuxer / Muxer

```go
type Demuxer interface {
    GetCodecs(ctx context.Context) ([]Stream, error)
    ReadPacket(ctx context.Context) (Packet, error)
}

type Muxer interface {
    WriteHeader(ctx context.Context, streams []Stream) error
    WritePacket(ctx context.Context, pkt Packet) error
    WriteTrailer(ctx context.Context, upstreamError error) error
}
```

Optional capabilities are accessed by type assertion:

```go
if p, ok := dmx.(av.Pauser);     ok { p.Pause(ctx) }
if s, ok := dmx.(av.TimeSeeker); ok { s.SeekToTime(ctx, 30*time.Second) }
if c, ok := mux.(av.CodecChanger); ok { c.WriteCodecChange(ctx, changed) }
```

### Factory and remover functions

```go
type DemuxerFactory func(ctx context.Context, sourceID string) (DemuxCloser, error)
type MuxerFactory   func(ctx context.Context, consumerID string) (MuxCloser, error)
```

These are the primary extension points — pass them into `relayhub.New` to wire up any source/sink combination.

---

## RelayHub

`RelayHub` fans packets from one demuxer out to N muxer consumers. Relays are created on-demand and reclaimed automatically when idle (~1 s after the last consumer disconnects).

```go
hub := relayhub.New(demuxerFactory, demuxerRemover)
if err := hub.Start(ctx); err != nil { /* handle */ }
defer hub.Stop()

handle, err := hub.Consume(ctx, "rtsp://camera-1/stream", av.ConsumeOptions{
    ConsumerID:   "recorder-a",
    MuxerFactory: muxFactory,
    MuxerRemover: muxRemover,
    ErrChan:      errCh,
})
if err != nil { /* handle */ }
defer handle.Close(ctx)
```

**Delivery policy**

| Consumers on relay | Behaviour |
|--------------------|-----------|
| 1 | Blocking write — back-pressure propagates to `ReadPacket`; no frames are dropped |
| 2+ | Leaky write — a slow consumer drops frames rather than stalling others |

---

## Format containers

### Fragmented MP4 (`av/format/fmp4`)

Fragments are emitted on each video keyframe (or each packet for audio-only streams).

```go
mux := fmp4.NewMuxer(w)
mux.WriteHeader(ctx, streams)
for _, pkt := range packets {
    mux.WritePacket(ctx, pkt)
}
mux.WriteTrailer(ctx, nil)
```

The demuxer reads standard fMP4 files and CMAF streams produced by this muxer.

### gRPC transport (`av/format/grpc`)

Bidirectional streaming transport for AV packets between vrtc nodes over gRPC. The `AVTransportService` defines two streaming RPCs plus control RPCs:

| RPC | Direction | Purpose |
|-----|-----------|---------|
| `PushStream` | client → server | Edge pushes packets to cloud (client-streaming) |
| `PullStream` | server → client | Consumer pulls packets from cloud (server-streaming) |
| `PauseStream` / `ResumeStream` | unary | Pause/resume packet delivery |
| `SeekStream` | unary | Seek to a keyframe-aligned position |

```go
// Server side — register with a gRPC server
srv := avgrpc.NewServer(demuxerFactory, demuxerRemover)
avtransportv1.RegisterAVTransportServiceServer(grpcServer, srv)

// Client side — push packets to a remote server
mux, _ := avgrpc.NewClientMuxer(ctx, conn, "rtsp://camera-1/stream")
mux.WriteHeader(ctx, streams)
// ... WritePacket loop ...
mux.WriteTrailer(ctx, nil)

// Client side — pull packets from a remote server
dmx, _ := avgrpc.NewClientDemuxer(ctx, conn, "rtsp://camera-1/stream")
streams, _ := dmx.GetCodecs(ctx)
for {
    pkt, err := dmx.ReadPacket(ctx)
    // ...
}
```

`ClientDemuxer` also implements `av.Pauser` and `av.TimeSeeker` via the control RPCs.

### Low-Latency HLS (`av/format/llhls`)

Packages media as CMAF parts and serves an LL-HLS playlist over HTTP.

```go
m := llhls.NewMuxer(llhls.DefaultConfig())
http.Handle("/hls/", m.Handler("/hls"))
m.WriteHeader(ctx, streams)
// ... WritePacket loop ...
// Clients reach the playlist at /hls/index.m3u8
```

### MSE WebSocket (`av/format/mse`)

Delivers fMP4 fragments and JSON metadata to browser `MediaSource` clients over WebSocket.

```go
// Pre-opened writers (long-lived WebSocket connection)
w, _ := mse.NewFromWriters(binaryWS, jsonWS)

// Or factory mode (one writer per frame, e.g. HTTP chunked)
w, _ := mse.NewFromFactories(binaryFactory, jsonFactory)

w.WriteHeader(ctx, streams)  // sends codec string + init segment
// ... WritePacket loop ...
```

PCM µ-law and A-law streams are automatically transcoded to FLAC for browser compatibility.

---

## Lifecycle

```go
// Service components implement lifecycle.StartStopper:
type StartStopper interface {
    Start(ctx context.Context) error
    SignalStop() bool   // async — returns true on first call
    WaitStop() error    // blocks until stopped
    Stop() error        // SignalStop + WaitStop
}

// Block until SIGINT/SIGTERM or an error:
lifecycle.WaitForTerminationRequest(errChan)
```

---

## Development

```bash
cd vrtc-sdk

make           # fmt + lint + build
make build     # go build ./...
make lint      # golangci-lint run --fix ./...
make fmt       # gofumpt -l -w -extra .
make test      # go test -race -count=1 ./...
make update    # go get -u ./... && go mod tidy

# Single package
go test -race -count=1 ./av/relayhub/...

# Single test
go test -race -count=1 -run TestName ./av/relayhub/...
```

### Workspace usage

This module is consumed by `github.com/vtpl1/vrtc` via a Go workspace. The
`go.work` file at the workspace root wires them together locally:

```
use ./vrtc-sdk
use ./vrtc

replace github.com/vtpl1/vrtc-sdk v0.1.1 => ./vrtc-sdk
```

The `replace` directive is required because the module is not yet published at `v0.1.1`.
