package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	_ "modernc.org/sqlite" // sqlite driver
)

const (
	sampleBufSize   = 32768
	snapshotBufSize = 64
	flushInterval   = time.Second
	flushBatch      = 100
	pruneInterval   = 10 * time.Minute
)

type sample struct {
	Name      string
	Value     float64
	Labels    string
	Timestamp time.Time
}

// Store is the central metrics collector backed by SQLite.
// All Record* methods are non-blocking: they push samples to a buffered
// channel. A background goroutine batches them into SQLite.
type Store struct {
	db        *sql.DB
	samples   chan sample
	snapshots chan Snapshot
	retention time.Duration
	maxRows   int64
	done      chan struct{}
	cancel    context.CancelFunc
}

// New opens or creates the SQLite DB at dbPath and starts the background writer.
func New(dbPath string, retention time.Duration, maxRows int64) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("metrics: open db: %w", err)
	}

	if err := initSchema(context.Background(), db); err != nil {
		db.Close()

		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Store{
		db:        db,
		samples:   make(chan sample, sampleBufSize),
		snapshots: make(chan Snapshot, snapshotBufSize),
		retention: retention,
		maxRows:   maxRows,
		done:      make(chan struct{}),
		cancel:    cancel,
	}

	go s.run(ctx)

	return s, nil
}

// RecordLatency records a latency sample (non-blocking, drops on full buffer).
func (s *Store) RecordLatency(name string, d time.Duration, labels map[string]string) {
	s.emit(name, float64(d.Milliseconds()), labels)
}

// RecordCounter records a counter value (non-blocking, drops on full buffer).
func (s *Store) RecordCounter(name string, value float64, labels map[string]string) {
	s.emit(name, value, labels)
}

// RecordSnapshot stores a periodic system snapshot (non-blocking).
func (s *Store) RecordSnapshot(snap Snapshot) {
	select {
	case s.snapshots <- snap:
	default:
	}
}

// Query returns aggregated metrics for the JSON API.
func (s *Store) Query(ctx context.Context, opts QueryOpts) (*MetricsResponse, error) {
	since := opts.Since
	if since <= 0 {
		since = time.Hour
	}

	cutoff := time.Now().UTC().Add(-since).Format(time.RFC3339Nano)

	latencies, err := s.queryLatencies(ctx, cutoff)
	if err != nil {
		return nil, err
	}

	snapshots, err := s.querySnapshots(ctx, cutoff)
	if err != nil {
		return nil, err
	}

	return &MetricsResponse{
		GeneratedAt: time.Now().UTC(),
		Latencies:   latencies,
		Snapshots:   snapshots,
	}, nil
}

// Close stops the background writer and closes the DB.
func (s *Store) Close() error {
	s.cancel()
	<-s.done

	return s.db.Close()
}

func (s *Store) emit(name string, value float64, labels map[string]string) {
	lblJSON := "{}"

	if len(labels) > 0 {
		if b, err := json.Marshal(labels); err == nil {
			lblJSON = string(b)
		}
	}

	select {
	case s.samples <- sample{
		Name:      name,
		Value:     value,
		Labels:    lblJSON,
		Timestamp: time.Now().UTC(),
	}:
	default: // drop on full buffer
	}
}

func (s *Store) run(ctx context.Context) {
	defer close(s.done)

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	pruneTicker := time.NewTicker(pruneInterval)
	defer pruneTicker.Stop()

	var batch []sample

	for {
		select {
		case <-ctx.Done():
			s.flush(ctx, batch)

			return
		case sm := <-s.samples:
			batch = append(batch, sm)
			if len(batch) >= flushBatch {
				s.flush(ctx, batch)
				batch = batch[:0]
			}
		case snap := <-s.snapshots:
			s.writeSnapshot(ctx, snap)
		case <-ticker.C:
			if len(batch) > 0 {
				s.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-pruneTicker.C:
			s.prune(ctx)
		}
	}
}

func (s *Store) flush(ctx context.Context, batch []sample) {
	if len(batch) == 0 {
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}

	stmt, err := tx.PrepareContext(
		ctx,
		"INSERT INTO samples(name, value, labels, ts) VALUES(?, ?, ?, ?)",
	)
	if err != nil {
		_ = tx.Rollback()

		return
	}

	defer stmt.Close()

	for _, sm := range batch {
		_, _ = stmt.ExecContext(
			ctx,
			sm.Name,
			sm.Value,
			sm.Labels,
			sm.Timestamp.Format(time.RFC3339Nano),
		)
	}

	_ = tx.Commit()
}

func (s *Store) writeSnapshot(ctx context.Context, snap Snapshot) {
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO snapshots(ts, goroutines, heap_alloc_mb, active_relays, active_viewers,
		 active_segments, total_packets, total_dropped, avg_fps, total_bitrate)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.Timestamp.Format(time.RFC3339Nano),
		snap.Goroutines, snap.HeapAllocMB, snap.ActiveRelays, snap.ActiveViewers,
		snap.ActiveSegments, snap.TotalPackets, snap.TotalDropped, snap.AvgFPS, snap.TotalBitrate,
	)
}

func (s *Store) queryLatencies(ctx context.Context, cutoff string) (map[string]Histogram, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, value FROM samples WHERE ts >= ? ORDER BY name, value", cutoff)
	if err != nil {
		return nil, fmt.Errorf("metrics: query latencies: %w", err)
	}
	defer rows.Close()

	grouped := make(map[string][]float64)

	for rows.Next() {
		var name string

		var value float64

		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("metrics: scan sample: %w", err)
		}

		grouped[name] = append(grouped[name], value)
	}

	result := make(map[string]Histogram, len(grouped))

	for name, values := range grouped {
		result[name] = computeHistogram(values)
	}

	return result, rows.Err()
}

func (s *Store) querySnapshots(ctx context.Context, cutoff string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, goroutines, heap_alloc_mb, active_relays, active_viewers,
		 active_segments, total_packets, total_dropped, avg_fps, total_bitrate
		 FROM snapshots WHERE ts >= ? ORDER BY ts`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("metrics: query snapshots: %w", err)
	}
	defer rows.Close()

	var result []Snapshot

	for rows.Next() {
		var snap Snapshot

		var tsStr string

		if err := rows.Scan(&tsStr, &snap.Goroutines, &snap.HeapAllocMB, &snap.ActiveRelays,
			&snap.ActiveViewers, &snap.ActiveSegments, &snap.TotalPackets, &snap.TotalDropped,
			&snap.AvgFPS, &snap.TotalBitrate); err != nil {
			return nil, fmt.Errorf("metrics: scan snapshot: %w", err)
		}

		snap.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		result = append(result, snap)
	}

	return result, rows.Err()
}

func computeHistogram(values []float64) Histogram {
	n := len(values)
	if n == 0 {
		return Histogram{}
	}

	sort.Float64s(values)

	var sum float64

	for _, v := range values {
		sum += v
	}

	return Histogram{
		Count: n,
		P50:   percentile(values, 0.50),
		P95:   percentile(values, 0.95),
		P99:   percentile(values, 0.99),
		Max:   values[n-1],
		Avg:   sum / float64(n),
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}

	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))

	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}

	frac := idx - float64(lower)

	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
