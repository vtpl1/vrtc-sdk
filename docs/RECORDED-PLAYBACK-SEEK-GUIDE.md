# Recorded Playback & Frame-Accurate Seek: Frontend Developer Guide

This guide explains how to implement recorded video playback with frame-accurate seeking using wall-clock referenced events. It covers the timestamp model, API contracts, and the two-stage seek strategy (server-side coarse + browser-side fine).

## Architecture Overview

```
  Frontend (Browser)                         Edge Runtime
  ┌──────────────────────┐                  ┌──────────────────────────┐
  │                      │   1. Timeline    │                          │
  │  Timeline Bar ───────┼──GET /api/───────┼─▶ MemStore (segment      │
  │  (wall-clock)        │  timeline/{id}   │    index with start/end) │
  │                      │                  │                          │
  │                      │  2. Playback     │                          │
  │  <video> + MSE ◀─────┼──WS /ws/────────┼─▶ ChainingDemuxer        │
  │  (0-based time)      │  recorded       │    ├─ segment1.fmp4       │
  │                      │                  │    ├─ segment2.fmp4       │
  │                      │  3. Analytics    │    └─ ...                 │
  │  Event Overlay ◀─────┼──JSON text ──────┼─▶ emsg / FrameAnalytics  │
  │  (wall-clock)        │  frames          │    (captureMs field)      │
  └──────────────────────┘                  └──────────────────────────┘
```

## Timestamp Model

There are **two time domains** the frontend must bridge:

| Domain | Source | Example | Used By |
|--------|--------|---------|---------|
| **Wall-clock** (absolute) | Camera RTCP Sender Reports | `2026-04-02T17:43:23Z` | Timeline API, seek commands, analytics events, segment filenames |
| **Media time** (relative) | `video.currentTime` | `0.000` ... `42.700` | MSE buffer, `<video>` element, frame display |

The server always normalises media timestamps to a **zero-based origin** when streaming to the browser. This means `video.currentTime = 0` corresponds to the wall-clock time the frontend requested as `start`.

### The Mapping Formula

```
wallClockTime = streamStartWallClock + video.currentTime

video.currentTime = wallClockTime - streamStartWallClock
```

Where `streamStartWallClock` is the wall-clock time the frontend sent as the `start` parameter (or the seek target time). The frontend **already knows** this value because it chose it.

> **Important:** On seek, the server restarts the stream. `video.currentTime` resets to 0. The frontend must update `streamStartWallClock` to the new seek target.

## API Reference

### 1. Timeline: `GET /api/cameras/{camera_id}/timeline`

Returns available recording periods for the timeline bar.

**Query Parameters:**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `start` | RFC3339 | 24h ago | Timeline range start |
| `end` | RFC3339 | now | Timeline range end |

**Response:**

```jsonc
[
  {
    "start": "2026-04-02T17:39:23Z",   // segment wall-clock start
    "end":   "2026-04-02T17:41:23Z",   // segment wall-clock end
    "duration_ms": 120000,
    "has_events": true
  },
  // ... more segments
]
```

Use this to render the timeline bar. Gaps between segments indicate periods with no recording.

### 2. WebSocket Playback: `WS /ws/stream`

Streams fMP4 fragments over WebSocket for MSE consumption. Always operates in
follow mode — playback continues into live when it reaches the end of recorded
segments.

**Query Parameters:**

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `camera_id` | string | yes | Camera/channel identifier |
| `start` | RFC3339 | no | Wall-clock start time. Omit for live mode. |

**Client -> Server messages (JSON text frames):**

```jsonc
// Start playback
{"type": "mse"}

// Pause / resume
{"type": "mse", "value": "pause"}
{"type": "mse", "value": "resume"}

// Seek to wall-clock time
{"type": "mse", "value": "seek", "time": "2026-04-02T17:45:00Z"}
```

**Server -> Client messages:**

