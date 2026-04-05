package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // SQLite driver
)

const (
	maxIdleDBAge      = 10 * time.Minute
	evictionThreshold = 100
)

// DBManager manages per-channel SQLite databases for analytics persistence.
// Databases are opened lazily and evicted when idle, matching the recording
// index pattern in pkg/recorder/index_sqlite.go.
type DBManager struct {
	baseDir    string
	mu         sync.RWMutex
	dbs        map[string]*sql.DB
	lastAccess map[string]time.Time
}

// NewDBManager creates a manager rooted at baseDir. Each channel gets a
// subdirectory containing an analytics.db file.
func NewDBManager(baseDir string) *DBManager {
	return &DBManager{
		baseDir:    baseDir,
		dbs:        make(map[string]*sql.DB),
		lastAccess: make(map[string]time.Time),
	}
}

// GetDB returns the *sql.DB for channelID, opening it lazily. LRU eviction
// of idle databases is triggered when the open count exceeds evictionThreshold.
func (m *DBManager) GetDB(ctx context.Context, channelID string) (*sql.DB, error) {
	m.mu.RLock()
	db, ok := m.dbs[channelID]
	m.mu.RUnlock()

	if ok {
		m.mu.Lock()
		m.lastAccess[channelID] = time.Now()
		m.mu.Unlock()

		return db, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock.
	if db, ok = m.dbs[channelID]; ok {
		m.lastAccess[channelID] = time.Now()

		return db, nil
	}

	if len(m.dbs) >= evictionThreshold {
		m.evictIdleLocked()
	}

	dir := filepath.Join(m.baseDir, channelID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("analytics persistence: mkdir %q: %w", dir, err)
	}

	dsn := filepath.Join(dir, "analytics.db")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("analytics persistence: open %q: %w", dsn, err)
	}

	if err = initSchema(ctx, db); err != nil {
		db.Close()

		return nil, err
	}

	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)

	m.dbs[channelID] = db
	m.lastAccess[channelID] = time.Now()

	return db, nil
}

// OpenChannelIDs returns the IDs of all currently open channel databases.
func (m *DBManager) OpenChannelIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]string, 0, len(m.dbs))
	for id := range m.dbs {
		ids = append(ids, id)
	}

	return ids
}

// Close closes all open channel databases.
func (m *DBManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error

	for ch, db := range m.dbs {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("analytics persistence: close %q: %w", ch, err)
		}

		delete(m.dbs, ch)
	}

	return firstErr
}

func (m *DBManager) evictIdleLocked() {
	cutoff := time.Now().Add(-maxIdleDBAge)

	for ch, lastAt := range m.lastAccess {
		if lastAt.Before(cutoff) {
			if db, ok := m.dbs[ch]; ok {
				_ = db.Close()

				delete(m.dbs, ch)
			}

			delete(m.lastAccess, ch)
		}
	}
}
