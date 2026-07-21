// Package model is Odin's provider layer.
//
// Every provider resolves to the same thing at request time: a bearer token
// plus an OpenAI-compatible (or Anthropic) endpoint. The only real variation
// is where the token comes from — a static key, or an OAuth token that must
// be refreshed before it expires. That split is the TokenSource interface;
// the transports below don't care which one they got.
package model

import (
	"context"
	"encoding/json"
	"fmt"
)

// Role values used in Message.Role.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// StopReason values returned in Response.StopReason.
const (
	StopEndTurn = "end_turn"   // model finished normally
	StopToolUse = "tool_use"   // model wants one or more tools run
	StopLength  = "max_tokens" // hit the output cap mid-thought
)

// Message is one turn of conversation. ToolCalls is set only on assistant
// turns; ToolCallID and Name are set only on RoleTool turns, echoing the call
// they answer.
type Message struct {
	Role          string         `json:"role"`
	Content       string         `json:"content"`
	ToolCalls     []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID    string         `json:"tool_call_id,omitempty"`
	Name          string         `json:"name,omitempty"`
	ProviderState *ProviderState `json:"provider_state,omitempty"`
}

// ProviderState carries opaque response items that a provider requires on a
// later turn. It is deliberately scoped to the provider that produced it so a
// fallback never receives another backend's private wire format.
type ProviderState struct {
	Provider string          `json:"provider"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
}

// ToolCall is a single tool invocation requested by the model.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Tool is a tool definition sent to the provider.
//
// Keep Schema small: 2-4 fields, required ones first, no trailing optional
// sprawl. On 2026-07-04 a 15-param schema with the required fields buried
// under optionals made glm-5.2 emit the same malformed call 162 times. The
// hard-stop guardrail in agent/ bounds the damage; a small schema prevents it.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

// Request is one inference call. System is kept separate from Messages so the
// transports can place it where each API expects (a leading system message for
// OpenAI-compatible, a top-level field for Anthropic) and so the stable prefix
// stays byte-identical across turns for prompt caching.
type Request struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
	// Effort is a portable reasoning hint: "low" | "medium" | "high".
	// Transports map it to whatever the provider actually accepts, or drop it.
	Effort string
}

// Response is one inference result, normalized across providers.
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
	Usage      Usage
	RateLimit  *RateLimit
	// ProviderState is copied onto the assistant history message by the agent
	// loop and replayed only when the same provider handles the next turn.
	ProviderState *ProviderState
	// Model and Provider record who actually served this, which matters when
	// a fallback chain silently moved off the primary.
	Model    string
	Provider string
}

// Usage is per-request token accounting. Cached is prompt-cache reads, which
// bill far cheaper than fresh input — a non-zero value means the stable
// prefix is being reused.
type Usage struct {
	Input  int
	Output int
	Cached int
}

// Provider is one callable model behind one endpoint.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Response, error)
}

// TokenSource yields a bearer token, refreshing it if needed. Implementations
// must be safe for concurrent use: the scheduler and the Telegram gateway can
// call Complete at the same time, and a naive refresh would race.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// RecoverableTokenSource can replace a credential rejected by a provider.
// Implementations must compare rejectedToken with durable state before
// refreshing: another process may already have rotated the shared token.
type RecoverableTokenSource interface {
	TokenSource
	Recover(ctx context.Context, status int, rejectedToken string) (token string, retry bool, err error)
}

// StaticToken is a TokenSource for API keys, which never expire.
type StaticToken string

func (s StaticToken) Token(context.Context) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty api key")
	}
	return string(s), nil
}

// Error is a provider failure carrying enough detail for the fallback chain to
// decide whether trying the next provider could plausibly help.
type Error struct {
	Provider string
	Status   int
	Message  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: http %d: %s", e.Provider, e.Status, e.Message)
}

// Retryable reports whether a different provider might succeed where this one
// failed. Auth, rate-limit, and server errors are worth failing over; a 400 is
// usually our own malformed request and will fail identically everywhere.
//
// 403 is included deliberately: on 2026-07-14 xAI returned 403 "out of
// credits" on every turn, and on 2026-07-12 403 "not available in your region"
// — both are provider-scoped conditions a different provider does not share.
func (e *Error) Retryable() bool {
	switch e.Status {
	case 401, 402, 403, 408, 409, 425, 429:
		return true
	}
	return e.Status >= 500
}
