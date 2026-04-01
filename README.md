# vrtc-sdk

A Go library for building audio/video pipelines. It provides the core data model, codec utilities, container formats, and a fan-out relay hub used by the `vrtc` edge and recording services.

**Module:** `github.com/vtpl1/vrtc-sdk`  
**Version:** v0.3.0
**Go version:** 1.26+

---

## Package map

| Package | Purpose |
|---------|---------|
| `av` | Core types: `Packet`, `Stream`, `CodecData`, `Demuxer`, `Muxer`, codec constants |
| `av/chain` | Multi-segment chaining demuxer with `SegmentSource` abstraction for sequential playback |
| `av/segment` | Segment recording helpers: storage-aware file writer, size-based rotation, optional fragment ring buffer, segment validation |
| `av/packetbuf` | Packet-level ring buffer with `DemuxCloser` replay for near-live playback |
| `av/relayhub` | Fan-out coordinator: one demuxer → N muxer consumers, with muxer rotation support |
| `av/codec/h264parser` | H.264 SPS/PPS extraction, AVCC↔Annex B conversion |
| `av/codec/h265parser` | H.265 VPS/SPS/PPS extraction, RTP reassembly, AVCC↔Annex B |
| `av/codec/aacparser` | MPEG-4 audio config, ADTS framing |
| `av/codec/pcm` | PCM/FLAC/µ-law/A-law codec data and transcoding |
| `av/codec/parser` | Generic NALU splitting and format detection (raw / Annex B / AVCC) |
| `av/codec` | SDP → `[]CodecData` parsing; Opus codec data |
| `av/format/fmp4` | Fragmented MP4 (ISO 14496-12) muxer and demuxer |
| `av/format/mp4` | Standard MP4 muxer and demuxer |
| `av/format/grpc` | gRPC transport: PushStream (client→server) and PullStream (server→client) with pause/seek |
| `av/format/rtsp` | RTSP demuxer with RTP/H.264 packet reassembly |
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

**Operational notes**

- `Start(ctx)` must be called before `Consume`; otherwise `Consume` returns `ErrRelayHubNotStartedYet`.
- The first `Consume` for a `sourceID` creates a relay, queues it for startup, waits for `GetCodecs`, primes the consumer with the initial stream list, and then returns the consumer handle.
- If the demuxer factory or initial `GetCodecs` call fails during startup, the relay stores that error and later `Consume` calls return it wrapped with `ErrRelayLastError`.
- If a running relay later fails during `ReadPacket`, the relay shuts down and is reclaimed by the hub cleanup loop; that read error is not persisted in `RelayStats.LastError`.
- `PauseRelay` and `ResumeRelay` forward to the underlying demuxer only when it implements `av.Pauser`; otherwise they are no-ops.
- `Stop()` cancels the hub context, closes all active relays, and waits for relay and consumer goroutines to exit.

### Relay stats

`GetRelayStats(ctx)` returns one `av.RelayStats` per active relay:

```go
type RelayStats struct {
    ID             string
    ConsumerCount  int
    PacketsRead    uint64
    BytesRead      uint64
    KeyFrames      uint64
    DroppedPackets uint64
    StartedAt      time.Time
    LastPacketAt   time.Time
    LastError      string
    Streams        []StreamInfo
    ActualFPS      float64
    BitrateBps     float64
}

type StreamInfo struct {
    Idx        uint16
    CodecType  CodecType
    Width      int
    Height     int
    SampleRate int
}
```

Notes:

- `Streams` is derived from the relay's current codec headers and is refreshed on mid-stream codec changes (`pkt.NewCodecs`).
- Today the relay replaces its stored headers with `pkt.NewCodecs` verbatim. If a demuxer emits partial codec-change updates, `Streams` may contain only the changed streams rather than the full stream set.
- `ActualFPS` is computed as `PacketsRead / elapsedSecondsSinceStart`, so for mixed audio/video relays it is an aggregate packet rate, not strictly video frame rate.
- `BitrateBps` is computed as `(BytesRead * 8) / elapsedSecondsSinceStart`.
- `DroppedPackets` increments only in the `2+` consumer leaky-delivery mode when a consumer queue is full.

### RelayHub architecture

Each level has a single responsibility:

