# Keyframe Cache for Instant Live View Start

## Problem

When recording is already pulling an RTSP stream, a new live view consumer (browser) must wait up to one GOP interval (1-2 seconds) for the next keyframe before the fMP4 muxer can emit its first fragment. The target is sub-500ms live view start.

## Root Cause

`av/relayhub/relay.go` dispatches packets to consumers as they arrive from the demuxer. A late-joining consumer misses the last keyframe and must wait for the next one. The relay has no packet cache.

## Proposed Solution

Cache the last video keyframe (+ any preceding codec change packets) in the `Relay` struct. When a new consumer is added via `AddConsumer`, send the cached keyframe immediately after `WriteHeader` so the consumer's muxer can emit its first fragment without waiting.

## Implementation

### File: `av/relayhub/relay.go`

**Add to Relay struct:**
```go
type Relay struct {
    // ... existing fields ...

    // Cached last video keyframe for instant start of late-joining consumers.
    lastKeyMu   sync.RWMutex
    lastKeyframe *av.Packet // most recent video keyframe
}
```

**Update packet dispatch loop** (around line 460, in the `readDemuxerLoop`):
```go
// After dispatching to consumers, cache keyframes for late joiners.
if pkt.KeyFrame && pkt.CodecType.IsVideo() {
    cached := pkt // copy
    m.lastKeyMu.Lock()
    m.lastKeyframe = &cached
    m.lastKeyMu.Unlock()
}
```

**Update `AddConsumer`** (around line 380, after `WriteHeader`):
```go
// Send cached keyframe for instant start.
m.lastKeyMu.RLock()
cached := m.lastKeyframe
m.lastKeyMu.RUnlock()
if cached != nil {
    _ = c.WritePacket(ctx, *cached)
}

return nil
```

### File: `av/relayhub/consumer.go`

No changes needed — `WritePacket` already accepts packets before the packet loop starts (they queue in the channel).

## Edge Cases

- **No keyframe cached yet**: First consumer after relay start — behaves as today, waits for first keyframe from demuxer
- **Codec change after cache**: The `pkt.NewCodecs` field on the cached keyframe handles this — consumer processes codec change before the frame
- **Multiple video streams**: Cache per-stream index if needed, but typically there's only one video stream per relay
- **Memory**: One keyframe per relay (~50-200KB for HEVC 1080p) — negligible

## Verification

1. Start edge-runtime with recording enabled
2. Wait for recording to be active (keyframes flowing)
3. Open `/live/{camera_id}` in browser — measure time to first frame
4. Target: <500ms from WebSocket connect to first video frame rendered

## Impact

- `av/relayhub/relay.go` — ~15 lines added
- No API changes, no breaking changes
- All existing consumers unaffected — they just get an extra packet at join time
