package pva

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/pva/persistence"
)

// cacheWindowHalf is half the time window fetched from SQLite on a cache miss.
// A 5-second half-window means each reload covers 10 seconds of analytics.
const cacheWindowHalf = 5 * time.Second

// persistMatchTolerance mirrors the AnalyticsStore tolerance: a persisted
// analytics entry matches a packet if their wall-clock times are within 200 ms.
const persistMatchTolerance = 200 * time.Millisecond

// cacheEntry holds a pre-converted analytics result keyed by capture time.
type cacheEntry struct {
	captureMS int64
	analytics *av.FrameAnalytics
}

// PersistenceSource implements pva.Source by reading persisted analytics from
// SQLite via persistence.Reader. It prefetches analytics in a 10-second batch
// window and caches the results so that sequential playback incurs at most one
// SQLite round-trip per ~10 seconds of video.
type PersistenceSource struct {
	reader    *persistence.Reader
	channelID string

	mu        sync.RWMutex
	cache     []cacheEntry // sorted by captureMS ascending
	cacheFrom int64        // lower bound of cached range (unix ms, inclusive)
	cacheTo   int64        // upper bound of cached range (unix ms, exclusive)
}

// NewPersistenceSource creates a Source backed by the persistence Reader for
// the given channel.
func NewPersistenceSource(reader *persistence.Reader, channelID string) *PersistenceSource {
	return &PersistenceSource{
		reader:    reader,
		channelID: channelID,
	}
}

// Fetch returns persisted analytics for the given wall-clock time, or nil if
// none are available. It uses a batch cache to amortize SQLite lookups.
func (s *PersistenceSource) Fetch(_ int64, wallClock time.Time) *FrameAnalytics {
	if wallClock.IsZero() {
		return nil
	}

	targetMS := wallClock.UnixMilli()

	// Fast path: check the cache under a read lock.
	s.mu.RLock()

	if targetMS >= s.cacheFrom && targetMS < s.cacheTo {
		fa := s.searchCache(targetMS)
		s.mu.RUnlock()

		return fa
	}

	s.mu.RUnlock()

	// Slow path: reload cache from SQLite.
	s.reload(wallClock)

	s.mu.RLock()
	fa := s.searchCache(targetMS)
	s.mu.RUnlock()

	return fa
}

// reload queries the persistence Reader for a window around the target time
// and replaces the cache.
func (s *PersistenceSource) reload(around time.Time) {
	from := around.Add(-cacheWindowHalf)
	to := around.Add(cacheWindowHalf)

	frames, _, _ := s.reader.QueryFrames(
		context.Background(),
		s.channelID,
		from, to,
		persistence.QueryOpts{Limit: 1000},
	)

	entries := make([]cacheEntry, 0, len(frames))
	for _, f := range frames {
		entries = append(entries, cacheEntry{
			captureMS: f.CaptureMS,
			analytics: toFrameAnalytics(f),
		})
	}

	s.mu.Lock()
	s.cache = entries
	s.cacheFrom = from.UnixMilli()
	s.cacheTo = to.UnixMilli()
	s.mu.Unlock()
}

// searchCache performs a binary search for the nearest entry within
// persistMatchTolerance. Must be called with at least s.mu.RLock held.
func (s *PersistenceSource) searchCache(targetMS int64) *av.FrameAnalytics {
	if len(s.cache) == 0 {
		return nil
	}

	tolMS := persistMatchTolerance.Milliseconds()

	// Binary search for the first entry >= targetMS.
	pos := sort.Search(len(s.cache), func(i int) bool {
		return s.cache[i].captureMS >= targetMS
	})

	var best *cacheEntry

	if pos < len(s.cache) {
		e := &s.cache[pos]
		if absDiffInt64(e.captureMS, targetMS) <= tolMS {
			best = e
		}
	}

	if pos > 0 {
		e := &s.cache[pos-1]
		if absDiffInt64(e.captureMS, targetMS) <= tolMS {
			if best == nil ||
				absDiffInt64(e.captureMS, targetMS) < absDiffInt64(best.captureMS, targetMS) {
				best = e
			}
		}
	}

	if best == nil {
		return nil
	}

	return best.analytics
}

func absDiffInt64(a, b int64) int64 {
	if a > b {
		return a - b
	}

	return b - a
}

// toFrameAnalytics converts a persistence FrameWithDetections to an
// av.FrameAnalytics suitable for attaching to av.Packet.Analytics.
func toFrameAnalytics(f persistence.FrameWithDetections) *av.FrameAnalytics {
	fa := &av.FrameAnalytics{
		SiteID:       f.SiteID,
		ChannelID:    f.ChannelID,
		FramePTS:     f.FramePTS,
		CaptureMS:    f.CaptureMS,
		CaptureEndMS: f.CaptureEndMS,
		InferenceMS:  f.InferenceMS,
		RefWidth:     f.RefWidth,
		RefHeight:    f.RefHeight,
		VehicleCount: f.VehicleCount,
		PeopleCount:  f.PeopleCount,
	}

	if len(f.Detections) > 0 {
		fa.Objects = make([]*av.Detection, len(f.Detections))
		for i, d := range f.Detections {
			fa.Objects[i] = &av.Detection{
				X:          uint32(d.X),
				Y:          uint32(d.Y),
				W:          uint32(d.W),
				H:          uint32(d.H),
				ClassID:    uint32(d.ClassID),
				Confidence: uint32(d.Confidence),
				TrackID:    d.TrackID,
				IsEvent:    d.IsEvent,
			}
		}
	}

	return fa
}

// ── CompositeSource ─────────────────────────────────────────────────────────

// CompositeSource tries Primary first; if it returns nil, falls back to
// Fallback. This enables seamless transition from persisted analytics
// (historical) to live analytics (in-memory store) during follow-mode playback.
type CompositeSource struct {
	Primary  Source
	Fallback Source
}

// Fetch tries the primary source first, then the fallback.
func (c *CompositeSource) Fetch(frameID int64, wallClock time.Time) *FrameAnalytics {
	if fa := c.Primary.Fetch(frameID, wallClock); fa != nil {
		return fa
	}

	if c.Fallback != nil {
		return c.Fallback.Fetch(frameID, wallClock)
	}

	return nil
}