- `RelayHub`: owns the `sourceID -> relay` map, starts relays on demand, and removes idle relays on a 1 s ticker.
- `Relay`: owns one upstream `DemuxCloser`, tracks current codec headers and stats, reads packets, and fans them out to active consumers.
- `Consumer`: owns one downstream `MuxCloser`, buffers packets in a bounded queue, writes headers/trailers, and reports async write failures via `ErrChan`.

Data flow:

```text
Consume(sourceID, consumerID)
    |
    v
RelayHub
    |- create relay on first consumer for sourceID
    |- enqueue relay start
    `- attach consumer
          |
          v
      Relay
          |- demuxerFactory(sourceID)
          |- GetCodecs() -> current headers
          |- readWriteLoop():
          |    ReadPacket() -> update stats -> fan out packet
          `- close when hub stops or after the hub cleanup tick observes zero consumers
                |
                v
            Consumer
                |- muxerFactory(consumerID)
                |- async muxer open + WriteHeader(headers)
                |- WritePacket(pkt)
                |- optional WriteCodecChange(pkt.NewCodecs)
                `- WriteTrailer() on shutdown
```

Goroutine model:

- `RelayHub.Start` launches one background goroutine that handles relay startup, hub shutdown, and idle-relay cleanup.
- `Relay.Start` launches the demuxer lifecycle goroutine plus a dedicated packet read/fan-out loop.
- `Consumer.Start` launches one goroutine per consumer that opens the muxer and drains that consumer's packet queue.

Cleanup model:

- Hub-level cleanup removes relays whose `ConsumerCount()` reaches zero.
- Relay-level cleanup closes inactive consumers and invokes the optional `DemuxerRemover` with a detached 5 s timeout context.
- Consumer-level cleanup invokes `WriteTrailer`, closes the muxer, and then invokes the optional `MuxerRemover` with a detached 5 s timeout context.

### Muxer rotation

A consumer's `MuxCloser` may return `relayhub.ErrMuxerRotate` from `WritePacket` to
signal that the current segment is full. The consumer handles this automatically:

1. Calls `WriteTrailer` + `Close` on the current muxer
2. Calls `MuxerFactory` again to open a new muxer
3. Sends `WriteHeader` with the current codec headers
4. Re-delivers the keyframe packet that triggered rotation

This enables size-based segment rotation without any coordination between the
relay and the recording layer. The `segment.SegmentMuxer` uses this mechanism
internally when `maxSegmentBytes > 0`.

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

### Segment recording (`av/segment`)

Builds on top of `av/format/fmp4` for file-per-segment recording with size-based
rotation, preallocation, and optional in-memory replay.

```go
ring := segment.NewRingBuffer(10 * time.Second) // optional, can be nil

mux, err := segment.NewSegmentMuxer(
    "segments/2026-03-31T12-00-00Z.mp4",
    time.Now().UTC(),
    segment.ProfileAuto, // auto-detect: SSD/HDD/NAS/SAN
    64<<20,              // max segment size (bytes); 0 = no limit
    72<<20,              // preallocation size (target + headroom); 0 = none
    ring,
    func(info segment.SegmentCloseInfo) {
        // info includes Path, Start/End, SizeBytes, analytics flags, validation result
        if info.ValidationError != nil {
            // handle invalid/corrupt segment
        }
    },
)
if err != nil { /* handle */ }

if err := mux.WriteHeader(ctx, streams); err != nil { /* handle */ }
for _, pkt := range packets {
    if err := mux.WritePacket(ctx, pkt); err != nil { /* handle */ }
}
if err := mux.Close(); err != nil { /* handle */ }
```

**Size-based rotation:** When `maxSegmentBytes > 0`, the muxer returns
`relayhub.ErrMuxerRotate` from `WritePacket` at the first keyframe after the
threshold is crossed. When used with a relay hub consumer, rotation is handled
automatically — the consumer closes the current muxer and opens a new one via
the `MuxerFactory`. The triggering keyframe is re-delivered to the new muxer.

**Fixed-size files:** When both `maxSegmentBytes` and `preallocBytes` are set,
the muxer preallocates the file via `fallocate(2)` and writes an ISO BMFF
`free` box to pad unused space on close. This keeps all segment files at the
exact preallocated size on disk, eliminating filesystem fragmentation from
variable-size files.

**Recommended sizes:** 64 MB for 1–4 Mbps streams, 128 MB for 4–8+ Mbps.

`SegmentMuxer` tracks analytics-derived flags per segment:

| Field | Set when |
|-------|----------|
| `HasMotion` | Any written packet has non-nil `pkt.Analytics` |
| `HasObjects` | Any written packet has `len(pkt.Analytics.Objects) > 0` |

`ValidateSegment(path)` performs a fast structural check (`ftyp` present, valid box sizes, at least one `moof`).
Use sentinel errors such as `ErrSegmentEmpty`, `ErrSegmentNoFtyp`, and `ErrSegmentNoMoof` for error handling.

### Chaining demuxer (`av/chain`)

Chains multiple segment demuxers into a single monotonic `av.DemuxCloser` stream.
DTS values are adjusted at each segment boundary so timestamps remain monotonically
non-decreasing across all segments.

```go
// Open the first segment eagerly (fail fast).
first, _ := openSegment(paths[0])

