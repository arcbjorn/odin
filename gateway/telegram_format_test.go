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

func TestHardBreaks(t *testing.T) {
	// Consecutive prose lines get hard breaks so the renderer keeps them apart.
	if got := hardBreaks("a\nb\nc"); got != "a  \nb  \nc" {
		t.Fatalf("prose: got %q", got)
	}
	// A blank-line paragraph break is preserved as-is.
	if got := hardBreaks("a\n\nb"); got != "a\n\nb" {
		t.Fatalf("paragraph: got %q", got)
	}
	if got := hardBreaks("single"); got != "single" {
		t.Fatalf("single line: got %q", got)
	}
}

func TestRichMarkdownLeavesTableNewlinesAlone(t *testing.T) {
	in := "Line one\nLine two\n\n| Day | Hours |\n|---|--:|\n| Tue | 8.0 |\n| Wed | 9.0 |"
	got := richMarkdown(in)

	// Prose lines get hard breaks.
	if !strings.Contains(got, "Line one  \nLine two") {
		t.Fatalf("prose not hard-broken:\n%s", got)
	}
	// Table rows keep single newlines (no "  \n" injected inside the table).
	if strings.Contains(got, "8.0  \n") || strings.Contains(got, "|  \n") {
		t.Fatalf("hard break injected into table:\n%s", got)
	}
	// The bare "---" column is made explicit-left; the right column stays right.
	if !strings.Contains(got, "|:---|--:|") {
		t.Fatalf("delimiter alignment not made explicit:\n%s", got)
	}
}

func TestRichMarkdownProtectsCodeFences(t *testing.T) {
	in := "before\n```\ncode line 1\ncode line 2\n```\nafter"
	got := richMarkdown(in)
	// Newlines inside the fence must not become hard breaks.
	if strings.Contains(got, "code line 1  \n") {
		t.Fatalf("hard break injected into code fence:\n%s", got)
	}
	if !strings.Contains(got, "```\ncode line 1\ncode line 2\n```") {
		t.Fatalf("code fence altered:\n%s", got)
	}
}

func TestMarkDelimiterAlignment(t *testing.T) {
	cases := map[string]string{
		"|---|---|":     "|:---|:---|", // both bare → both explicit-left
		"|:--|--:|":     "|:--|--:|",   // left + right preserved
		"| --- | :-: |": "|:---|:-:|",  // spaces trimmed, center preserved
		"---|---:":      ":---|---:",   // no outer pipes
	}
	for in, want := range cases {
		if got := markDelimiter(in); got != want {
			t.Fatalf("markDelimiter(%q) = %q, want %q", in, got, want)
		}
	}
}

// sendChunk uses sendRichMessage with raw markdown (no parse_mode), and the
// message id is tracked for /new.
func TestSendUsesRichMessage(t *testing.T) {
	var mu sync.Mutex
	var method, richMsg string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		method = r.URL.Path
		richMsg = r.FormValue("rich_message")
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{"message_id":7}}`)
	}))
	defer srv.Close()

	g := newRawGateway(t, srv)
	g.send(context.Background(), 1, "**hi** there")

	mu.Lock()
	defer mu.Unlock()
	if !strings.HasSuffix(method, "/sendRichMessage") {
		t.Fatalf("expected sendRichMessage, got %q", method)
	}
	// Raw markdown is passed through untouched inside the JSON payload.
	if !strings.Contains(richMsg, `**hi** there`) {
		t.Fatalf("raw markdown not forwarded: %q", richMsg)
	}
}

// When the endpoint is missing (older server), it latches off and falls back
// to plain sendMessage for this and every later send.
func TestSendFallsBackWhenRichUnavailable(t *testing.T) {
	var mu sync.Mutex
	var calls []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/sendRichMessage") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer srv.Close()

	g := newRawGateway(t, srv)
	g.send(context.Background(), 1, "first")
	g.send(context.Background(), 1, "second")

	mu.Lock()
	defer mu.Unlock()
	// first send: rich (404) then plain. second send: plain only (latched off).
	rich, plain := 0, 0
	for _, c := range calls {
		switch {
		case strings.HasSuffix(c, "/sendRichMessage"):
			rich++
		case strings.HasSuffix(c, "/sendMessage"):
			plain++
		}
	}
	if rich != 1 {
		t.Fatalf("rich should be tried once then latched off, got %d", rich)
	}
	if plain != 2 {
		t.Fatalf("both messages should reach plain sendMessage, got %d", plain)
	}
}

// newRawGateway builds a gateway pointed at srv with no command-menu noise.
func newRawGateway(t *testing.T, srv *httptest.Server) *Telegram {
	t.Helper()
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
	return g
}
