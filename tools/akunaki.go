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

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
)

const (
	// maxAkunakiBytes caps one API response. The tools surface serves bounded
	// day views, so anything larger indicates a mistake — truncate rather
	// than blow the context window.
	maxAkunakiBytes = 64 << 10

	akunakiTimeout = 15 * time.Second
)

// Akunaki gives the model read-only access to the akunaki health backend's
// typed tool registry (GET/POST /v1/tools) with a bearer service token.
//
// Deliberately a passthrough, not per-endpoint wrappers: the registry is the
// backend's own agent surface — typed, versioned, and enforced server-side
// (a read-scoped token cannot invoke a mutating tool, the server refuses it)
// — so duplicating its catalog here would only create a second list that
// drifts. Two tools: list the catalog, invoke one entry.
//
// This client never touches the reader or any third party: health data goes
// direct to the backend over TLS, or nowhere.
type Akunaki struct {
	baseURL string
	token   string
	http    *http.Client
}

// AkunakiConfig configures the toolset. Both fields are required: an agent
// with the toolset enabled but no reachable, authenticated backend should
// fail at build, not at the first scheduled job.
type AkunakiConfig struct {
	// BaseURL of the akunaki API, e.g. https://akunaki.example.com.
	BaseURL string
	// Token is the read-scoped service token (resolved from the environment
	// by the profile build; never lives in config.toml).
	Token string
}

// NewAkunaki validates the config and builds the toolset.
func NewAkunaki(cfg AkunakiConfig) (*Akunaki, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("akunaki: base_url is required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("akunaki: base_url must be an absolute http(s) URL")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("akunaki: token is required")
	}
	return &Akunaki{
		baseURL: base,
		token:   strings.TrimSpace(cfg.Token),
		http:    &http.Client{Timeout: akunakiTimeout},
	}, nil
}

// Tools returns the akunaki tools.
func (a *Akunaki) Tools() []agent.Tool {
	return []agent.Tool{a.listTool(), a.invokeTool()}
}

func (a *Akunaki) listTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "health_tools",
			Description: "List the health backend's tools: name, what it needs, whether it reads or mutates.",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		Handle: a.handleList,
	}
}

func (a *Akunaki) invokeTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name:        "health_tool",
			Description: "Invoke one health backend tool by name (see health_tools). Read-only: mutating tools are refused by the server.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Tool name, e.g. health.get_today",
					},
					"input": map[string]any{
						"type":        "object",
						"description": "Tool input, e.g. {\"day\": \"2026-08-15\"}. Omit when the tool needs none.",
					},
				},
				"required": []string{"name"},
			},
		},
		Handle: a.handleInvoke,
	}
}

type akunakiInvokeInput struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (a *Akunaki) handleList(ctx context.Context, _ json.RawMessage) (string, error) {
	return a.call(ctx, http.MethodGet, "/v1/tools", nil)
}

func (a *Akunaki) handleInvoke(ctx context.Context, raw json.RawMessage) (string, error) {
	var in akunakiInvokeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	// The name becomes a path segment; keep it to the registry's dotted
	// identifiers so the request cannot be steered off /v1/tools/.
	if name == "" || strings.ContainsAny(name, "/?#%\\ ") {
		return "", fmt.Errorf("invalid tool name %q", in.Name)
	}

	input := in.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(map[string]json.RawMessage{"input": input})
	if err != nil {
		return "", err
	}
	return a.call(ctx, http.MethodPost, "/v1/tools/"+name, body)
}

func (a *Akunaki) call(ctx context.Context, method, path string, body []byte) (string, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("akunaki request failed: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxAkunakiBytes+1))
	if err != nil {
		return "", err
	}
	truncated := false
	if len(payload) > maxAkunakiBytes {
		payload, truncated = payload[:maxAkunakiBytes], true
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The backend's error bodies are typed and PHI-free ({"detail":
		// {"code": ...}}); a bounded snippet tells the model what to fix.
		snippet := strings.TrimSpace(string(payload))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return "", fmt.Errorf("akunaki returned http %d: %s", resp.StatusCode, snippet)
	}

	text := string(payload)
	if truncated {
		text += "\n[truncated]"
	}
	return text, nil
}
