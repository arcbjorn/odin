// Package tools implements Odin's tool handlers.
package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
)

// maxRows caps result size. A runaway SELECT would otherwise blow the context
// window and cost real money on every retry.
const maxRows = 200

// SQLite exposes a profile database to the model as two tools: `query` for
// reads and `exec` for writes.
//
// Split deliberately. The model reaches for reads constantly and writes rarely,
// and the write path carries rules that reads don't (never touch another day's
// row, always read back). Two small tools with 2 fields each are also far
// easier for a weaker model to fill than one tool with a mode flag.
type SQLite struct {
	db *sql.DB
	// tz is the user's local zone, read from the database's settings table.
	// The server runs UTC; date('now') in SQL is UTC and lands on the wrong
	// day for anything logged late at night. Every date this package computes
	// goes through tz instead.
	tz *time.Location
}

// NewSQLite opens the database and loads its timezone.
//
// The timezone lives in the DB rather than config because it is switchable
// live when travelling — the agent updates one row and "today" moves with it.
func NewSQLite(db *sql.DB) (*SQLite, error) {
	s := &SQLite{db: db, tz: time.UTC}

	var name string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = 'timezone'`).Scan(&name)
	switch {
	case err == sql.ErrNoRows:
		return nil, fmt.Errorf("database has no timezone in settings; refusing to guess")
	case err != nil:
		return nil, fmt.Errorf("read timezone: %w", err)
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q in settings: %w", name, err)
	}
	s.tz = loc
	return s, nil
}

// Now returns the current time in the database's local zone.
func (s *SQLite) Now() time.Time { return time.Now().In(s.tz) }

// Today returns the local date as YYYY-MM-DD. Never derive this from the
// server clock or SQL date('now') — both are UTC.
func (s *SQLite) Today() string { return s.Now().Format("2006-01-02") }

// Location exposes the database's zone for the scheduler, which must fire jobs
// on local wall-clock time.
func (s *SQLite) Location() *time.Location { return s.tz }

type queryInput struct {
	SQL  string `json:"sql"`
	Note string `json:"note,omitempty"`
}

type execInput struct {
	SQL  string `json:"sql"`
	Note string `json:"note,omitempty"`
}

// QueryTool returns the read-only tool.
func (s *SQLite) QueryTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name: "query",
			Description: "Run a read-only SQL SELECT against the profile database. " +
				"Today's local date is available as :today. " +
				"Never use date('now') or datetime('now') — the server is UTC and they land on the wrong day.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql":  map[string]any{"type": "string", "description": "A single SELECT statement."},
					"note": map[string]any{"type": "string", "description": "Optional: what you are looking for."},
				},
				"required": []string{"sql"},
			},
		},
		Handle: s.handleQuery,
	}
}

// ExecTool returns the write tool.
func (s *SQLite) ExecTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name: "exec",
			Description: "Run a single INSERT or UPDATE against the profile database. " +
				"Today's local date is available as :today. Store times as LOCAL times. " +
				"A session's day is the date it STARTED, even if it crossed midnight. " +
				"Never modify a row on a different date — that is a different session.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql":  map[string]any{"type": "string", "description": "A single INSERT or UPDATE statement."},
					"note": map[string]any{"type": "string", "description": "Optional: what you are recording."},
				},
				"required": []string{"sql"},
			},
		},
		Handle: s.handleExec,
	}
}

func (s *SQLite) handleQuery(ctx context.Context, raw json.RawMessage) (string, error) {
	var in queryInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	stmt := strings.TrimSpace(in.SQL)
	if stmt == "" {
		return "", fmt.Errorf("sql is required")
	}
	if err := checkSingleStatement(stmt); err != nil {
		return "", err
	}
	if !isReadOnly(stmt) {
		return "", fmt.Errorf("query is read-only; use the exec tool to write")
	}
	if err := checkNoUTCNow(stmt); err != nil {
		return "", err
	}

	rows, err := s.db.QueryContext(ctx, stmt, sql.Named("today", s.Today()))
	if err != nil {
		return "", fmt.Errorf("sql error: %w", err)
	}
	defer rows.Close()

	return formatRows(rows)
}

func (s *SQLite) handleExec(ctx context.Context, raw json.RawMessage) (string, error) {
	var in execInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	stmt := strings.TrimSpace(in.SQL)
	if stmt == "" {
		return "", fmt.Errorf("sql is required")
	}
	if err := checkSingleStatement(stmt); err != nil {
		return "", err
	}
	if err := checkWriteVerb(stmt); err != nil {
		return "", err
	}
	if err := checkNoUTCNow(stmt); err != nil {
		return "", err
	}

	res, err := s.db.ExecContext(ctx, stmt, sql.Named("today", s.Today()))
	if err != nil {
		return "", fmt.Errorf("sql error: %w", err)
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		// Silence here is a bug, not a success. An UPDATE matching nothing
		// usually means the WHERE targeted the wrong day.
		return "", fmt.Errorf("statement affected 0 rows — check the WHERE clause targets the right date")
	}
	if affected > 5 {
		// One session, one row. A wide UPDATE means a missing WHERE.
		return "", fmt.Errorf("statement affected %d rows, which looks like a missing WHERE clause; "+
			"no rollback was performed — verify the data", affected)
	}

	out := fmt.Sprintf("OK: %d row(s) affected.", affected)
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		out += fmt.Sprintf(" Inserted id %d.", id)
	}
	// The skill requires reading the row back and stating what was saved, so a
	// wrong date is visible immediately instead of rotting in the log.
	out += " Now SELECT the row back and state the day and times you actually saved."
	return out, nil
}

// formatRows renders a result set as compact TSV. Cheaper in tokens than JSON
// and easier for a model to read back accurately.
func formatRows(rows *sql.Rows) (string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(strings.Join(cols, "\t"))
	b.WriteByte('\n')

	scan := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range scan {
		ptrs[i] = &scan[i]
	}

	n := 0
	truncated := false
	for rows.Next() {
		if n >= maxRows {
			truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		cells := make([]string, len(cols))
		for i, v := range scan {
			cells[i] = renderCell(v)
		}
		b.WriteString(strings.Join(cells, "\t"))
		b.WriteByte('\n')
		n++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	if n == 0 {
		// Explicit, because an empty field is accurate and must not be
		// mistaken for "the query failed" or filled in with a guess.
		return "(no rows)", nil
	}
	if truncated {
		fmt.Fprintf(&b, "(truncated at %d rows; add LIMIT or narrow the query)", maxRows)
	}
	return b.String(), nil
}

func renderCell(v any) string {
	switch t := v.(type) {
	case nil:
		// NULL is meaningful in this schema: an unanswered field stays empty
		// rather than being assumed. Render it distinctly from "".
		return "NULL"
	case []byte:
		return strings.ReplaceAll(string(t), "\t", " ")
	case string:
		return strings.ReplaceAll(t, "\t", " ")
	default:
		return strings.ReplaceAll(fmt.Sprint(t), "\t", " ")
	}
}

// checkSingleStatement rejects stacked statements. Not a sandbox — the model
// is trusted — but it keeps one tool call to one intelligible action.
func checkSingleStatement(stmt string) error {
	trimmed := strings.TrimRight(stmt, "; \t\n")
	if strings.Contains(trimmed, ";") {
		return fmt.Errorf("send one statement per call")
	}
	return nil
}

func isReadOnly(stmt string) bool {
	head := firstWord(stmt)
	return head == "select" || head == "with"
}

func checkWriteVerb(stmt string) error {
	switch firstWord(stmt) {
	case "insert", "update":
		return nil
	case "delete", "drop", "alter", "truncate":
		// The database is an append-mostly log. Losing history silently is
		// worse than any convenience deletion buys.
		return fmt.Errorf("%s is not permitted on the database", strings.ToUpper(firstWord(stmt)))
	default:
		return fmt.Errorf("exec accepts INSERT or UPDATE only")
	}
}

// checkNoUTCNow blocks SQL that derives dates from the server clock.
//
// This is the single highest-value guard in the file. The server runs UTC and
// the user trains late at night, so date('now') silently files a 23:51 session
// under tomorrow. The model is told this in the tool description; this check
// makes it enforceable rather than advisory.
func checkNoUTCNow(stmt string) error {
	lower := strings.ToLower(stmt)
	for _, bad := range []string{"date('now')", "datetime('now')", "date(\"now\")", "datetime(\"now\")", "current_date", "current_timestamp"} {
		if strings.Contains(lower, bad) {
			return fmt.Errorf("%s is UTC and lands on the wrong local day; use :today or an explicit local time", bad)
		}
	}
	return nil
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t\n(")
	if i < 0 {
		return strings.ToLower(s)
	}
	return strings.ToLower(s[:i])
}
