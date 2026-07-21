package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newWeb(t *testing.T, readerURL string) *Web {
	t.Helper()
	return NewWeb(WebConfig{ReaderURL: readerURL})
}

// The model supplies these URLs, so a naive fetcher is an SSRF primitive:
// file:// reads local files and a private address reaches cloud metadata
// endpoints and host-only services.
func TestFetchRejectsDangerousURLs(t *testing.T) {
	w := newWeb(t, "https://reader.invalid/")

	blocked := []string{
		"file:///etc/passwd",
		"http://localhost/admin",
		"http://127.0.0.1:8080/",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.1/internal",
		"http://192.168.1.1/router",
		"http://172.16.0.1/",
		"http://[::1]:8080/",
		"http://0.0.0.0/",
		"http://db.internal/dump",
		"ftp://example.com/file",
		"gopher://example.com/",
		"",
	}
	for _, target := range blocked {
		// Check validateURL directly rather than going through handleFetch:
		// a blocked address would fail to connect anyway, so asserting only
		// "some error occurred" would pass even with the guard removed. The
		// point is that we refuse *before* making any request.
		if _, err := validateURL(target); err == nil {
			t.Errorf("expected %q to be refused by validateURL", target)
		}
	}

	// And the guard must actually be wired into the fetch path.
	raw, _ := json.Marshal(map[string]string{"url": "http://169.254.169.254/latest/meta-data/"})
	_, err := w.handleFetch(context.Background(), raw)
	if err == nil {
		t.Fatal("cloud metadata address reached the fetch path")
	}
	if !strings.Contains(err.Error(), "private or loopback") {
		t.Fatalf("fetch should refuse before requesting, got: %v", err)
	}
}

func TestValidateURLAcceptsPublicHTTP(t *testing.T) {
	for _, target := range []string{
		"https://example.com/article",
		"http://example.com",
		"https://sub.example.co.uk/path?q=1#frag",
	} {
		if _, err := validateURL(target); err != nil {
			t.Errorf("%q should be allowed: %v", target, err)
		}
	}
}

func TestFetchViaReader(t *testing.T) {
	var gotPath atomic.Value
	reader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.String())
		io.WriteString(w, "# Article\n\nThe body of the article.")
	}))
	defer reader.Close()

	w := newWeb(t, reader.URL)
	raw, _ := json.Marshal(map[string]string{"url": "https://example.com/post"})

	out, err := w.handleFetch(context.Background(), raw)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(out, "The body of the article.") {
		t.Fatalf("got %q", out)
	}
	// The target URL is appended to the reader prefix.
	if path, _ := gotPath.Load().(string); !strings.Contains(path, "example.com/post") {
		t.Fatalf("reader received %q", path)
	}
}

