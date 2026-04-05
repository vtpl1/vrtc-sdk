package metrics_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/metrics"
)

func newTestStore(t *testing.T) *metrics.Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "metrics.db")

	s, err := metrics.New(dbPath, 24*time.Hour, 10000)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestStore_RecordAndQuery(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	for range 10 {
		s.RecordLatency("test_latency", 50*time.Millisecond, nil)
	}

	s.RecordLatency("test_latency", 200*time.Millisecond, nil)

	// Wait for flush.
	time.Sleep(2 * time.Second)

	resp, err := s.Query(context.Background(), metrics.QueryOpts{Since: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	h, ok := resp.Latencies["test_latency"]
	if !ok {
		t.Fatal("expected test_latency in response")
	}

	if h.Count != 11 {
		t.Errorf("expected 11 samples, got %d", h.Count)
	}

	if h.P50 < 40 || h.P50 > 60 {
		t.Errorf("expected P50 ~50ms, got %.1f", h.P50)
	}

	if h.Max < 190 {
		t.Errorf("expected Max >= 190ms, got %.1f", h.Max)
	}
}

func TestStore_Snapshot(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	s.RecordSnapshot(metrics.Snapshot{
		Timestamp:    time.Now().UTC(),
		Goroutines:   42,
		HeapAllocMB:  100.5,
		ActiveRelays: 3,
	})

	time.Sleep(2 * time.Second)

	resp, err := s.Query(context.Background(), metrics.QueryOpts{Since: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.Snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(resp.Snapshots))
	}

	if resp.Snapshots[0].Goroutines != 42 {
		t.Errorf("expected 42 goroutines, got %d", resp.Snapshots[0].Goroutines)
	}
}

func TestStore_QuerySinceFilters(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	s.RecordLatency("recent_metric", 10*time.Millisecond, nil)

	time.Sleep(2 * time.Second)

	// Query with a very short window — should return nothing since
	// the sample was recorded ~2s ago.
	resp, err := s.Query(context.Background(), metrics.QueryOpts{Since: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	if h, ok := resp.Latencies["recent_metric"]; ok && h.Count > 0 {
		t.Errorf("expected no samples in 1ms window, got %d", h.Count)
	}

	// Query with a 1h window — should find the sample.
	resp, err = s.Query(context.Background(), metrics.QueryOpts{Since: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if h, ok := resp.Latencies["recent_metric"]; !ok || h.Count == 0 {
		t.Error("expected sample in 1h window")
	}
}
