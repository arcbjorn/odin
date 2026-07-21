package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
)

const (
	// maxFetchBytes caps a single page. Long articles get truncated rather
	// than blowing the context window and being re-sent every turn after.
	maxFetchBytes = 200 << 10

	// defaultReaderURL turns any URL into LLM-readable markdown by prefixing
	// it. Free without a key at ~20 req/min; a free key raises that.
	//
	// Deliberately a third-party dependency: JS rendering, boilerplate
	// stripping, and markdown conversion are someone else's problem. The
	// direct-HTTP fallback below covers the reader being down or rate-limiting.
	defaultReaderURL = "https://r.jina.ai/"
)

// Web gives the model read-only access to web pages.
//
// Scoped to what this agent actually does: the user pastes a link and asks for
// a summary. That is one endpoint — fetch a URL, return text. There is no
// crawler, no sitemap walker, no batch job, because nothing here needs one.
//
// Search is intentionally absent. None of the scheduled jobs search, so
// building it now would be speculative. The Searcher hook below is where a
// self-hosted SearXNG plugs in when there is a real use case for it.
type Web struct {
	http      *http.Client
	readerURL string
	readerKey string

	// searcher is nil until a search backend is configured.
	searcher Searcher
}

// Searcher is a pluggable search backend.
//
// The intended implementation is self-hosted SearXNG: unlimited queries
// against 70+ engines on your own box, with no per-query metering. Metered
// search APIs are a poor fit for an agent whose whole point is running on
// plans rather than per-token billing.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// SearchResult is one hit.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// WebConfig configures web access.
type WebConfig struct {
	// ReaderURL is the markdown-reader prefix. Swappable so a self-hosted
	// reader can replace the public one without touching code.
	ReaderURL string
	// ReaderKeyEnv names an env var holding an optional reader API key,
	// which raises the rate limit. The key is never in config.
	ReaderKey string
	// Searcher enables the search tool when non-nil.
	Searcher Searcher
	Timeout  time.Duration
}

// NewWeb builds the web toolset.
func NewWeb(cfg WebConfig) *Web {
	reader := cfg.ReaderURL
	if reader == "" {
		reader = defaultReaderURL
	}
	if !strings.HasSuffix(reader, "/") {
		reader += "/"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 45 * time.Second
	}

	return &Web{
		readerURL: reader,
		readerKey: cfg.ReaderKey,
		searcher:  cfg.Searcher,
		http: &http.Client{
			Timeout: timeout,
			// Cap redirects: a redirect loop would otherwise burn the whole
			// request timeout.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

// Tools returns the web tools available. Search only appears when a backend
// is configured — an absent capability is better than one that always errors.
func (w *Web) Tools() []agent.Tool {
	tools := []agent.Tool{w.fetchTool()}
	if w.searcher != nil {
		tools = append(tools, w.searchTool())
	}
	return tools
}

func (w *Web) fetchTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "fetch_url",
			Description: "Fetch a web page and return its readable text. Use for links the user shares.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "Full URL including https://"},
				},
				"required": []string{"url"},
			},
		},
		Handle: w.handleFetch,
	}
}

func (w *Web) searchTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "search_web",
			Description: "Search the web and return titles, URLs, and snippets.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query."},
					"limit": map[string]any{"type": "integer", "description": "Max results, default 5."},
				},
				"required": []string{"query"},
			},
		},
		Handle: w.handleSearch,
	}
}

type fetchInput struct {
	URL string `json:"url"`
}

type searchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

func (w *Web) handleFetch(ctx context.Context, raw json.RawMessage) (string, error) {
	var in fetchInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	target, err := validateURL(in.URL)
	if err != nil {
		return "", err
	}

	// Reader first: it renders JS and strips boilerplate. On failure fall
	// back to fetching directly, so a third-party outage degrades the output
	// rather than removing the capability.
	text, err := w.fetchViaReader(ctx, target)
	if err == nil {
		return text, nil
	}
	direct, directErr := w.fetchDirect(ctx, target)
	if directErr != nil {
		return "", fmt.Errorf("reader failed (%v) and direct fetch failed: %w", err, directErr)
	}
	return direct, nil
}