| Order | Type | Content |
|-------|------|---------|
| 1 | Text | `{"type":"mse","value":"video/mp4; codecs=\"avc1.42E01E\""}` |
| 2 | Binary | fMP4 init segment (ftyp + moov) |
| 3+ | Binary | fMP4 fragments (moof + mdat), continuous |
| interleaved | Text | `FrameAnalytics` JSON (when analytics are present) |

### 3. HTTP Playback: `GET /api/cameras/{camera_id}/stream`

Returns chunked `video/mp4` stream. Omit `start` for live; provide `start`
(RFC3339) for recorded playback in follow mode. Simpler than WebSocket but no
bidirectional control (no pause/seek commands).

## Implementation Guide

### Step 1: Timeline Bar

```typescript
interface TimelineSegment {
  start: string;   // RFC3339
  end: string;     // RFC3339
  duration_ms: number;
  has_events: boolean;
}

async function fetchTimeline(
  cameraId: string,
  start: Date,
  end: Date,
): Promise<TimelineSegment[]> {
  const params = new URLSearchParams({
    start: start.toISOString(),
    end: end.toISOString(),
  });
  const res = await fetch(`/api/cameras/${cameraId}/timeline?${params}`);
  return res.json();
}
```

Render segments as filled bars on the timeline. Gaps = no recording.

### Step 2: MSE Playback with WebSocket

```typescript
interface PlaybackState {
  ws: WebSocket;
  mediaSource: MediaSource;
  sourceBuffer: SourceBuffer | null;
  streamStartWallClock: Date;   // the wall-clock anchor
  mimeType: string;
}

function startPlayback(cameraId: string, startTime: Date): PlaybackState {
  const video = document.querySelector('video')!;
  const ms = new MediaSource();
  video.src = URL.createObjectURL(ms);

  const state: PlaybackState = {
    ws: new WebSocket(
      `/api/cameras/ws/stream?camera_id=${cameraId}&start=${startTime.toISOString()}`
    ),
    mediaSource: ms,
    sourceBuffer: null,
    streamStartWallClock: startTime,
    mimeType: '',
  };

  state.ws.binaryType = 'arraybuffer';

  state.ws.onopen = () => {
    state.ws.send(JSON.stringify({ type: 'mse' }));
  };

  state.ws.onmessage = (event) => {
    if (typeof event.data === 'string') {
      handleTextMessage(state, video, JSON.parse(event.data));
    } else {
      handleBinaryMessage(state, event.data as ArrayBuffer);
    }
  };

  return state;
}
```

### Step 3: Handle Server Messages

```typescript
function handleTextMessage(
  state: PlaybackState,
  video: HTMLVideoElement,
  msg: { type: string; value: unknown },
): void {
  if (msg.type === 'mse' && typeof msg.value === 'string') {
    // Codec negotiation — create SourceBuffer
    state.mimeType = msg.value as string;
    const sb = state.mediaSource.addSourceBuffer(state.mimeType);
    sb.mode = 'segments';
    state.sourceBuffer = sb;
    return;
  }

  // Analytics event (FrameAnalytics JSON)
  if (msg.type === undefined && 'captureMs' in (msg as any)) {
    handleAnalyticsEvent(state, video, msg as FrameAnalytics);
  }
}

function handleBinaryMessage(state: PlaybackState, data: ArrayBuffer): void {
  if (!state.sourceBuffer) return;

  if (state.sourceBuffer.updating) {
    // Queue if SourceBuffer is busy
    state.sourceBuffer.addEventListener('updateend', () => {
      state.sourceBuffer!.appendBuffer(data);
    }, { once: true });
  } else {
    state.sourceBuffer.appendBuffer(data);
  }
}
```

### Step 4: Wall-Clock <-> Media Time Conversion

This is the critical piece. The frontend maintains the anchor:

