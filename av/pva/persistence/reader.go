package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Reader provides query methods for persisted analytics data.
type Reader struct {
	dbm *DBManager
}

// NewReader creates a Reader backed by the given DBManager.
func NewReader(dbm *DBManager) *Reader {
	return &Reader{dbm: dbm}
}

// QueryFrames returns frames within [from, to) for the given channel, with
// optional filtering. Returns (frames, totalCount, error).
func (r *Reader) QueryFrames(
	ctx context.Context,
	channelID string,
	from, to time.Time,
	opts QueryOpts,
) ([]FrameWithDetections, int, error) {
	db, err := r.dbm.GetDB(ctx, channelID)
	if err != nil {
		return nil, 0, err
	}

	countQuery, dataQuery, args := r.buildFrameQuery(from, to, opts)

	// Total count (before pagination).
	var total int
	if err := db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("analytics persistence: count frames: %w", err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	// Paginated frame rows.
	limit, offset := effectiveLimitOffset(opts)
	pagedArgs := append(args, limit, offset) //nolint:gocritic

	rows, err := db.QueryContext(ctx, dataQuery, pagedArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("analytics persistence: query frames: %w", err)
	}
	defer rows.Close()

	frames, err := scanFrames(rows)
	if err != nil {
		return nil, 0, err
	}

	if len(frames) > 0 {
		if err := r.populateDetections(ctx, db, frames); err != nil {
			return nil, 0, err
		}
	}

	return frames, total, nil
}

// CountsByInterval returns aggregated counts bucketed by intervalSec seconds.
func (r *Reader) CountsByInterval(
	ctx context.Context,
	channelID string,
	from, to time.Time,
	intervalSec int,
) ([]CountBucket, error) {
	db, err := r.dbm.GetDB(ctx, channelID)
	if err != nil {
		return nil, err
	}

	if intervalSec <= 0 {
		intervalSec = 60
	}

	fromMS := from.UnixMilli()
	toMS := to.UnixMilli()
	intervalMS := int64(intervalSec) * 1000

	query := fmt.Sprintf(
		`SELECT (capture_ms / %d) * %d AS bucket_ms,
		        COUNT(*)            AS frame_count,
		        SUM(vehicle_count)  AS vehicle_count,
		        SUM(people_count)   AS people_count,
		        SUM(object_count)   AS object_count,
		        SUM(has_event)      AS event_count
		 FROM frames
		 WHERE capture_ms >= ? AND capture_ms < ?
		 GROUP BY bucket_ms
		 ORDER BY bucket_ms ASC`,
		intervalMS, intervalMS,
	)

	rows, err := db.QueryContext(ctx, query, fromMS, toMS)
	if err != nil {
		return nil, fmt.Errorf("analytics persistence: counts by interval: %w", err)
	}
	defer rows.Close()

	var buckets []CountBucket

	for rows.Next() {
		var b CountBucket
		if err := rows.Scan(&b.BucketMS, &b.FrameCount, &b.VehicleCount,
			&b.PeopleCount, &b.ObjectCount, &b.EventCount); err != nil {
			return nil, fmt.Errorf("analytics persistence: scan count bucket: %w", err)
		}

		buckets = append(buckets, b)
	}

	return buckets, rows.Err()
}

// SearchByTrackID returns frames that contain detections with the given trackID.
func (r *Reader) SearchByTrackID(
	ctx context.Context,
	channelID string,
	trackID int64,
	from, to time.Time,
	opts QueryOpts,
) ([]FrameWithDetections, int, error) {
	db, err := r.dbm.GetDB(ctx, channelID)
	if err != nil {
		return nil, 0, err
	}

	fromMS := from.UnixMilli()
	toMS := to.UnixMilli()

	// Count total matching frames.
	countQuery := `SELECT COUNT(DISTINCT f.id) FROM frames f
		JOIN detections d ON d.frame_id = f.id
		WHERE d.track_id = ? AND f.capture_ms >= ? AND f.capture_ms < ?`

	var total int
	if err := db.QueryRowContext(ctx, countQuery, trackID, fromMS, toMS).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("analytics persistence: count track frames: %w", err)
	}

	if total == 0 {
		return nil, 0, nil
	}

	limit, offset := effectiveLimitOffset(opts)

	dataQuery := `SELECT f.id, f.site_id, f.channel_id, f.frame_pts, f.capture_ms,
		f.capture_end_ms, f.inference_ms, f.ref_width, f.ref_height,
		f.vehicle_count, f.people_count, f.object_count, f.has_event, f.ts
		FROM frames f
		JOIN detections d ON d.frame_id = f.id
		WHERE d.track_id = ? AND f.capture_ms >= ? AND f.capture_ms < ?
		GROUP BY f.id
		ORDER BY f.capture_ms ASC
		LIMIT ? OFFSET ?`

	rows, err := db.QueryContext(ctx, dataQuery, trackID, fromMS, toMS, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("analytics persistence: search by track: %w", err)
	}
	defer rows.Close()

	frames, err := scanFrames(rows)
	if err != nil {
		return nil, 0, err
	}

	if len(frames) > 0 {
		if err := r.populateDetections(ctx, db, frames); err != nil {
			return nil, 0, err
		}
	}

	return frames, total, nil
}

