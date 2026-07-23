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
// Split deliberately. The model reaches for reads constantly and writes rarely.
// Two small tools with two fields each are also easier for a weaker model to
// fill than one tool with a mode flag.
type SQLite struct {
	db              *sql.DB
	tz              *time.Location
	maxAffectedRows int64
}

// SQLiteConfig configures the profile database tools.
type SQLiteConfig struct {
	Location        *time.Location
	MaxAffectedRows int64
}

// NewSQLite exposes a domain database using profile runtime settings.
func NewSQLite(db *sql.DB, cfg SQLiteConfig) (*SQLite, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	if cfg.Location == nil {
		return nil, fmt.Errorf("database location is required")
	}
	if cfg.MaxAffectedRows < 0 {
		return nil, fmt.Errorf("max affected rows must not be negative")
	}
	return &SQLite{db: db, tz: cfg.Location, maxAffectedRows: cfg.MaxAffectedRows}, nil
}

// Now returns the current time in the profile's runtime zone.
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
				"Common table expressions are supported. Today's profile-local date is available as :today.",
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
				"Today's profile-local date is available as :today. Destructive and schema-changing statements are not allowed.",
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

	// A leading WITH is not proof of a read: SQLite accepts WITH ... INSERT.
	// Enforce read-only behavior in SQLite on the exact connection executing
	// this call instead of relying only on the lexical check above.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("open read connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		return "", fmt.Errorf("enable read-only query: %w", err)
	}
	defer func() {
		resetCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = conn.ExecContext(resetCtx, `PRAGMA query_only = OFF`)
	}()

	rows, err := conn.QueryContext(ctx, stmt, sql.Named("today", s.Today()))
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin write: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, stmt, sql.Named("today", s.Today()))
	if err != nil {
		return "", fmt.Errorf("sql error: %w", err)
	}

	affected, _ := res.RowsAffected()
	if s.maxAffectedRows > 0 && affected > s.maxAffectedRows {
		return "", fmt.Errorf("statement would affect %d rows, over this profile's limit of %d; rolled back",
			affected, s.maxAffectedRows)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit write: %w", err)
	}

	out := fmt.Sprintf("OK: %d row(s) affected.", affected)
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		out += fmt.Sprintf(" Inserted id %d.", id)
	}
	out += " Verify the stored row when correctness matters."
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

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t\n(")
	if i < 0 {
		return strings.ToLower(s)
	}
	return strings.ToLower(s[:i])
}