```typescript
// Convert video.currentTime to wall-clock
function mediaTimeToWallClock(state: PlaybackState, mediaTimeSec: number): Date {
  return new Date(state.streamStartWallClock.getTime() + mediaTimeSec * 1000);
}

// Convert wall-clock to video.currentTime
function wallClockToMediaTime(state: PlaybackState, wallClock: Date): number {
  return (wallClock.getTime() - state.streamStartWallClock.getTime()) / 1000;
}

// Show current wall-clock time in the UI
function updateTimeDisplay(state: PlaybackState, video: HTMLVideoElement): void {
  const wallClock = mediaTimeToWallClock(state, video.currentTime);
  document.getElementById('time-display')!.textContent =
    wallClock.toLocaleTimeString();
}
```

### Step 5: Two-Stage Seek (Coarse + Fine)

Seeking to a wall-clock time is a two-stage process:

1. **Server-side (coarse):** Find the right segment, seek to the nearest keyframe (~1s tolerance)
2. **Browser-side (fine):** Advance `video.currentTime` to the exact frame (50ms precision at 20fps)

```typescript
async function seekToWallClock(
  state: PlaybackState,
  video: HTMLVideoElement,
  targetWallClock: Date,
): Promise<void> {
  // Stage 1: Tell the server to seek (coarse — lands on keyframe)
  // The server restarts the stream from the keyframe at or before the target.
  state.ws.send(JSON.stringify({
    type: 'mse',
    value: 'seek',
    time: targetWallClock.toISOString(),
  }));

  // Update the wall-clock anchor.
  // The server seeks to the keyframe AT OR BEFORE the target, so the actual
  // stream start is slightly earlier. We set the anchor to the target and
  // accept that video.currentTime may start slightly negative (the browser
  // handles this gracefully) — OR we can set it to the keyframe time if the
  // server reports it.
  //
  // Simplest correct approach: set anchor to the seek target.
  state.streamStartWallClock = targetWallClock;

  // The server will send a new init segment + fragments.
  // The SourceBuffer must be reset.
  await resetSourceBuffer(state);

  // Stage 2: Once enough data is buffered, set currentTime to 0 (= target).
  // The browser decodes from the keyframe and displays the closest frame.
  video.addEventListener('canplay', () => {
    video.currentTime = 0;
    video.play();
  }, { once: true });
}

async function resetSourceBuffer(state: PlaybackState): Promise<void> {
  const sb = state.sourceBuffer;
  if (!sb) return;

  // Wait for any pending append to finish.
  if (sb.updating) {
    await new Promise<void>(resolve =>
      sb.addEventListener('updateend', () => resolve(), { once: true })
    );
  }

  // Remove old buffered data.
  const buffered = sb.buffered;
  if (buffered.length > 0) {
    sb.remove(buffered.start(0), buffered.end(buffered.length - 1));
    await new Promise<void>(resolve =>
      sb.addEventListener('updateend', () => resolve(), { once: true })
    );
  }
}
```

### Step 6: Correlate Analytics Events with Video Frames

Analytics events arrive as JSON text frames on the WebSocket with wall-clock timestamps:

```typescript
interface FrameAnalytics {
  siteId: number;
  channelId: number;
  framePts: number;       // camera PTS ticks
  captureMs: number;      // wall-clock epoch ms when frame was captured
  captureEndMs: number;
  inferenceMs: number;    // wall-clock epoch ms when inference completed
  refWidth: number;       // frame dimensions for bbox normalisation
  refHeight: number;
  vehicleCount: number;
  peopleCount: number;
  objects: Detection[] | null;
}

interface Detection {
  x: number; y: number; w: number; h: number;  // bbox in pixels
  classId: number;
  confidence: number;     // 0-100
  trackId: number;        // cross-frame tracking ID
  isEvent: boolean;       // triggered an alert rule
}
```

To overlay detections on the correct video frame:

