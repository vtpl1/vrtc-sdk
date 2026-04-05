package metrics

import (
	"context"
	"time"
)

// prune enforces retention and max-row limits on both tables.
func (s *Store) prune(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-s.retention).Format(time.RFC3339Nano)

	// Time-based retention.
	_, _ = s.db.ExecContext(ctx, "DELETE FROM samples WHERE ts < ?", cutoff)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM snapshots WHERE ts < ?", cutoff)

	// Row-count cap on samples using efficient row ID range delete.
	if s.maxRows > 0 {
		var count int64

		row := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM samples")
		if err := row.Scan(&count); err == nil && count > s.maxRows {
			deleteCount := count - (s.maxRows * 4 / 5) // keep 80%
			_, _ = s.db.ExecContext(ctx,
				"DELETE FROM samples WHERE id IN (SELECT id FROM samples ORDER BY ts ASC LIMIT ?)",
				deleteCount,
			)
		}
	}

	// Reclaim disk space. VACUUM works with WAL mode (incremental_vacuum does not).
	_, _ = s.db.ExecContext(ctx, "VACUUM")
}
