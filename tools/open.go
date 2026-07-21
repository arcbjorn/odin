package tools

import (
	"database/sql"
	"fmt"

	// Pure Go keeps cross-compilation free of cgo requirements.
	_ "modernc.org/sqlite"
)

// OpenTracker opens a profile's SQLite database.
func OpenTracker(path string) (*sql.DB, error) {
	// _time_format=sqlite keeps DATETIME round-trips in the textual
	// 'YYYY-MM-DD HH:MM' shape the schema already stores. foreign_keys=on
	// matches the tracker's own PRAGMA so cascades behave as designed.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open tracker %s: %w", path, err)
	}

	// A single writer avoids SQLITE_BUSY between the scheduler and gateway.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping tracker %s: %w", path, err)
	}
	return db, nil
}
