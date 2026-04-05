package pva

import (
	"sort"
	"sync"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// ─── AnalyticsStore ──────────────────────────────────────────────────────────

// entry holds a stored analytics result together with its wall-clock key.
type entry struct {
	wallClock time.Time
	analytics *av.FrameAnalytics
}

// AnalyticsStore is a thread-safe, time-indexed store for FrameAnalytics
// keyed by (sourceID, wallClockTime). Entries expire after ttl.
//
// The store is fed by the analytics ingestion gRPC server via Put.
// It is consumed by MetadataMerger via the pva.Source returned by SourceFor.
type AnalyticsStore struct {
	mu  sync.RWMutex
	ttl time.Duration

	// data maps sourceID → sorted slice of entries (sorted by wallClock ascending).
	data map[string][]entry
}

// NewAnalyticsStore creates a store with the given TTL (entries older than ttl
// are lazily evicted on Put). A ttl of 30 s is recommended for live pipelines.
func NewAnalyticsStore(ttl time.Duration) *AnalyticsStore {
	return &AnalyticsStore{
		ttl:  ttl,
		data: make(map[string][]entry),
	}
}

// Put stores analytics for (sourceID, wallClock). Entries beyond ttl are
// evicted before inserting to bound memory usage.
func (s *AnalyticsStore) Put(sourceID string, wallClock time.Time, a *av.FrameAnalytics) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := s.data[sourceID]

	// Evict expired entries.
	cutoff := time.Now().Add(-s.ttl)
	i := sort.Search(len(entries), func(i int) bool {
		return entries[i].wallClock.After(cutoff)
	})
	entries = entries[i:]

	// Insert in sorted order.
	e := entry{wallClock: wallClock, analytics: a}
	pos := sort.Search(len(entries), func(i int) bool {
		return !entries[i].wallClock.Before(wallClock)
	})
	entries = append(entries, entry{})
	copy(entries[pos+1:], entries[pos:])
	entries[pos] = e

	s.data[sourceID] = entries
}

// SourceFor returns a pva.Source for the given camera sourceID.
// Fetch(frameID, wallClock) looks up stored analytics at wallClock within ±200 ms.
//
// When no analytics entry matches, the source returns nil. The MSE writer
// emits a separate {"type":"timing"} text frame for continuous wall-clock
// delivery, independent of analytics.
func (s *AnalyticsStore) SourceFor(sourceID string) Source {
	return &sourcedStore{store: s, sourceID: sourceID}
}

const matchTolerance = 200 * time.Millisecond

// lookup finds the nearest analytics entry to target within matchTolerance.
// Returns nil if no entry is within tolerance.
func (s *AnalyticsStore) lookup(sourceID string, target time.Time) *av.FrameAnalytics {
	s.mu.RLock()
	entries := append([]entry(nil), s.data[sourceID]...)
	s.mu.RUnlock()

	if len(entries) == 0 {
		return nil
	}

	// Binary search for the first entry ≥ target.
	pos := sort.Search(len(entries), func(i int) bool {
		return !entries[i].wallClock.Before(target)
	})

	var best *entry

	if pos < len(entries) {
		e := &entries[pos]
		if diff(e.wallClock, target) <= matchTolerance {
			best = e
		}
	}

	if pos > 0 {
		e := &entries[pos-1]
		if diff(e.wallClock, target) <= matchTolerance {
			if best == nil || diff(e.wallClock, target) < diff(best.wallClock, target) {
				best = e
			}
		}
	}

	if best == nil {
		return nil
	}

	return best.analytics
}

func diff(a, b time.Time) time.Duration {
	d := a.Sub(b)
	if d < 0 {
		return -d
	}

	return d
}

type sourcedStore struct {
	store    *AnalyticsStore
	sourceID string
}

func (ss *sourcedStore) Fetch(_ int64, wallClock time.Time) *FrameAnalytics {
	return ss.store.lookup(ss.sourceID, wallClock)
}

