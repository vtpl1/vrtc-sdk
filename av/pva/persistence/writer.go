package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

const (
	analyticsBufSize = 65536
	flushInterval    = time.Second
	flushBatch       = 500
	pruneInterval    = 10 * time.Minute
)

type frameRecord struct {
	ChannelID string
	WallClock time.Time
	Analytics *av.FrameAnalytics
}

// Writer is an asynchronous, batched writer for persisting FrameAnalytics to
// per-channel SQLite databases. It follows the metrics Store pattern: a buffered
// channel feeds a background goroutine that flushes in batches.
type Writer struct {
	dbm       *DBManager
	incoming  chan frameRecord
	retention time.Duration
	maxRows   int64
	done      chan struct{}
	cancel    context.CancelFunc
}

// NewWriter creates a Writer and starts its background goroutine.
func NewWriter(dbm *DBManager, retention time.Duration, maxRows int64) *Writer {
	ctx, cancel := context.WithCancel(context.Background())

	w := &Writer{
		dbm:       dbm,
		incoming:  make(chan frameRecord, analyticsBufSize),
		retention: retention,
		maxRows:   maxRows,
		done:      make(chan struct{}),
		cancel:    cancel,
	}

	go w.run(ctx)

	return w
}

// Enqueue adds a FrameAnalytics to the write queue (non-blocking, drops on full).
func (w *Writer) Enqueue(channelID string, wallClock time.Time, a *av.FrameAnalytics) {
	select {
	case w.incoming <- frameRecord{ChannelID: channelID, WallClock: wallClock, Analytics: a}:
	default: // drop on full buffer
	}
}

// Close stops the background writer and flushes remaining records.
func (w *Writer) Close() error {
	w.cancel()
	<-w.done

	return nil
}

func (w *Writer) run(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	pruneTicker := time.NewTicker(pruneInterval)
	defer pruneTicker.Stop()

	var batch []frameRecord

	for {
		select {
		case <-ctx.Done():
			w.flush(ctx, batch)

			return
		case rec := <-w.incoming:
			batch = append(batch, rec)
			if len(batch) >= flushBatch {
				w.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-pruneTicker.C:
			w.prune(ctx)
		}
	}
}

func (w *Writer) flush(ctx context.Context, batch []frameRecord) {
	if len(batch) == 0 {
		return
	}

	// Group by channel for per-channel DB transactions.
	grouped := make(map[string][]frameRecord, 8)
	for _, rec := range batch {
		grouped[rec.ChannelID] = append(grouped[rec.ChannelID], rec)
	}

	for channelID, records := range grouped {
		w.flushChannel(ctx, channelID, records)
	}
}

func (w *Writer) flushChannel(ctx context.Context, channelID string, records []frameRecord) {
	db, err := w.dbm.GetDB(ctx, channelID)
	if err != nil {
		return
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return
	}

	frameStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO frames(site_id, channel_id, frame_pts, capture_ms, capture_end_ms,
		 inference_ms, ref_width, ref_height, vehicle_count, people_count, object_count,
		 has_event, ts)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()

		return
	}

	defer frameStmt.Close()

	detStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO detections(frame_id, x, y, w, h, class_id, confidence, track_id, is_event)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()

		return
	}

	defer detStmt.Close()

	for _, rec := range records {
		insertRecord(ctx, frameStmt, detStmt, rec)
	}

	_ = tx.Commit()
}

func insertRecord(ctx context.Context, frameStmt, detStmt *sql.Stmt, rec frameRecord) {
	a := rec.Analytics

	hasEvent := 0

	for _, obj := range a.Objects {
		if obj.IsEvent {
			hasEvent = 1

			break
		}
	}

	res, err := frameStmt.ExecContext(ctx,
		a.SiteID, a.ChannelID, a.FramePTS, a.CaptureMS, a.CaptureEndMS,
		a.InferenceMS, a.RefWidth, a.RefHeight, a.VehicleCount, a.PeopleCount,
		len(a.Objects), hasEvent,
		rec.WallClock.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return
	}

	frameID, err := res.LastInsertId()
	if err != nil {
		return
	}

	for _, obj := range a.Objects {
		isEvent := 0
		if obj.IsEvent {
			isEvent = 1
		}

		_, _ = detStmt.ExecContext(ctx,
			frameID, obj.X, obj.Y, obj.W, obj.H,
			obj.ClassID, obj.Confidence, obj.TrackID, isEvent,
		)
	}
}

func (w *Writer) prune(ctx context.Context) {
	channelIDs := w.dbm.OpenChannelIDs()

	cutoff := time.Now().UTC().Add(-w.retention).Format(time.RFC3339Nano)

	for _, chID := range channelIDs {
		db, err := w.dbm.GetDB(ctx, chID)
		if err != nil {
			continue
		}

		// Delete detections first to maintain referential integrity.
		_, _ = db.ExecContext(ctx,
			"DELETE FROM detections WHERE frame_id IN (SELECT id FROM frames WHERE ts < ?)", cutoff)
		_, _ = db.ExecContext(ctx,
			"DELETE FROM frames WHERE ts < ?", cutoff)

		// Row-count cap.
		if w.maxRows > 0 {
			var count int64

			row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM frames")
			if err := row.Scan(&count); err == nil && count > w.maxRows {
				deleteCount := count - (w.maxRows * 4 / 5) // keep 80%
				_, _ = db.ExecContext(ctx,
					fmt.Sprintf("DELETE FROM detections WHERE frame_id IN "+
						"(SELECT id FROM frames ORDER BY ts ASC LIMIT %d)", deleteCount))
				_, _ = db.ExecContext(ctx,
					fmt.Sprintf("DELETE FROM frames WHERE id IN "+
						"(SELECT id FROM frames ORDER BY ts ASC LIMIT %d)", deleteCount))
			}
		}
	}
}
