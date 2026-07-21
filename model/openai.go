package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI is the /v1/chat/completions transport. It covers every provider Odin
// uses except Anthropic: xAI (api.x.ai/v1), DeepSeek, GLM, Kimi, opencode-go.
// Adding one of those is a config entry, not code.
type OpenAI struct {
	provider string
	model    string
	baseURL  string
	tokens   TokenSource
	http     *http.Client
	headers  map[string]string

	// dropEffort suppresses reasoning_effort for models that reject it.
	// xAI returns HTTP 400 on grok-4, grok-4-fast, grok-3, and grok-code-fast
	// even though those models do reason. grok-4.5 accepts low|medium|high.
	dropEffort bool
}

// OpenAIConfig configures an OpenAI-compatible provider.
type OpenAIConfig struct {
	Provider   string
	Model      string
	BaseURL    string
	Tokens     TokenSource
	DropEffort bool
	Headers    map[string]string
	Timeout    time.Duration
}

// NewOpenAI builds an OpenAI-compatible provider.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	timeout := cfg.Timeout
	if timeout == 0 {
		// Fail fast so a stalled provider drops to the next fallback instead of
		// hanging the turn. A dead primary combined with a long timeout and
		// retries once left interactive turns stuck for minutes.
		timeout = 120 * time.Second
	}
	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
	}
	return &OpenAI{
		provider:   cfg.Provider,
		model:      cfg.Model,
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		tokens:     cfg.Tokens,
		dropEffort: cfg.DropEffort,
		headers:    headers,
		http:       &http.Client{Timeout: timeout},
	}
}

func (o *OpenAI) Name() string { return o.provider + "/" + o.model }

type oaiMessage struct {
	Role             string          `json:"role"`
	Content          string          `json:"content,omitempty"`
	ToolCalls        []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	Reasoning        json.RawMessage `json:"reasoning,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON *string* on the wire, not an object — the one
		// shape difference from Anthropic that actually bites.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiRequest struct {
	Model           string       `json:"model"`
	Messages        []oaiMessage `json:"messages"`
	MaxTokens       int          `json:"max_tokens,omitempty"`
	Tools           []oaiTool    `json:"tools,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content          string          `json:"content"`
			ToolCalls        []oaiToolCall   `json:"tool_calls"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			ReasoningDetails json.RawMessage `json:"reasoning_details"`
			Reasoning        json.RawMessage `json:"reasoning"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete runs one inference call.
func (o *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	body := oaiRequest{
		Model:     o.model,
		MaxTokens: req.MaxTokens,
		Messages:  make([]oaiMessage, 0, len(req.Messages)+1),
	}
	if !o.dropEffort {
		body.ReasoningEffort = req.Effort
	}

	// System goes first and stays byte-identical across turns so the provider's
	// prompt cache can hit on it. Volatile data belongs in the last user turn.
	if req.System != "" {
		body.Messages = append(body.Messages, oaiMessage{Role: RoleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		om := oaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if m.ProviderState != nil && m.ProviderState.Provider == o.provider && m.ProviderState.Kind == "chat_reasoning" {
			var state struct {
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				ReasoningDetails json.RawMessage `json:"reasoning_details"`
				Reasoning        json.RawMessage `json:"reasoning"`
			}
			if json.Unmarshal(m.ProviderState.Data, &state) == nil {
				om.ReasoningContent = state.ReasoningContent
				om.ReasoningDetails = state.ReasoningDetails
				om.Reasoning = state.Reasoning
			}
		}
		for _, tc := range m.ToolCalls {
			var c oaiToolCall
			c.ID = tc.ID
			c.Type = "function"
			c.Function.Name = tc.Name
			c.Function.Arguments = string(tc.Input)
			om.ToolCalls = append(om.ToolCalls, c)
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range req.Tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Schema
		body.Tools = append(body.Tools, ot)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := doTokenRequest(ctx, o.http, o.tokens, func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		for name, value := range o.headers {
			httpReq.Header.Set(name, value)
		}
		return httpReq, nil
	})
	if err != nil {
		var credentialErr *credentialError
		if errors.As(err, &credentialErr) {
			return nil, &Error{Provider: o.provider, Status: 401, Message: err.Error()}
		}
		// Transport failure (timeout, DNS, refused). Status 0 is not in the
		// Retryable set above, so mark it 503: a different provider may work.
		return nil, &Error{Provider: o.provider, Status: 503, Message: err.Error()}
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &Error{Provider: o.provider, Status: 503, Message: err.Error()}
	}

	var parsed oaiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, &Error{
			Provider: o.provider,
			Status:   resp.StatusCode,
			Message:  fmt.Sprintf("invalid json: %s", truncate(string(payload), 200)),
		}
	}

	if resp.StatusCode != http.StatusOK {
		msg := truncate(string(payload), 300)
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return nil, &Error{Provider: o.provider, Status: resp.StatusCode, Message: msg}
	}
	if len(parsed.Choices) == 0 {
		return nil, &Error{Provider: o.provider, Status: 502, Message: "no choices in response"}
	}

	choice := parsed.Choices[0]
	out := &Response{
		Text:       choice.Message.Content,
		StopReason: mapFinishReason(choice.FinishReason),
		Model:      o.model,
		Provider:   o.provider,
		RateLimit:  parseRateLimitHeaders(resp.Header, o.provider),
		Usage: Usage{
			Input:  parsed.Usage.PromptTokens,
			Output: parsed.Usage.CompletionTokens,
			Cached: parsed.Usage.PromptTokensDetails.CachedTokens,
		},
	}
	reasoningState, err := json.Marshal(struct {
		ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
		ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
		Reasoning        json.RawMessage `json:"reasoning,omitempty"`
	}{
		ReasoningContent: choice.Message.ReasoningContent,
		ReasoningDetails: choice.Message.ReasoningDetails,
		Reasoning:        choice.Message.Reasoning,
	})
	if err == nil && string(reasoningState) != "{}" {
		out.ProviderState = &ProviderState{Provider: o.provider, Kind: "chat_reasoning", Data: reasoningState}
	}
	for _, tc := range choice.Message.ToolCalls {
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(args),
		})
	}
	// Some providers return tool calls with finish_reason "stop". Trust the
	// presence of calls over the label, or the agent loop would end early.
	if len(out.ToolCalls) > 0 {
		out.StopReason = StopToolUse
	}
	return out, nil
}

func mapFinishReason(r string) string {
	switch r {
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopLength
	default:
		return StopEndTurn
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
