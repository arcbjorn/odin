package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newDB builds an in-memory database with the parts of the real schema
// these tests exercise. America/New_York is UTC-3 (IANA inverts the sign) — the same
// zone the real database ships with, and the reason a late-night session can
// land on the wrong UTC day.
func newDB(t *testing.T, tz string) *SQLite {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	schema := `
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE projects (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE);
CREATE TABLE work_sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    started_at   TEXT NOT NULL,
    ended_at     TEXT,
    summary      TEXT,
    day          TEXT GENERATED ALWAYS AS (date(started_at)) VIRTUAL
);
CREATE TABLE daily_reviews (
    date TEXT PRIMARY KEY,
    kept_main_promise TEXT,
    actual_output TEXT,
    day_score INTEGER
);
CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TEXT NOT NULL,
    type TEXT NOT NULL,
    note TEXT NOT NULL
);
INSERT INTO projects(name) VALUES ('odin');
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES ('timezone', ?)`, tz); err != nil {
		t.Fatalf("seed timezone: %v", err)
	}

	s, err := NewSQLite(db)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	return s
}

func callTool(t *testing.T, h func(context.Context, json.RawMessage) (string, error), sqlText string) (string, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"sql": sqlText})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return h(context.Background(), raw)
}

func TestRefusesDBWithoutTimezone(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// Guessing a zone would silently file sessions on the wrong day.
	if _, err := NewSQLite(db); err == nil {
		t.Fatal("expected refusal when the database has no timezone")
	}
}

func TestTodayUsesDBTimezoneNotUTC(t *testing.T) {
	s := newDB(t, "America/New_York") // UTC-3

	wantLocal := time.Now().In(s.Location()).Format("2006-01-02")
	if got := s.Today(); got != wantLocal {
		t.Fatalf("Today() = %q, want local %q", got, wantLocal)
	}

	// The bug this guards: between 00:00 and 03:00 UTC, the UTC date is
	// already tomorrow while it is still yesterday locally.
	utcDate := time.Now().UTC().Format("2006-01-02")
	if s.Today() != utcDate {
		t.Logf("local %s differs from UTC %s — exactly the window that misfiles sessions",
			s.Today(), utcDate)
	}
}

// The highest-value guard: date('now') is UTC and files a 23:51 session under
// tomorrow.
func TestBlocksUTCNowFunctions(t *testing.T) {
	s := newDB(t, "America/New_York")

	blocked := []string{
		`SELECT * FROM work_sessions WHERE day = date('now')`,
		`SELECT * FROM work_sessions WHERE started_at > datetime('now')`,
		`SELECT * FROM daily_reviews WHERE date = CURRENT_DATE`,
		`SELECT * FROM work_sessions WHERE started_at > CURRENT_TIMESTAMP`,
		`SELECT * FROM work_sessions WHERE day = DATE("now")`,
	}
	for _, stmt := range blocked {
		if _, err := callTool(t, s.handleQuery, stmt); err == nil {
			t.Errorf("expected refusal for %q", stmt)
		}
	}

	// Writes must be guarded too — that is where a wrong date persists.
	insert := `INSERT INTO events(timestamp, type, note) VALUES (datetime('now'), 'idea', 'x')`
	if _, err := callTool(t, s.handleExec, insert); err == nil {
		t.Error("expected refusal for datetime('now') in a write")
	}
}

func TestTodayParameterResolvesToLocalDate(t *testing.T) {
	s := newDB(t, "America/New_York")

	ins := `INSERT INTO daily_reviews(date, actual_output, day_score) VALUES (:today, 'shipped odin', 8)`
	if _, err := callTool(t, s.handleExec, ins); err != nil {
		t.Fatalf("insert: %v", err)
	}

	out, err := callTool(t, s.handleQuery, `SELECT date, day_score FROM daily_reviews WHERE date = :today`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, s.Today()) {
		t.Fatalf("expected local date %s in output:\n%s", s.Today(), out)
	}
	if !strings.Contains(out, "8") {
		t.Fatalf("expected score 8 in output:\n%s", out)
	}
}

func TestQueryRejectsWrites(t *testing.T) {
	s := newDB(t, "UTC")
	writes := []string{
		`INSERT INTO events(timestamp,type,note) VALUES ('2026-07-20 10:00','idea','x')`,
		`UPDATE daily_reviews SET day_score = 10`,
		`DELETE FROM events`,
	}
	for _, stmt := range writes {
		if _, err := callTool(t, s.handleQuery, stmt); err == nil {
			t.Errorf("query tool accepted a write: %q", stmt)
		}
	}
}

func TestExecRejectsDestructiveVerbs(t *testing.T) {
	s := newDB(t, "UTC")
	for _, stmt := range []string{
		`DELETE FROM events WHERE id = 1`,
		`DROP TABLE events`,
		`ALTER TABLE events ADD COLUMN x TEXT`,
	} {
		if _, err := callTool(t, s.handleExec, stmt); err == nil {
			t.Errorf("exec accepted destructive statement: %q", stmt)
		}
	}
}

