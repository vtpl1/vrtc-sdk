package metrics

import (
	"context"
	"database/sql"
	"fmt"
)

const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA synchronous=NORMAL;

CREATE TABLE IF NOT EXISTS samples (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    name   TEXT    NOT NULL,
    value  REAL    NOT NULL,
    labels TEXT    DEFAULT '{}',
    ts     TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_name_ts ON samples(name, ts);
CREATE INDEX IF NOT EXISTS idx_samples_ts      ON samples(ts);

CREATE TABLE IF NOT EXISTS snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              TEXT    NOT NULL,
    goroutines      INTEGER,
    heap_alloc_mb   REAL,
    active_relays   INTEGER,
    active_viewers  INTEGER,
    active_segments INTEGER,
    total_packets   INTEGER,
    total_dropped   INTEGER,
    avg_fps         REAL,
    total_bitrate   REAL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_ts ON snapshots(ts);
`

func initSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("metrics: init schema: %w", err)
	}

	return nil
}
