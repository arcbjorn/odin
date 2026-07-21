package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SearXNG is a Searcher backed by a self-hosted SearXNG instance.
//
// Chosen over metered search APIs because the economics match the rest of
// Odin: unlimited queries against 70+ engines on a box you already run, with
// no per-query billing. Tavily at $0.008/query and Exa at $0.001/result are
// fine services, but they reintroduce exactly the metered spend this agent
// was built to avoid.
//
// Not wired into any profile yet. None of the scheduled jobs search, so this
// exists as a ready implementation rather than a speculative dependency —
// set search_url in config.toml to switch it on.
//
// SearXNG serves HTML only by default; the JSON output format must be enabled
// in its settings.yml (`search.formats: [html, json]`), otherwise every query
// here returns a parse error.
type SearXNG struct {
	baseURL string
	http    *http.Client
}

// SearXNGConfig configures the backend.
type SearXNGConfig struct {
	// BaseURL is the instance root, e.g. http://127.0.0.1:8080.
	BaseURL string
	Timeout time.Duration
}

// NewSearXNG builds a SearXNG-backed searcher.
func NewSearXNG(cfg SearXNGConfig) (*SearXNG, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("searxng base url is required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid searxng url %q", cfg.BaseURL)
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	return &SearXNG{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}, nil
}

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search runs one query.
func (s *SearXNG) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	params := url.Values{
		"q":      {query},
		"format": {"json"},
	}

	endpoint := s.baseURL + "/search?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng returned http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var parsed searxResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// The overwhelmingly common cause is JSON output not being enabled,
		// so name the fix instead of dumping a parse error.
		return nil, fmt.Errorf("searxng did not return json; enable the json format in its settings.yml")
	}

	out := make([]SearchResult, 0, limit)
	for _, r := range parsed.Results {
		if len(out) >= limit {
			break
		}
		if r.URL == "" {
			continue
		}
		out = append(out, SearchResult{
			Title:   strings.TrimSpace(r.Title),
			URL:     r.URL,
			Snippet: truncateSnippet(r.Content),
		})
	}
	return out, nil
}

// truncateSnippet keeps result listings cheap: ten hits of full page text
// would cost more context than fetching the one page that matters.
func truncateSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 200
	if len(s) <= max {
		return s
	}
	if cut := strings.LastIndex(s[:max], " "); cut > max/2 {
		return s[:cut] + "..."
	}
	return s[:max] + "..."
}
