package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestFormatMarkdownV2(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain period escaped", "Task saved.", `Task saved\.`},
		{"bold to single asterisk", "**7 h total** today.", `*7 h total* today\.`},
		{"italic to underscore", "that was *fast* today", `that was _fast_ today`},
		{"header to bold", "# Morning brief", `*Morning brief*`},
		{"header strips inner bold", "## **Important**", `*Important*`},
		{"snake_case not italicized", "col created_at is fine", `col created\_at is fine`},
		{"inline code preserved", "run `odin status` now", "run `odin status` now"},
		{"special chars escaped", "1-2 (a) {b} = c!", `1\-2 \(a\) \{b\} \= c\!`},
		{"link", "see [docs](https://x.io/a)", `see [docs](https://x.io/a)`},
		{"bullet dash escaped", "- item one", `\- item one`},
		{"inline code inside bold", "**use `x`**", "*use `x`*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatMarkdownV2(c.in); got != c.want {
				t.Fatalf("formatMarkdownV2(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

func TestFormatTablesToCodeBlock(t *testing.T) {
	in := "Work log\n\n" +
		"| Day | Project | Hours |\n" +
		"|-----|---------|------:|\n" +
		"| Wed 8 | New Reach | 3.0 |\n" +
		"| Fri 10 | New Reach | 5.5 |\n\n" +
		"14-day total: 74.0 h"

	got := formatTables(in)

	// The table is wrapped in a code fence, and the raw delimiter row is gone.
	if !strings.Contains(got, "```") {
		t.Fatalf("table not wrapped in a code block:\n%s", got)
	}
	if strings.Contains(got, "|---") || strings.Contains(got, "|------:|") {
		t.Fatalf("delimiter row survived:\n%s", got)
	}
	// Columns are padded to a consistent width so they align in monospace.
	if !strings.Contains(got, "Day     Project    Hours") {
		t.Fatalf("header not aligned as expected:\n%s", got)
	}
	// Right-aligned Hours column (delimiter ended with a colon): 3.0 padded left.
	if !strings.Contains(got, "New Reach    3.0") {
		t.Fatalf("hours column not right-aligned:\n%s", got)
	}
	// Prose around the table is untouched.
	if !strings.Contains(got, "Work log") || !strings.Contains(got, "14-day total: 74.0 h") {
		t.Fatalf("prose corrupted:\n%s", got)
	}
}

// End to end: a table run through the full formatter stays a fenced code block
// (Telegram renders it monospace) with no leftover delimiter row.
func TestFormatMarkdownV2RendersTable(t *testing.T) {
	in := "| A | B |\n|---|---|\n| x | y |"
	got := formatMarkdownV2(in)
	if !strings.Contains(got, "```") {
		t.Fatalf("table not a code block: %q", got)
	}
	if strings.Contains(got, "|---") {
		t.Fatalf("delimiter row survived: %q", got)
	}
}

func TestFormatMarkdownV2Empty(t *testing.T) {
	if got := formatMarkdownV2(""); got != "" {
		t.Fatalf("empty input should stay empty, got %q", got)
	}
}

func TestStripMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold** text", "bold text"},
		{"# Heading", "Heading"},
		{"see [docs](http://x)", "see docs"},
		{"run `cmd` now", "run cmd now"},
		{`already escaped\.`, "already escaped."},
	}
	for _, c := range cases {
		if got := stripMarkdown(c.in); got != c.want {
			t.Fatalf("stripMarkdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsParseError(t *testing.T) {
	yes := []string{
		"telegram sendMessage failed: Bad Request: can't parse entities: unexpected end",
		"telegram sendMessage failed: can't parse entities",
	}
	no := []string{
		"telegram sendMessage failed: Too Many Requests",
		"connection reset",
	}
	for _, s := range yes {
		if !isParseError(errString(s)) {
			t.Fatalf("expected parse error for %q", s)
		}
	}
	for _, s := range no {
		if isParseError(errString(s)) {
			t.Fatalf("did not expect parse error for %q", s)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// When Telegram rejects the MarkdownV2 entities, the chunk must still be
// delivered as clean plain text rather than dropped.
func TestSendFallsBackToPlainOnParseError(t *testing.T) {
	var mu sync.Mutex
	var got []struct{ parseMode, text string }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		pm := r.FormValue("parse_mode")
		mu.Lock()
		got = append(got, struct{ parseMode, text string }{pm, r.FormValue("text")})
		mu.Unlock()
		if pm == "MarkdownV2" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"ok":false,"description":"Bad Request: can't parse entities: bad"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	g, err := NewTelegram(TelegramConfig{
		Token:        "test-token",
		AllowedUsers: []int64{1},
		Agent:        &fakeAgent{reply: "ok"},
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	g.http = srv.Client()
	g.baseURL = srv.URL

	g.send(context.Background(), 1, "**hi** there_friend.")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("want 2 sends (rich then plain), got %d: %+v", len(got), got)
	}
	if got[0].parseMode != "MarkdownV2" {
		t.Fatalf("first send should be MarkdownV2, got %q", got[0].parseMode)
	}
	if got[1].parseMode != "" {
		t.Fatalf("fallback send must drop parse_mode, got %q", got[1].parseMode)
	}
	if strings.Contains(got[1].text, "**") || strings.Contains(got[1].text, `\`) {
		t.Fatalf("fallback text should be clean plain, got %q", got[1].text)
	}
}