```typescript
function handleAnalyticsEvent(
  state: PlaybackState,
  video: HTMLVideoElement,
  analytics: FrameAnalytics,
): void {
  // Convert analytics wall-clock to media time
  const eventWallClock = new Date(analytics.captureMs);
  const mediaTime = wallClockToMediaTime(state, eventWallClock);

  // Schedule overlay at the right video time
  if (analytics.objects) {
    scheduleOverlay(video, mediaTime, analytics);
  }
}

function scheduleOverlay(
  video: HTMLVideoElement,
  mediaTime: number,
  analytics: FrameAnalytics,
): void {
  // Use requestVideoFrameCallback for frame-accurate overlay timing
  video.requestVideoFrameCallback((_now, metadata) => {
    const currentMediaTime = metadata.mediaTime;

    // Check if this frame matches the analytics event (within 1 frame)
    const frameDuration = 1 / 20; // 50ms at 20fps
    if (Math.abs(currentMediaTime - mediaTime) < frameDuration) {
      drawDetections(video, analytics);
    }

    // Re-register for continuous overlay
    video.requestVideoFrameCallback(arguments.callee as any);
  });
}

function drawDetections(
  video: HTMLVideoElement,
  analytics: FrameAnalytics,
): void {
  const canvas = document.getElementById('overlay') as HTMLCanvasElement;
  const ctx = canvas.getContext('2d')!;
  ctx.clearRect(0, 0, canvas.width, canvas.height);

  // Scale from analytics reference dimensions to canvas
  const scaleX = canvas.width / analytics.refWidth;
  const scaleY = canvas.height / analytics.refHeight;

  for (const det of analytics.objects ?? []) {
    ctx.strokeStyle = det.isEvent ? '#ff0000' : '#00ff00';
    ctx.lineWidth = 2;
    ctx.strokeRect(
      det.x * scaleX,
      det.y * scaleY,
      det.w * scaleX,
      det.h * scaleY,
    );

    ctx.fillStyle = ctx.strokeStyle;
    ctx.font = '12px monospace';
    ctx.fillText(
      `class=${det.classId} conf=${det.confidence}%`,
      det.x * scaleX,
      det.y * scaleY - 4,
    );
  }
}
```

### Step 7: Timeline Click Handler

Wire timeline clicks to the two-stage seek:

```typescript
function onTimelineClick(
  state: PlaybackState,
  video: HTMLVideoElement,
  timelineEl: HTMLElement,
  event: MouseEvent,
  rangeStart: Date,
  rangeEnd: Date,
): void {
  const rect = timelineEl.getBoundingClientRect();
  const fraction = (event.clientX - rect.left) / rect.width;
  const targetMs =
    rangeStart.getTime() + fraction * (rangeEnd.getTime() - rangeStart.getTime());
  const targetWallClock = new Date(targetMs);

  seekToWallClock(state, video, targetWallClock);
}
```

### Step 8: Continuous Time Display

Keep the timeline cursor and clock display in sync during playback:

```typescript
function startTimeSync(state: PlaybackState, video: HTMLVideoElement): void {
  const timeDisplay = document.getElementById('time-display')!;
  const cursor = document.getElementById('timeline-cursor')!;

  function update(): void {
    if (video.paused && !video.seeking) return;

    const wallClock = mediaTimeToWallClock(state, video.currentTime);
    timeDisplay.textContent = wallClock.toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      fractionalSecondDigits: 1,
    });

    // Move timeline cursor to the corresponding position
    // (depends on your timeline range — adapt to your layout)

    requestAnimationFrame(update);
  }

  video.addEventListener('play', () => requestAnimationFrame(update));
  video.addEventListener('seeked', () => requestAnimationFrame(update));
}
```

## Seek Precision Summary

| Stage | Precision | Who | Mechanism |
|-------|-----------|-----|-----------|
| Segment lookup | exact | Server | SQLite recording index maps wall-clock → segment file |
| Fragment seek (with sidx) | ~2s (fragment interval) | Server | `SeekToKeyframe()` binary-searches the `sidx` box written at segment close |
| Fragment seek (no sidx) | ~1s (GOP interval) | Server | `SeekToKeyframe()` scans `moof` boxes sequentially (fallback for old recordings) |
| Frame display | 1 frame (33–40ms @ 25–30fps) | Browser | MSE decodes from keyframe; `video.currentTime` snaps to the nearest frame |