// Provide remaining segments lazily via SegmentSource.
src := chain.SliceSource(paths[1:], func(ctx context.Context, path string) (av.DemuxCloser, error) {
    return openSegment(path)
})

dmx := chain.NewChainingDemuxer(first, src)
streams, _ := dmx.GetCodecs(ctx)
for {
    pkt, err := dmx.ReadPacket(ctx)
    if err == io.EOF { break }
    // pkt.DTS is monotonically non-decreasing across all segments
}
dmx.Close()
```

Implement `chain.SegmentSource` for custom sources (e.g. polling a recording index for new segments in follow mode):

```go
type SegmentSource interface {
    Next(ctx context.Context) (av.DemuxCloser, error) // io.EOF when done; may block
}
```

### Packet buffer (`av/packetbuf`)

A time-limited ring buffer of `av.Packet` values for near-live replay. The write
side pushes packets (typically from a recording muxer via a tee), and the read
side creates `DemuxCloser` instances that replay buffered packets then follow
new ones in real time.

```go
buf := packetbuf.New(30 * time.Second)

// Write side — push packets as they arrive (thread-safe).
buf.WriteHeader(streams)
buf.WritePacket(pkt) // sets WallClockTime if zero

// Read side — replay from a given wall-clock time with live follow.
dmx := buf.Demuxer(time.Now().Add(-5 * time.Second))
streams, _ := dmx.GetCodecs(ctx)
for {
    pkt, err := dmx.ReadPacket(ctx) // blocks waiting for new packets
    if err == io.EOF { break }      // buffer closed
}
dmx.Close()

buf.Close() // wakes all waiting Demuxers with io.EOF
```

**Seamless recorded-to-live playback:** Use `packetbuf.Buffer` together with
`chain.ChainingDemuxer` to bridge the gap between completed disk segments and
the live stream. The recording consumer tees packets to both disk and the buffer.
The playback path chains disk segments, then transitions to `buf.Demuxer(lastSegmentEnd)`
for the live tail:

```text
ChainingDemuxer
  ├─ Segment 1 (disk, complete)
  ├─ Segment 2 (disk, complete)
  └─ PacketBuffer.Demuxer(lastSegEnd) ← seamless near-live tail
```

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
srv := avgrpc.NewServer(
    nil, // optional PushHandler
    func(ctx context.Context, sourceID, consumerID string, mux av.MuxCloser) error {
        handle, err := hub.Consume(ctx, sourceID, av.ConsumeOptions{
            ConsumerID: consumerID,
            MuxerFactory: func(context.Context, string) (av.MuxCloser, error) {
                return mux, nil
            },
        })
        if err != nil { return err }
        defer handle.Close(ctx)
        <-ctx.Done()
        return nil
    },
    avgrpc.WithPauseHandler(func(ctx context.Context, sourceID string, pause bool) error {
        if pause {
            return hub.PauseRelay(ctx, sourceID)
        }
        return hub.ResumeRelay(ctx, sourceID)
    }),
)
avtransportv1.RegisterAVTransportServiceServer(grpcServer, srv)

// Optional: when using PushStream, expose pushed sources as a DemuxerFactory.
demuxerFactory := srv.DemuxerFactory()

// Client side — push packets to a remote server.
// NewClientMuxer returns a muxer directly; it opens PushStream on WriteHeader.
mux := avgrpc.NewClientMuxer(conn, "rtsp://camera-1/stream")
mux.WriteHeader(ctx, streams)
// ... WritePacket loop ...
mux.WriteTrailer(ctx, nil)

// Client side — pull packets from a remote server.
// consumerID is required by the constructor.
dmx := avgrpc.NewClientDemuxer(conn, "rtsp://camera-1/stream", "viewer-a")
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
```