// ─── AnalyticsHub ────────────────────────────────────────────────────────────

const hubChanBufSize = 16

// AnalyticsHub is a per-sourceID pub/sub broadcaster for FrameAnalytics.
// It is fed by the analytics ingestion gRPC server and consumed by the
// /api/cameras/ws/analytics WebSocket handler.
type AnalyticsHub struct {
	mu   sync.RWMutex
	subs map[string][]chan *av.FrameAnalytics
}

// NewAnalyticsHub creates an empty hub.
func NewAnalyticsHub() *AnalyticsHub {
	return &AnalyticsHub{subs: make(map[string][]chan *av.FrameAnalytics)}
}

// Subscribe registers a subscriber for sourceID. The caller must call
// Unsubscribe with the returned channel when done. The channel is buffered;
// slow consumers will lose frames rather than blocking the broadcast.
func (h *AnalyticsHub) Subscribe(sourceID string) <-chan *av.FrameAnalytics {
	ch := make(chan *av.FrameAnalytics, hubChanBufSize)

	h.mu.Lock()
	h.subs[sourceID] = append(h.subs[sourceID], ch)
	h.mu.Unlock()

	return ch
}

// Unsubscribe removes the given channel from the sourceID subscriber list
// and closes it.
func (h *AnalyticsHub) Unsubscribe(sourceID string, ch <-chan *av.FrameAnalytics) {
	h.mu.Lock()
	defer h.mu.Unlock()

	list := h.subs[sourceID]
	for i, c := range list {
		if c == ch {
			h.subs[sourceID] = append(list[:i], list[i+1:]...)

			close(c)

			break
		}
	}
}

// Broadcast sends analytics to all current subscribers of sourceID.
// Non-blocking: frames are dropped for subscribers whose buffer is full.
func (h *AnalyticsHub) Broadcast(sourceID string, a *av.FrameAnalytics) {
	h.mu.RLock()
	list := h.subs[sourceID]
	h.mu.RUnlock()

	for _, ch := range list {
		select {
		case ch <- a:
		default: // slow consumer — drop
		}
	}
}

// ─── AnalyticsPipeline ───────────────────────────────────────────────────────

// PersistenceWriter is the subset of persistence.Writer used by the pipeline.
type PersistenceWriter interface {
	Enqueue(channelID string, wallClock time.Time, a *av.FrameAnalytics)
}

// AnalyticsPipeline wires the AnalyticsStore and AnalyticsHub together as a
// single handler called by the gRPC AnalyticsIngestionServer on every result.
// An optional PersistenceWriter persists analytics to SQLite for historical
// playback enrichment.
type AnalyticsPipeline struct {
	store  *AnalyticsStore
	hub    *AnalyticsHub
	writer PersistenceWriter // nil if persistence not configured
}

// NewAnalyticsPipeline creates a pipeline backed by store and hub.
func NewAnalyticsPipeline(
	store *AnalyticsStore,
	hub *AnalyticsHub,
	opts ...PipelineOption,
) *AnalyticsPipeline {
	p := &AnalyticsPipeline{store: store, hub: hub}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

// PipelineOption configures optional AnalyticsPipeline dependencies.
type PipelineOption func(*AnalyticsPipeline)

// WithPersistenceWriter enables analytics persistence to SQLite for
// analytics-enriched recorded playback.
func WithPersistenceWriter(w PersistenceWriter) PipelineOption {
	return func(p *AnalyticsPipeline) { p.writer = w }
}

// Handle is the grpc.AnalyticsHandler callback. It stores analytics by
// wall-clock, broadcasts to hub subscribers, and optionally persists to SQLite.
func (p *AnalyticsPipeline) Handle(
	sourceID string,
	_ int64,
	wallClock time.Time,
	a *av.FrameAnalytics,
) {
	p.store.Put(sourceID, wallClock, a)
	p.hub.Broadcast(sourceID, a)

	if p.writer != nil {
		p.writer.Enqueue(sourceID, wallClock, a)
	}
}