func TestRejectsStackedStatements(t *testing.T) {
	s := newDB(t, "UTC")
	stmt := `INSERT INTO events(timestamp,type,note) VALUES ('2026-07-20 10:00','idea','a'); DROP TABLE events`
	if _, err := callTool(t, s.handleExec, stmt); err == nil {
		t.Fatal("expected stacked statements to be rejected")
	}
}

// An UPDATE matching nothing usually means the WHERE targeted the wrong day.
// Reporting success there is how a wrong date rots silently in the log.
func TestZeroRowsAffectedIsAnError(t *testing.T) {
	s := newDB(t, "UTC")
	stmt := `UPDATE daily_reviews SET day_score = 9 WHERE date = '1999-01-01'`
	_, err := callTool(t, s.handleExec, stmt)
	if err == nil {
		t.Fatal("expected an error when no rows matched")
	}
	if !strings.Contains(err.Error(), "0 rows") {
		t.Fatalf("error should name the cause, got: %v", err)
	}
}

// He trains once per day per activity, so a wide UPDATE means a missing WHERE.
func TestWideUpdateIsRejected(t *testing.T) {
	s := newDB(t, "UTC")
	for _, d := range []string{"2026-07-10", "2026-07-11", "2026-07-12", "2026-07-13", "2026-07-14", "2026-07-15"} {
		if _, err := callTool(t, s.handleExec,
			`INSERT INTO daily_reviews(date, day_score) VALUES ('`+d+`', 5)`); err != nil {
			t.Fatalf("seed %s: %v", d, err)
		}
	}

	_, err := callTool(t, s.handleExec, `UPDATE daily_reviews SET day_score = 10`)
	if err == nil {
		t.Fatal("expected a WHERE-less UPDATE across 6 rows to be rejected")
	}
	if !strings.Contains(err.Error(), "WHERE") {
		t.Fatalf("error should name the missing WHERE, got: %v", err)
	}
}

// A session crossing midnight belongs to the day it STARTED. He trains late on
// purpose; that is normal, not an error to round away.
func TestSessionCrossingMidnightKeepsStartDay(t *testing.T) {
	s := newDB(t, "America/New_York")

	ins := `INSERT INTO work_sessions(project_id, started_at, ended_at, summary)
	        VALUES (1, '2026-07-19 23:51', '2026-07-20 00:46', 'late session')`
	if _, err := callTool(t, s.handleExec, ins); err != nil {
		t.Fatalf("insert: %v", err)
	}

	out, err := callTool(t, s.handleQuery, `SELECT day, started_at, ended_at FROM work_sessions`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "2026-07-19") {
		t.Fatalf("session should be filed under its start day:\n%s", out)
	}
}

// NULL must render distinctly from an empty string: an unanswered field is
// accurate and must never be mistaken for a value or filled in with a guess.
func TestNullRendersDistinctly(t *testing.T) {
	s := newDB(t, "UTC")
	if _, err := callTool(t, s.handleExec,
		`INSERT INTO daily_reviews(date, actual_output) VALUES ('2026-07-20', 'shipped')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	out, err := callTool(t, s.handleQuery,
		`SELECT actual_output, kept_main_promise FROM daily_reviews WHERE date = '2026-07-20'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "NULL") {
		t.Fatalf("expected NULL to be visible:\n%s", out)
	}
}

func TestEmptyResultIsExplicit(t *testing.T) {
	s := newDB(t, "UTC")
	out, err := callTool(t, s.handleQuery, `SELECT * FROM events WHERE type = 'nothing'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != "(no rows)" {
		t.Fatalf("empty result must be explicit, got %q", out)
	}
}

func TestResultsAreTruncated(t *testing.T) {
	s := newDB(t, "UTC")
	for i := 0; i < maxRows+50; i++ {
		if _, err := callTool(t, s.handleExec,
			`INSERT INTO events(timestamp,type,note) VALUES ('2026-07-20 10:00','idea','n')`); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	out, err := callTool(t, s.handleQuery, `SELECT * FROM events`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("expected large result sets to be truncated")
	}
}

// The tools must satisfy the registry's schema-size limit — that limit exists
// because oversized schemas caused the 162-call loop.
func TestToolSchemasAreSmall(t *testing.T) {
	s := newDB(t, "UTC")
	for _, tool := range []struct {
		name  string
		props int
	}{
		{"query", len(s.QueryTool().Def.Schema["properties"].(map[string]any))},
		{"exec", len(s.ExecTool().Def.Schema["properties"].(map[string]any))},
	} {
		if tool.props > 6 {
			t.Errorf("%s has %d properties; keep tool schemas small", tool.name, tool.props)
		}
	}
}