**Effective end-to-end precision: 1 frame.** All segments written by `SegmentMuxer` include a `sidx` box, enabling O(log N) random access within a segment. The browser covers the remaining sub-GOP gap by decoding forward from the keyframe.

## Timing Diagram

```
Wall-clock: ──17:43:20─────17:43:21─────17:43:22─────17:43:23───▶

Server:     Segment contains keyframes at 17:43:20 and 17:43:21
            User clicks timeline at 17:43:21.7

            1. Server finds segment covering 17:43:21.7
            2. SeekToKeyframe lands on keyframe at 17:43:21 (700ms before target)
            3. Server streams from 17:43:21 onward

Browser:    video.currentTime:  0.0 ──── 0.35 ──── 0.70 ──── 1.0
            Wall-clock display: 17:43:21  17:43:21.35  17:43:21.70  17:43:22

            4. Browser decodes frames from keyframe (0.0s)
            5. video.currentTime reaches 0.7s = the exact target frame
            6. If paused, browser displays that frame
```

## emsg Box Format (Advanced)

Analytics are also embedded in fMP4 fragments as ISO 14496-12 emsg boxes. This is an alternative to the JSON text frames for players that process raw fMP4 (non-WebSocket path).

```
emsg (version 1) {
  scheme_id_uri: "urn:vtpl:analytics:1"    // identifies our analytics schema
  value:         ""
  timescale:     1000                       // millisecond resolution
  presentation_time: <media_time_ms>        // 0-based media time in ms
  event_duration: 0xFFFFFFFF                // unbounded
  id:            <monotonic_counter>
  message_data:  <FrameAnalytics JSON>      // same structure as text frames
}
```

The `presentation_time` in emsg is **media time** (0-based, matching `video.currentTime * 1000`), not wall-clock. Convert using the anchor:

```typescript
const wallClockMs = state.streamStartWallClock.getTime() + emsg.presentationTime;
```

## Follow Mode (Live Tail of Recording)

Both HTTP and WebSocket playback always operate in follow mode. The server
polls for new segments and streams them as they appear, seamlessly
transitioning into live when recorded footage runs out.

```typescript
const ws = new WebSocket(
  `/api/cameras/ws/stream?camera_id=${cameraId}&start=${startTime.toISOString()}`
);
```

In follow mode, `video.currentTime` continues to grow as new fragments arrive. The wall-clock formula remains the same.

## Error Handling

| Error | Response | Recovery |
|-------|----------|----------|
| Invalid camera ID | HTTP 400 / WS `{"type":"error","error":"..."}` | Show error to user |
| No recordings | HTTP 404 | Grey out timeline, show "No recordings" |
| Invalid seek time | WS `{"type":"error","error":"invalid seek time, expected RFC3339"}` | Validate before sending |
| Segment gap during playback | Stream may have a brief stall | ChainingDemuxer handles segment transitions transparently |
| WebSocket disconnect | `ws.onclose` fires | Reconnect with the last known wall-clock position |

## Quick Reference

```
Timeline API:       GET  /api/cameras/{camera_id}/timeline?start=RFC3339&end=RFC3339
Recordings API:     GET  /api/cameras/{camera_id}/recordings?start=RFC3339&end=RFC3339
Playback (HTTP):    GET  /api/cameras/{camera_id}/stream?start=RFC3339
Playback (WS):      WS   /api/cameras/ws/stream?cameraId=ID&start=RFC3339

Wall-clock -> mediaTime:  (wallClock.getTime() - streamStartWallClock.getTime()) / 1000
mediaTime -> wall-clock:  new Date(streamStartWallClock.getTime() + video.currentTime * 1000)
```