// SearchEvents returns frames that have event-flagged detections.
func (r *Reader) SearchEvents(
	ctx context.Context,
	channelID string,
	from, to time.Time,
	opts QueryOpts,
) ([]FrameWithDetections, int, error) {
	opts.EventsOnly = true

	return r.QueryFrames(ctx, channelID, from, to, opts)
}

// buildFrameQuery constructs the count and data SELECT queries with filtering.
func (r *Reader) buildFrameQuery(
	from, to time.Time,
	opts QueryOpts,
) (countQuery, dataQuery string, args []any) {
	fromMS := from.UnixMilli()
	toMS := to.UnixMilli()

	where := "WHERE f.capture_ms >= ? AND f.capture_ms < ?"

	args = append(args, fromMS, toMS)

	needsJoin := false

	if opts.EventsOnly {
		where += " AND f.has_event = 1"
	}

	if opts.ClassID != nil {
		needsJoin = true
		where += " AND d.class_id = ?"

		args = append(args, *opts.ClassID)
	}

	if opts.MinConfidence != nil {
		needsJoin = true
		where += " AND d.confidence >= ?"

		args = append(args, *opts.MinConfidence)
	}

	fromClause := "FROM frames f"
	groupBy := ""

	if needsJoin {
		fromClause = "FROM frames f JOIN detections d ON d.frame_id = f.id"
		groupBy = " GROUP BY f.id"
	}

	countQuery = fmt.Sprintf("SELECT COUNT(DISTINCT f.id) %s %s", fromClause, where)

	dataQuery = fmt.Sprintf(
		`SELECT f.id, f.site_id, f.channel_id, f.frame_pts, f.capture_ms,
		 f.capture_end_ms, f.inference_ms, f.ref_width, f.ref_height,
		 f.vehicle_count, f.people_count, f.object_count, f.has_event, f.ts
		 %s %s%s ORDER BY f.capture_ms ASC LIMIT ? OFFSET ?`,
		fromClause, where, groupBy,
	)

	return countQuery, dataQuery, args
}

// populateDetections batch-loads detections for the given frames.
func (r *Reader) populateDetections(
	ctx context.Context,
	db *sql.DB,
	frames []FrameWithDetections,
) error {
	if len(frames) == 0 {
		return nil
	}

	// Build frame ID list and lookup map.
	ids := make([]any, len(frames))
	idxMap := make(map[int64]int, len(frames))

	placeholders := ""

	var placeholdersSb264 strings.Builder

	for i, f := range frames {
		ids[i] = f.ID
		idxMap[f.ID] = i

		if i > 0 {
			placeholdersSb264.WriteString(",")
		}

		placeholdersSb264.WriteString("?")
	}

	placeholders += placeholdersSb264.String()

	query := fmt.Sprintf(
		`SELECT id, frame_id, x, y, w, h, class_id, confidence, track_id, is_event
		 FROM detections WHERE frame_id IN (%s) ORDER BY frame_id, id`, placeholders)

	rows, err := db.QueryContext(ctx, query, ids...)
	if err != nil {
		return fmt.Errorf("analytics persistence: query detections: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var d StoredDetection

		var isEventInt int

		if err := rows.Scan(&d.ID, &d.FrameID, &d.X, &d.Y, &d.W, &d.H,
			&d.ClassID, &d.Confidence, &d.TrackID, &isEventInt); err != nil {
			return fmt.Errorf("analytics persistence: scan detection: %w", err)
		}

		d.IsEvent = isEventInt != 0

		if idx, ok := idxMap[d.FrameID]; ok {
			frames[idx].Detections = append(frames[idx].Detections, d)
		}
	}

	return rows.Err()
}

func scanFrames(rows *sql.Rows) ([]FrameWithDetections, error) {
	var frames []FrameWithDetections

	for rows.Next() {
		var f FrameWithDetections

		var hasEventInt int

		var tsStr string

		if err := rows.Scan(
			&f.ID, &f.SiteID, &f.ChannelID, &f.FramePTS, &f.CaptureMS,
			&f.CaptureEndMS, &f.InferenceMS, &f.RefWidth, &f.RefHeight,
			&f.VehicleCount, &f.PeopleCount, &f.ObjectCount, &hasEventInt, &tsStr,
		); err != nil {
			return nil, fmt.Errorf("analytics persistence: scan frame: %w", err)
		}

		f.HasEvent = hasEventInt != 0
		f.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		f.Detections = []StoredDetection{} // ensure non-nil JSON array

		frames = append(frames, f)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics persistence: rows iteration: %w", err)
	}

	return frames, nil
}

func effectiveLimitOffset(opts QueryOpts) (int, int) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}

	if limit > 1000 {
		limit = 1000
	}

	offset := max(opts.Offset, 0)

	return limit, offset
}
