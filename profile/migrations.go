package profile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const migrationsTable = `_odin_migrations`

// Migration is one immutable, profile-owned schema change.
type Migration struct {
	Version  int64
	Name     string
	Path     string
	SQL      string
	Checksum string
}

// LoadMigrations validates and orders migrations without touching a database.
func LoadMigrations(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var migrations []Migration
	seen := make(map[int64]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect migration %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("migration %s must be a regular file", entry.Name())
		}
		versionText, label, ok := strings.Cut(strings.TrimSuffix(entry.Name(), ".sql"), "-")
		if !ok || versionText == "" || label == "" {
			return nil, fmt.Errorf("migration %q must be named <version>-<name>.sql", entry.Name())
		}
		version, err := strconv.ParseInt(versionText, 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q has an invalid positive version", entry.Name())
		}
		if previous, exists := seen[version]; exists {
			return nil, fmt.Errorf("migration version %d is duplicated by %s and %s", version, previous, entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		body := strings.TrimSpace(string(raw))
		if body == "" {
			return nil, fmt.Errorf("migration %s is empty", entry.Name())
		}
		hash := sha256.Sum256(raw)
		migrations = append(migrations, Migration{
			Version: version, Name: entry.Name(), Path: path, SQL: body,
			Checksum: hex.EncodeToString(hash[:]),
		})
		seen[version] = entry.Name()
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// ApplyMigrations applies every pending migration in its own transaction and
// rejects edits to a migration that was already recorded.
func ApplyMigrations(ctx context.Context, db *sql.DB, dir string) (int, error) {
	migrations, err := LoadMigrations(dir)
	if err != nil {
		return 0, err
	}
	if len(migrations) == 0 {
		if err := CheckMigrations(ctx, db, migrations, dir); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+migrationsTable+` (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return 0, fmt.Errorf("create migration ledger: %w", err)
	}
	applied, err := readAppliedMigrations(ctx, db)
	if err != nil {
		return 0, err
	}
	if err := checkMigrationSet(migrations, applied, dir); err != nil {
		return 0, err
	}

	count := 0
	for _, migration := range migrations {
		if _, ok := applied[migration.Version]; ok {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return count, fmt.Errorf("migration %s: begin: %w", migration.Name, err)
		}
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			tx.Rollback()
			return count, fmt.Errorf("migration %s: %w", migration.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+migrationsTable+`(version, name, checksum) VALUES (?, ?, ?)`,
			migration.Version, migration.Name, migration.Checksum); err != nil {
			tx.Rollback()
			return count, fmt.Errorf("record migration %s: %w", migration.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return count, fmt.Errorf("migration %s: commit: %w", migration.Name, err)
		}
		count++
	}
	return count, nil
}

// CheckMigrations compares immutable migration files with an existing ledger
// without applying anything. A database with no ledger is valid and simply has
// all migrations pending.
func CheckMigrations(ctx context.Context, db *sql.DB, migrations []Migration, dir string) error {
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, migrationsTable).Scan(&exists); err != nil {
		return fmt.Errorf("inspect migration ledger: %w", err)
	}
	if exists == 0 {
		return nil
	}
	applied, err := readAppliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	return checkMigrationSet(migrations, applied, dir)
}

func checkMigrationSet(migrations []Migration, applied map[int64]string, dir string) error {
	available := make(map[int64]Migration, len(migrations))
	for _, migration := range migrations {
		available[migration.Version] = migration
		if checksum, ok := applied[migration.Version]; ok && checksum != migration.Checksum {
			return fmt.Errorf("migration %s changed after it was applied", migration.Name)
		}
	}
	for version := range applied {
		if _, ok := available[version]; !ok {
			return fmt.Errorf("applied migration version %d is missing from %s", version, dir)
		}
	}
	return nil
}

func readAppliedMigrations(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM `+migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]string)
	for rows.Next() {
		var version int64
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, err
		}
		out[version] = checksum
	}
	return out, rows.Err()
}
