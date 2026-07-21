package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const anthropicVersion = "2023-06-01"

// Anthropic is the /v1/messages transport. Shape differs from
// OpenAI-compatible in three ways that matter:
//   - system is a top-level field, not a leading message
//   - tool results are user-turn content blocks, not a "tool" role
//   - tool input is a JSON object, not a JSON-encoded string
type Anthropic struct {
	provider      string
	model         string
	baseURL       string
	tokens        TokenSource
	http          *http.Client
	bearer        bool
	oauthIdentity bool
	userAgent     string
	dropThinking  bool
}

// AnthropicConfig configures the Anthropic provider.
type AnthropicConfig struct {
	Provider string
	Model    string
	BaseURL  string
	Tokens   TokenSource
	// Bearer sends Authorization instead of x-api-key. Subscription OAuth and
	// MiniMax's Anthropic-compatible endpoint require it.
	Bearer bool
	// OAuthIdentity adds the Claude Code request identity required for Claude
	// Pro/Max subscription traffic.
	OAuthIdentity bool
	UserAgent     string
	// DropThinking omits Anthropic's adaptive-thinking fields for compatible
	// endpoints whose non-Claude models reject that request shape.
	DropThinking bool
	Timeout      time.Duration
}

// NewAnthropic builds an Anthropic provider. Model defaults to Opus 4.8.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	if cfg.Model == "" {
		cfg.Model = "claude-opus-4-8"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com/v1"
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	return &Anthropic{
		provider:      cfg.Provider,
		model:         cfg.Model,
		baseURL:       cfg.BaseURL,
		tokens:        cfg.Tokens,
		bearer:        cfg.Bearer,
		oauthIdentity: cfg.OAuthIdentity,
		userAgent:     cfg.UserAgent,
		dropThinking:  cfg.DropThinking,
		http:          &http.Client{Timeout: timeout},
	}
}

func (a *Anthropic) Name() string { return a.provider + "/" + a.model }

type antBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

type antSystem struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type antTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type antRequest struct {
	Model        string         `json:"model"`
	MaxTokens    int            `json:"max_tokens"`
	System       []antSystem    `json:"system,omitempty"`
	Messages     []antMessage   `json:"messages"`
	Tools        []antTool      `json:"tools,omitempty"`
	Thinking     map[string]any `json:"thinking,omitempty"`
	OutputConfig map[string]any `json:"output_config,omitempty"`
}

type antResponse struct {
	Content    []antBlock `json:"content"`
	StopReason string     `json:"stop_reason"`
	Model      string     `json:"model"`
	Usage      struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete runs one inference call.
func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 16000
	}

	body := antRequest{
		Model:     a.model,
		MaxTokens: maxTokens,
		Messages:  make([]antMessage, 0, len(req.Messages)),
	}

	// cache_control on the system block: the prefix (SOUL + skill + job prompt)
	// is identical every run, so this bills at read rates after the first call.
	if a.oauthIdentity {
		body.System = append(body.System, antSystem{
			Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude.",
		})
	}
	if req.System != "" {
		body.System = append(body.System, antSystem{
			Type:         "text",
			Text:         req.System,
			CacheControl: map[string]any{"type": "ephemeral"},
		})
	}

	// Opus 4.8 takes adaptive thinking; budget_tokens is rejected with a 400.
	// Effort rides in output_config, not at the top level.
	if !a.dropThinking {
		body.Thinking = map[string]any{"type": "adaptive"}
		if req.Effort != "" {
			body.OutputConfig = map[string]any{"effort": req.Effort}
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			// Tool results are user-turn content blocks here, not their own role.
			body.Messages = append(body.Messages, antMessage{
				Role: RoleUser,
				Content: []antBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   m.Content,
				}},
			})
		case RoleAssistant:
			am := antMessage{Role: RoleAssistant}
			if m.Content != "" {
				am.Content = append(am.Content, antBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				am.Content = append(am.Content, antBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: tc.Input,
				})
			}
			if len(am.Content) == 0 {
				continue
			}
			body.Messages = append(body.Messages, am)
		default:
			body.Messages = append(body.Messages, antMessage{
				Role:    RoleUser,
				Content: []antBlock{{Type: "text", Text: m.Content}},
			})
		}
	}

	for _, t := range req.Tools {
		body.Tools = append(body.Tools, antTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Schema,
		})
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := doTokenRequest(ctx, a.http, a.tokens, func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/messages", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if a.bearer {
			httpReq.Header.Set("Authorization", "Bearer "+token)
		} else {
			httpReq.Header.Set("x-api-key", token)
		}
		httpReq.Header.Set("anthropic-version", anthropicVersion)
		if a.oauthIdentity {
			httpReq.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
			httpReq.Header.Set("x-app", "cli")
			if a.userAgent != "" {
				httpReq.Header.Set("User-Agent", a.userAgent)
			}
		}
		return httpReq, nil
	})
	if err != nil {
		var credentialErr *credentialError
		if errors.As(err, &credentialErr) {
			return nil, &Error{Provider: a.provider, Status: 401, Message: err.Error()}
		}
		return nil, &Error{Provider: a.provider, Status: 503, Message: err.Error()}
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &Error{Provider: a.provider, Status: 503, Message: err.Error()}
	}

	var parsed antResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, &Error{
			Provider: a.provider,
			Status:   resp.StatusCode,
			Message:  fmt.Sprintf("invalid json: %s", truncate(string(payload), 200)),
		}
	}

	if resp.StatusCode != http.StatusOK {
		msg := truncate(string(payload), 300)
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return nil, &Error{Provider: a.provider, Status: resp.StatusCode, Message: msg}
	}

	out := &Response{
		StopReason: mapAnthropicStop(parsed.StopReason),
		Model:      parsed.Model,
		Provider:   a.provider,
		RateLimit:  parseRateLimitHeaders(resp.Header, a.provider),
		Usage: Usage{
			Input:  parsed.Usage.InputTokens,
			Output: parsed.Usage.OutputTokens,
			Cached: parsed.Usage.CacheReadInputTokens,
		},
	}
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			out.Text += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
		// thinking blocks are skipped: display defaults to "omitted", so their
		// text is empty anyway.
	}
	if len(out.ToolCalls) > 0 {
		out.StopReason = StopToolUse
	}
	return out, nil
}

func mapAnthropicStop(r string) string {
	switch r {
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopLength
	default:
		return StopEndTurn
	}
}