// A third-party outage must degrade the output, not remove the capability.
func TestFallsBackToDirectFetchWhenReaderFails(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><style>p{color:red}</style></head>
			<body><script>track()</script><h1>Real Title</h1>
			<p>Real body text.</p></body></html>`)
	}))
	defer origin.Close()

	reader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer reader.Close()

	w := NewWeb(WebConfig{ReaderURL: reader.URL})
	// The direct path must reach a test server on loopback, so bypass the
	// public-URL guard that would otherwise (correctly) block it.
	out, err := w.fetchDirect(context.Background(), origin.URL)
	if err != nil {
		t.Fatalf("direct fetch: %v", err)
	}
	if !strings.Contains(out, "Real Title") || !strings.Contains(out, "Real body text.") {
		t.Fatalf("content lost: %q", out)
	}
	// Script and style contents must not reach the model.
	if strings.Contains(out, "track()") || strings.Contains(out, "color:red") {
		t.Fatalf("script/style leaked into output: %q", out)
	}
}

func TestBothPathsFailingReportsBoth(t *testing.T) {
	reader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer reader.Close()

	w := NewWeb(WebConfig{ReaderURL: reader.URL})
	raw, _ := json.Marshal(map[string]string{"url": "https://nonexistent.invalid/x"})

	_, err := w.handleFetch(context.Background(), raw)
	if err == nil {
		t.Fatal("expected an error when both paths fail")
	}
	if !strings.Contains(err.Error(), "reader failed") {
		t.Fatalf("error should mention both attempts, got: %v", err)
	}
}

func TestNonTextContentIsRejected(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer origin.Close()

	w := NewWeb(WebConfig{})
	if _, err := w.fetchDirect(context.Background(), origin.URL); err == nil {
		t.Fatal("expected non-text content to be refused")
	}
}

func TestLargePageIsTruncated(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := 0; i < 20000; i++ {
			io.WriteString(w, "This is a line of an extremely long article.\n")
		}
	}))
	defer origin.Close()

	w := NewWeb(WebConfig{})
	out, err := w.fetchDirect(context.Background(), origin.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(out) > maxFetchBytes+64 {
		t.Fatalf("output is %d bytes, over the cap", len(out))
	}
	if !strings.Contains(out, "[truncated]") {
		t.Fatal("truncation should be marked so the model knows the page is partial")
	}
}

// An empty page must be reported, not summarized into an invention.
func TestEmptyPageIsExplicit(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body><script>x()</script></body></html>")
	}))
	defer origin.Close()

	w := NewWeb(WebConfig{})
	out, err := w.fetchDirect(context.Background(), origin.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(out, "no readable text") {
		t.Fatalf("expected an explicit empty-page message, got %q", out)
	}
}

// A capability that always errors is worse than one that is absent: the model
// would keep trying it.
func TestSearchToolAbsentWithoutBackend(t *testing.T) {
	w := NewWeb(WebConfig{})
	for _, tool := range w.Tools() {
		if tool.Def.Name == "search_web" {
			t.Fatal("search_web offered with no backend configured")
		}
	}
	if len(w.Tools()) != 1 {
		t.Fatalf("expected only fetch_url, got %d tools", len(w.Tools()))
	}
}

type stubSearcher struct {
	results []SearchResult
	gotLim  int
}

func (s *stubSearcher) Search(_ context.Context, _ string, limit int) ([]SearchResult, error) {
	s.gotLim = limit
	if limit < len(s.results) {
		return s.results[:limit], nil
	}
	return s.results, nil
}

func TestSearchToolAppearsWithBackend(t *testing.T) {
	stub := &stubSearcher{results: []SearchResult{
		{Title: "Result one", URL: "https://example.com/1", Snippet: "First snippet."},
		{Title: "Result two", URL: "https://example.com/2"},
	}}
	w := NewWeb(WebConfig{Searcher: stub})

	var found bool
	for _, tool := range w.Tools() {
		if tool.Def.Name == "search_web" {
			found = true
		}
	}
	if !found {
		t.Fatal("search_web missing despite a configured backend")
	}

	raw, _ := json.Marshal(map[string]any{"query": "database indexing"})
	out, err := w.handleSearch(context.Background(), raw)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, want := range []string{"Result one", "https://example.com/1", "First snippet."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if stub.gotLim != 5 {
		t.Fatalf("default limit = %d, want 5", stub.gotLim)
	}
}

func TestSearchLimitIsBounded(t *testing.T) {
	stub := &stubSearcher{}
	w := NewWeb(WebConfig{Searcher: stub})

	raw, _ := json.Marshal(map[string]any{"query": "x", "limit": 500})
	if _, err := w.handleSearch(context.Background(), raw); err != nil {
		t.Fatalf("search: %v", err)
	}
	if stub.gotLim > 10 {
		t.Fatalf("limit %d exceeds the cap", stub.gotLim)
	}
}

func TestEmptySearchResultsAreExplicit(t *testing.T) {
	w := NewWeb(WebConfig{Searcher: &stubSearcher{}})
	raw, _ := json.Marshal(map[string]any{"query": "nothing"})

	out, err := w.handleSearch(context.Background(), raw)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out != "(no results)" {
		t.Fatalf("got %q", out)
	}
}

func TestWebToolSchemasAreSmall(t *testing.T) {
	w := NewWeb(WebConfig{Searcher: &stubSearcher{}})
	for _, tool := range w.Tools() {
		props, _ := tool.Def.Schema["properties"].(map[string]any)
		if len(props) > 6 {
			t.Errorf("%s has %d properties; keep tool schemas small", tool.Def.Name, len(props))
		}
	}
}

func TestSearXNGParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format=json not requested, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[
			{"title":"Indexing guide","url":"https://example.com/a","content":"Index design techniques."},
			{"title":"No url","url":"","content":"skipped"},
			{"title":"Second","url":"https://example.com/b","content":"More."}
		]}`)
	}))
	defer srv.Close()

	s, err := NewSearXNG(SearXNGConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewSearXNG: %v", err)
	}

	results, err := s.Search(context.Background(), "database indexing", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// The entry without a URL is unusable and must be dropped.
	if len(results) != 2 {
		t.Fatalf("expected 2 usable results, got %d", len(results))
	}
	if results[0].Title != "Indexing guide" || results[0].URL != "https://example.com/a" {
		t.Fatalf("first result = %+v", results[0])
	}
}

// SearXNG serves HTML unless the JSON format is enabled; the error must name
// that fix rather than dumping a parse failure.
func TestSearXNGNamesTheJSONFormatFix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>results</body></html>")
	}))
	defer srv.Close()

	s, _ := NewSearXNG(SearXNGConfig{BaseURL: srv.URL})
	_, err := s.Search(context.Background(), "x", 5)
	if err == nil {
		t.Fatal("expected an error for an HTML response")
	}
	if !strings.Contains(err.Error(), "settings.yml") {
		t.Fatalf("error should name the fix, got: %v", err)
	}
}

func TestSearXNGRespectsLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[
			{"title":"1","url":"https://e.com/1"},{"title":"2","url":"https://e.com/2"},
			{"title":"3","url":"https://e.com/3"},{"title":"4","url":"https://e.com/4"}
		]}`)
	}))
	defer srv.Close()

	s, _ := NewSearXNG(SearXNGConfig{BaseURL: srv.URL})
	results, err := s.Search(context.Background(), "x", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("limit not applied: got %d results", len(results))
	}
}

func TestSearXNGRequiresBaseURL(t *testing.T) {
	if _, err := NewSearXNG(SearXNGConfig{}); err == nil {
		t.Fatal("expected a missing base url to be refused")
	}
	if _, err := NewSearXNG(SearXNGConfig{BaseURL: "not a url"}); err == nil {
		t.Fatal("expected an invalid base url to be refused")
	}
}

// Ten hits of full page text would cost more context than fetching the one
// page that matters.
func TestSnippetsAreTruncated(t *testing.T) {
	long := strings.Repeat("word ", 200)
	got := truncateSnippet(long)
	if len(got) > 210 {
		t.Fatalf("snippet is %d chars, too long", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncation not marked: %q", got)
	}
}
