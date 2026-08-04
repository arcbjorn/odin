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

// newDB builds an in-memory domain database and supplies its runtime timezone
// independently, as production profile assembly does.
func newDB(t *testing.T, tz string) *SQLite {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	schema := `
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

	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("timezone: %v", err)
	}
	s, err := NewSQLite(db, SQLiteConfig{Location: loc, MaxAffectedRows: 5})
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

func TestRequiresRuntimeLocation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := NewSQLite(db, SQLiteConfig{}); err == nil {
		t.Fatal("expected refusal without a runtime location")
	}
}

func TestTodayUsesRuntimeTimezoneNotUTC(t *testing.T) {
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

// Date semantics belong to the profile schema and skills. The generic database
// tool supplies :today but does not reject legitimate UTC expressions.
func TestAllowsProfileChosenDateExpressions(t *testing.T) {
	s := newDB(t, "America/New_York")
	if _, err := callTool(t, s.handleQuery, `SELECT date('now')`); err != nil {
		t.Fatalf("generic query rejected UTC expression: %v", err)
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

// :now is the local wall clock in the form the schema stores. Without it a
// statement that needs the current time has only datetime('now'), which is UTC
// — the wrong hour always, and the wrong day either side of midnight.
func TestNowParameterResolvesToLocalWallClock(t *testing.T) {
	s := newDB(t, "America/New_York")

	before := s.Clock()
	if _, err := callTool(t, s.handleExec,
		`INSERT INTO work_sessions(project_id, started_at) VALUES (1, :now)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	after := s.Clock()

	out, err := callTool(t, s.handleQuery, `SELECT started_at FROM work_sessions`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// A minute can turn over mid-test; either side of it is correct.
	if !strings.Contains(out, before) && !strings.Contains(out, after) {
		t.Fatalf("started_at is not the local clock (%s):\n%s", before, out)
	}
	if utc := time.Now().UTC().Format("2006-01-02 15:04"); strings.Contains(out, utc) {
		t.Fatalf("started_at is the server's UTC clock (%s), not the profile's:\n%s", utc, out)
	}
}

// The reported failure: "65 min cardio, finished 10 min ago" is a complete
// session once there is a clock to subtract from, and the subtraction can be
// pushed into SQL rather than done from a guessed time.
func TestNowSupportsRelativeArithmetic(t *testing.T) {
	s := newDB(t, "America/New_York")

	ins := `INSERT INTO work_sessions(project_id, started_at, ended_at)
	        VALUES (1, datetime(:now, '-75 minutes'), datetime(:now, '-10 minutes'))`
	if _, err := callTool(t, s.handleExec, ins); err != nil {
		t.Fatalf("insert: %v", err)
	}

	out, err := callTool(t, s.handleQuery,
		`SELECT cast((julianday(ended_at) - julianday(started_at)) * 1440 + 0.5 AS integer) FROM work_sessions`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "65") {
		t.Fatalf("expected a 65 minute session:\n%s", out)
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

func TestQueryRejectsMutatingCTE(t *testing.T) {
	s := newDB(t, "UTC")
	stmt := `WITH item(timestamp,type,note) AS (VALUES ('2026-07-20 10:00','idea','x'))
		INSERT INTO events(timestamp,type,note) SELECT timestamp, type, note FROM item`
	if _, err := callTool(t, s.handleQuery, stmt); err == nil {
		t.Fatal("query tool accepted a mutating CTE")
	}
	out, err := callTool(t, s.handleQuery, `SELECT count(*) AS count FROM events`)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if !strings.Contains(out, "\n0\n") {
		t.Fatalf("mutating CTE changed the database:\n%s", out)
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

func TestZeroRowsAffectedIsReportedWithoutFailure(t *testing.T) {
	s := newDB(t, "UTC")
	stmt := `UPDATE daily_reviews SET day_score = 9 WHERE date = '1999-01-01'`
	out, err := callTool(t, s.handleExec, stmt)
	if err != nil {
		t.Fatalf("zero-row update: %v", err)
	}
	if !strings.Contains(out, "0 row") {
		t.Fatalf("result should state that no rows matched, got: %s", out)
	}
}

func TestWideUpdateIsRolledBack(t *testing.T) {
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
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error should confirm rollback, got: %v", err)
	}

	out, err := callTool(t, s.handleQuery, `SELECT count(*) FROM daily_reviews WHERE day_score = 10`)
	if err != nil {
		t.Fatalf("verify rollback: %v", err)
	}
	if !strings.Contains(out, "\n0\n") {
		t.Fatalf("wide update was not rolled back:\n%s", out)
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
