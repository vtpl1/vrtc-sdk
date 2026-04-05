# fMP4 Fine-Grained Seek (10ms granularity)

**Status: Complete.** Both phases are implemented and integrated.

- Phase 1 — `fmp4.Demuxer.SeekToKeyframe` + `ChainingDemuxer` seek: `av/format/fmp4/demuxer.go`, `av/chain/chain.go`
- Phase 2 — sidx write on segment close: `av/format/fmp4/muxer.go` (`BuildSidx`, `FragIndex`, `VideoTrackInfo`, `MediaStartPos`), `av/segment/muxer.go` (`writeSidx` called from `Close`)

---

## Problem

Recorded playback seek currently lands on the nearest segment boundary (~7 min / 64MB chunks), then reads sequentially from segment start until the first keyframe. Effective granularity is one GOP interval (1-2 seconds). Target is 10ms-level seek.

## Root Cause

1. **No seek index in fMP4 files**: `segment.SegmentMuxer` writes `moof+mdat` fragments sequentially but no `sidx` (segment index) or `mfra` (movie fragment random access) boxes
2. **ChainingDemuxer reads from file start**: No ability to skip to a specific byte offset within a segment file
3. **Keyframe-only start**: fMP4 muxer can only begin a new fragment on a video keyframe

## Proposed Solution (Two Phases)

### Phase 1: Keyframe-accurate seek (< 2 second granularity)

Scan packets in the segment file to find the keyframe nearest to (but not after) the seek timestamp. Start playback from that keyframe.

**Files:**
- `av/format/fmp4/demuxer.go` — Add `SeekToKeyframe(pts time.Duration) error` method that scans `moof` boxes for the target PTS
- `av/chain/chain.go` — `ChainingDemuxer` should open the correct segment file and call `SeekToKeyframe` instead of reading from byte 0

**Approach:**
- Parse `moof.traf.tfdt` (track fragment decode time) to find the fragment containing the target PTS
- Seek the file reader to that `moof` offset
- Read forward until the first keyframe packet at or before the target PTS
- With 2-second GOPs at 25fps, this gives ~2 second granularity

### Phase 2: Frame-accurate seek (10ms granularity)

Write a `sidx` box at the end of each segment file mapping PTS ranges → byte offsets for every fragment. Use it for O(1) random access.

**Files:**
- `av/segment/muxer.go` — Accumulate fragment metadata (PTS, byte offset, size, keyframe flag) during writes. Write `sidx` box on segment close.
- `av/format/fmp4/muxer.go` — Track fragment byte offsets as `moof+mdat` pairs are written
- `av/format/fmp4/demuxer.go` — Read `sidx` box on open if present, use for binary search to target PTS

**sidx box structure (ISO 14496-12):**
```
sidx {
  reference_ID: track ID
  timescale: 90000
  earliest_presentation_time: first PTS
  entries: [
    { referenced_size, subsegment_duration, starts_with_SAP, SAP_type }
  ]
}
```

Each entry maps a PTS range to a byte range in the file. Binary search on PTS gives O(log N) seek to the exact fragment.

**Frame-accurate delivery:**
- After seeking to the correct fragment, skip packets until `pkt.PTS >= seekPTS`
- For video: start from the preceding keyframe, discard P/B frames before seekPTS (decoder needs the keyframe reference)
- Effective granularity: one frame interval (40ms at 25fps, 33ms at 30fps)
- For true 10ms: interpolate between frames (not practical for surveillance)

## Realistic Granularity

| Level | Granularity | Requirement |
|-------|------------|-------------|
| Segment-level | ~7 min | Current (no changes) |
| Keyframe-level | 1-2 sec | Phase 1 (scan moof boxes) |
| Fragment-level | ~2 sec | Phase 2 (sidx index) |
| Frame-level | 33-40ms | Phase 2 + PTS skip |
| Sub-frame | <33ms | Not applicable to video |

**Practical target: frame-level (33-40ms)** — this is the finest granularity that makes sense for video. 10ms is below one frame interval and indistinguishable visually.

## Impact

- Phase 1: ~50 lines in fmp4 demuxer + chain demuxer
- Phase 2: ~100 lines in segment muxer + fmp4 muxer/demuxer
- No breaking API changes — `SeekToKeyframe` is an optional method
- Existing recordings without `sidx` fall back to Phase 1 sequential scan
- New recordings get `sidx` automatically

## Verification

1. Record a camera for 10 minutes
2. Seek to a specific timestamp (e.g., 5:23.456)
3. Verify first displayed frame PTS is within one frame interval of requested time
4. Measure seek latency: target < 100ms from seek request to first frame delivered