func (w *Web) fetchViaReader(ctx context.Context, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.readerURL+target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	if w.readerKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.readerKey)
	}

	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 429 is the common one on the keyless tier; naming it tells the
		// operator to set a key rather than debug the page.
		return "", fmt.Errorf("reader returned http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return "", err
	}
	return finalize(string(body)), nil
}

// fetchDirect is the fallback: fetch the page and strip tags crudely. Worse
// output than the reader, but it keeps working when the reader does not.
func (w *Web) fetchDirect(ctx context.Context, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	// Some sites serve a bot page to an empty user agent.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; odin/1.0)")
	req.Header.Set("Accept", "text/html,text/plain")

	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !isTextual(ct) {
		return "", fmt.Errorf("unsupported content type %s", ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return "", err
	}
	return finalize(stripHTML(string(body))), nil
}

func (w *Web) handleSearch(ctx context.Context, raw json.RawMessage) (string, error) {
	if w.searcher == nil {
		return "", fmt.Errorf("search is not configured")
	}
	var in searchInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := in.Limit
	if limit <= 0 || limit > 10 {
		limit = 5
	}

	results, err := w.searcher.Search(ctx, query, limit)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "(no results)", nil
	}

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// validateURL rejects anything that is not a public http(s) URL.
//
// The model supplies these, and a naive fetcher is a server-side request
// forgery primitive: file:// reads local files, and a private-range address
// reaches cloud metadata endpoints and services on the host network that are
// only reachable from inside.
func validateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("only http and https urls are supported")
	}
	if u.Host == "" {
		return "", fmt.Errorf("url has no host")
	}

	host := u.Hostname()
	if isPrivateHost(host) {
		return "", fmt.Errorf("refusing to fetch a private or loopback address")
	}
	return u.String(), nil
}

// isPrivateHost reports whether a host is loopback, link-local, or private.
//
// Best-effort by design: it blocks literal IPs and obvious names, but a
// hostname resolving to a private address at connect time still gets through.
// Full protection needs a dialer-level check; this covers the realistic case
// of a model being talked into fetching http://169.254.169.254/ by page text.
func isPrivateHost(host string) bool {
	lower := strings.ToLower(strings.Trim(host, "[]"))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	ip := net.ParseIP(lower)
	if ip == nil {
		return false // a name; resolution-time check is out of scope here
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func isTextual(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "xml") ||
		strings.Contains(ct, "xhtml")
}

// Go's regexp is RE2, which has no backreferences, so each container tag gets
// its own explicit pattern rather than a \1 back-match on the opening tag.
var dropTags = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<script[^>]*>.*?</\s*script\s*>`),
	regexp.MustCompile(`(?is)<style[^>]*>.*?</\s*style\s*>`),
	regexp.MustCompile(`(?is)<noscript[^>]*>.*?</\s*noscript\s*>`),
	regexp.MustCompile(`(?is)<svg[^>]*>.*?</\s*svg\s*>`),
	regexp.MustCompile(`(?is)<head[^>]*>.*?</\s*head\s*>`),
	// An unclosed <script> would otherwise leave the whole tail in place.
	regexp.MustCompile(`(?is)<script[^>]*>.*`),
}

var (
	htmlTag   = regexp.MustCompile(`(?s)<[^>]+>`)
	manyLines = regexp.MustCompile(`\n{3,}`)
	manySpace = regexp.MustCompile(`[ \t]{2,}`)
)

// stripHTML is a crude tag stripper for the fallback path. It is not a parser
// and does not try to be — the reader handles the cases that need one.
func stripHTML(s string) string {
	for _, re := range dropTags {
		s = re.ReplaceAllString(s, " ")
	}
	s = htmlTag.ReplaceAllString(s, " ")
	s = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&mdash;", "—", "&ndash;", "–",
	).Replace(s)
	return s
}

// finalize normalizes whitespace and truncates at a line boundary.
func finalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = manySpace.ReplaceAllString(s, " ")

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	s = strings.TrimSpace(manyLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))

	if len(s) > maxFetchBytes {
		cut := strings.LastIndex(s[:maxFetchBytes], "\n")
		if cut < maxFetchBytes/2 {
			cut = maxFetchBytes
		}
		s = s[:cut] + "\n\n[truncated]"
	}
	if s == "" {
		// Explicit, so the model reports an empty page rather than inventing
		// a summary of nothing.
		return "(page had no readable text)"
	}
	return s
}
