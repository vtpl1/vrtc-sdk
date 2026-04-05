package persistence

import (
	"context"
	"database/sql"
	"fmt"
)

const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA synchronous=NORMAL;

CREATE TABLE IF NOT EXISTS frames (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    site_id        INTEGER NOT NULL,
    channel_id     INTEGER NOT NULL,
    frame_pts      INTEGER NOT NULL,
    capture_ms     INTEGER NOT NULL,
    capture_end_ms INTEGER NOT NULL DEFAULT 0,
    inference_ms   INTEGER NOT NULL DEFAULT 0,
    ref_width      INTEGER NOT NULL DEFAULT 0,
    ref_height     INTEGER NOT NULL DEFAULT 0,
    vehicle_count  INTEGER NOT NULL DEFAULT 0,
    people_count   INTEGER NOT NULL DEFAULT 0,
    object_count   INTEGER NOT NULL DEFAULT 0,
    has_event      INTEGER NOT NULL DEFAULT 0,
    ts             TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_frames_ts           ON frames(ts);
CREATE INDEX IF NOT EXISTS idx_frames_capture_ms   ON frames(capture_ms);
CREATE INDEX IF NOT EXISTS idx_frames_has_event_ts ON frames(has_event, ts);

CREATE TABLE IF NOT EXISTS detections (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    frame_id   INTEGER NOT NULL REFERENCES frames(id),
    x          INTEGER NOT NULL,
    y          INTEGER NOT NULL,
    w          INTEGER NOT NULL,
    h          INTEGER NOT NULL,
    class_id   INTEGER NOT NULL,
    confidence INTEGER NOT NULL,
    track_id   INTEGER NOT NULL DEFAULT 0,
    is_event   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_det_frame_id ON detections(frame_id);
CREATE INDEX IF NOT EXISTS idx_det_class_id ON detections(class_id, frame_id);
CREATE INDEX IF NOT EXISTS idx_det_track_id ON detections(track_id);
`

func initSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("analytics persistence: init schema: %w", err)
	}

	return nil
}
