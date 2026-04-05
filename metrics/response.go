package metrics

import "time"

// MetricsResponse is the JSON payload for GET /api/metrics.
type MetricsResponse struct {
	GeneratedAt time.Time            `json:"generatedAt"`
	Uptime      string               `json:"uptime"`
	Latencies   map[string]Histogram `json:"latencies"`
	Counters    map[string]float64   `json:"counters"`
	Snapshots   []Snapshot           `json:"snapshots"`
	Relays      []RelayMetrics       `json:"relays"`
}

// Histogram holds pre-computed percentiles for a latency metric.
type Histogram struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
	Avg   float64 `json:"avg"`
}

// Snapshot is a periodic system-level reading.
type Snapshot struct {
	Timestamp      time.Time `json:"timestamp"`
	Goroutines     int       `json:"goroutines"`
	HeapAllocMB    float64   `json:"heapAllocMb"`
	ActiveRelays   int       `json:"activeRelays"`
	ActiveViewers  int       `json:"activeViewers"`
	ActiveSegments int       `json:"activeSegments"`
	TotalPackets   uint64    `json:"totalPackets"`
	TotalDropped   uint64    `json:"totalDropped"`
	AvgFPS         float64   `json:"avgFps"`
	TotalBitrate   float64   `json:"totalBitrateBps"`
}

// RelayMetrics holds derived KPIs per relay.
type RelayMetrics struct {
	SourceID      string  `json:"sourceId"`
	FrameLossRate float64 `json:"frameLossRate"`
	ActualFPS     float64 `json:"actualFps"`
	BitrateBps    float64 `json:"bitrateBps"`
	ConsumerCount int     `json:"consumerCount"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

// QueryOpts controls what metrics are returned.
type QueryOpts struct {
	Since time.Duration // default: 1h
}
