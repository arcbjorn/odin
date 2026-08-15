package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/arcbjorn/odin/agent"
	"github.com/arcbjorn/odin/model"
)

const (
	// maxToolRegBytes caps one registry response. Registries serve bounded,
	// typed results, so anything larger indicates a mistake — truncate rather
	// than blow the context window.
	maxToolRegBytes = 64 << 10

	toolRegTimeout = 15 * time.Second
)

// toolRegName constrains a registry's name: it prefixes the exposed tool
// names, so it must stay a short schema-safe identifier.
var toolRegName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,23}$`)

// ToolReg connects one remote typed tool registry: an HTTP service that
// lists a tool catalog (GET the registry URL) and invokes one entry by name
// (POST <registry-url>/<entry>), authenticated with a bearer token.
//
// Deliberately a passthrough, not per-entry wrappers: the remote registry is
// the authority on its own catalog — typed, versioned, and authorized
// server-side — so duplicating its entries here would only create a second
// list that drifts. Each registry contributes exactly two tools: list the
// catalog, invoke one entry. What the entries mean and when to call them is
// domain knowledge, and domain knowledge belongs in the profile's skills,
// never in this runtime.
//
// This client never touches the reader or any third party: requests go
// direct to the configured registry, or nowhere.
type ToolReg struct {
	name        string
	registryURL string
	token       string
	http        *http.Client
}

// ToolRegConfig configures one registry connection. Every field is required:
// a profile that enables the toolset but cannot reach an authenticated
// registry must fail at build, not at the first scheduled job.
type ToolRegConfig struct {
	// Name labels the registry and prefixes its tool names: a registry named
	// "health" exposes health_tools and health_tool.
	Name string
	// RegistryURL is the catalog endpoint itself, e.g.
	// https://backend.example.com/v1/tools — GET lists it, POST
	// <RegistryURL>/<entry> invokes one entry. No path is assumed beyond it.
	RegistryURL string
	// Token is the bearer credential (resolved from the environment by the
	// profile build; never lives in config.toml).
	Token string
}

// NewToolReg validates the config and builds the registry connection.
func NewToolReg(cfg ToolRegConfig) (*ToolReg, error) {
	name := strings.TrimSpace(cfg.Name)
	if !toolRegName.MatchString(name) {
		return nil, fmt.Errorf(
			"toolreg: name %q must be a short identifier (%s)", cfg.Name, toolRegName)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.RegistryURL), "/")
	if base == "" {
		return nil, fmt.Errorf("toolreg %q: url is required", name)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("toolreg %q: url must be an absolute http(s) URL", name)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("toolreg %q: token is required", name)
	}
	return &ToolReg{
		name:        name,
		registryURL: base,
		token:       strings.TrimSpace(cfg.Token),
		http:        &http.Client{Timeout: toolRegTimeout},
	}, nil
}

// Tools returns the two tools this registry contributes.
func (r *ToolReg) Tools() []agent.Tool {
	return []agent.Tool{r.listTool(), r.invokeTool()}
}

func (r *ToolReg) listTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name: r.name + "_tools",
			Description: fmt.Sprintf(
				"List the %s registry's tools: each entry's name, input, and whether it reads or mutates.",
				r.name),
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		Handle: r.handleList,
	}
}

func (r *ToolReg) invokeTool() agent.Tool {
	return agent.Tool{
		Def: model.Tool{
			Name: r.name + "_tool",
			Description: fmt.Sprintf(
				"Invoke one %s registry tool by name (see %s_tools). The registry enforces its own authorization: entries this credential cannot invoke are refused.",
				r.name, r.name),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": fmt.Sprintf("Entry name from %s_tools.", r.name),
					},
					"input": map[string]any{
						"type":        "object",
						"description": "Entry input, e.g. {\"day\": \"2026-08-15\"}. Omit when the entry needs none.",
					},
				},
				"required": []string{"name"},
			},
		},
		Handle: r.handleInvoke,
	}
}

type toolRegInvokeInput struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (r *ToolReg) handleList(ctx context.Context, _ json.RawMessage) (string, error) {
	return r.call(ctx, http.MethodGet, "", nil)
}

func (r *ToolReg) handleInvoke(ctx context.Context, raw json.RawMessage) (string, error) {
	var in toolRegInvokeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	// The entry name becomes a path segment; keep it to the registry's
	// dotted identifiers so the request cannot be steered off the registry.
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
	return r.call(ctx, http.MethodPost, "/"+name, body)
}

func (r *ToolReg) call(ctx context.Context, method, path string, body []byte) (string, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, r.registryURL+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s registry request failed: %w", r.name, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxToolRegBytes+1))
	if err != nil {
		return "", err
	}
	truncated := false
	if len(payload) > maxToolRegBytes {
		payload, truncated = payload[:maxToolRegBytes], true
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A registry's error body is expected to be typed and small; a
		// bounded snippet tells the model what to fix without flooding it.
		snippet := strings.TrimSpace(string(payload))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return "", fmt.Errorf("%s registry returned http %d: %s", r.name, resp.StatusCode, snippet)
	}

	text := string(payload)
	if truncated {
		text += "\n[truncated]"
	}
	return text, nil
}
