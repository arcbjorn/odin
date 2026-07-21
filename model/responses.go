package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Responses is the OpenAI Responses API transport used by ChatGPT/Codex,
// xAI OAuth, and GPT models on OpenCode Zen.
type Responses struct {
	provider string
	model    string
	baseURL  string
	tokens   TokenSource
	http     *http.Client
	codex    bool
	xai      bool
}

type ResponsesConfig struct {
	Provider string
	Model    string
	BaseURL  string
	Tokens   TokenSource
	Codex    bool
	XAI      bool
	Timeout  time.Duration
}

func NewResponses(cfg ResponsesConfig) *Responses {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	return &Responses{
		provider: cfg.Provider,
		model:    cfg.Model,
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		tokens:   cfg.Tokens,
		http:     &http.Client{Timeout: timeout},
		codex:    cfg.Codex,
		xai:      cfg.XAI,
	}
}

func (r *Responses) Name() string { return r.provider + "/" + r.model }

type responsesRequest struct {
	Model             string              `json:"model"`
	Instructions      string              `json:"instructions,omitempty"`
	Input             []json.RawMessage   `json:"input"`
	Tools             []responsesTool     `json:"tools,omitempty"`
	ToolChoice        string              `json:"tool_choice,omitempty"`
	ParallelToolCalls bool                `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   int                 `json:"max_output_tokens,omitempty"`
	Reasoning         *responsesReasoning `json:"reasoning,omitempty"`
	Store             bool                `json:"store"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesInput struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type responsesResponse struct {
	Model  string            `json:"model"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Usage  struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type responsesOutput struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func appendResponsesInput(input []json.RawMessage, item responsesInput) []json.RawMessage {
	raw, err := json.Marshal(item)
	if err != nil {
		return input
	}
	return append(input, raw)
}

func (r *Responses) Complete(ctx context.Context, req Request) (*Response, error) {
	body := responsesRequest{
		Model:           r.model,
		Instructions:    req.System,
		MaxOutputTokens: req.MaxTokens,
		Store:           false,
	}
	if req.Effort != "" && !r.xai {
		body.Reasoning = &responsesReasoning{Effort: req.Effort}
		body.Reasoning.Summary = "auto"
	}
	for _, message := range req.Messages {
		switch message.Role {
		case RoleAssistant:
			if message.ProviderState != nil && message.ProviderState.Provider == r.provider && message.ProviderState.Kind == "responses_output" {
				var items []json.RawMessage
				if json.Unmarshal(message.ProviderState.Data, &items) == nil && len(items) > 0 {
					body.Input = append(body.Input, items...)
					continue
				}
			}
			if message.Content != "" {
				body.Input = appendResponsesInput(body.Input, responsesInput{Role: RoleAssistant, Content: message.Content})
			}
			for _, call := range message.ToolCalls {
				body.Input = appendResponsesInput(body.Input, responsesInput{
					Type: "function_call", CallID: call.ID, Name: call.Name,
					Arguments: string(call.Input),
				})
			}
		case RoleTool:
			body.Input = appendResponsesInput(body.Input, responsesInput{
				Type: "function_call_output", CallID: message.ToolCallID, Output: message.Content,
			})
		default:
			body.Input = appendResponsesInput(body.Input, responsesInput{Role: RoleUser, Content: message.Content})
		}
	}
	for _, tool := range req.Tools {
		body.Tools = append(body.Tools, responsesTool{
			Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Schema,
		})
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
		body.ParallelToolCalls = true
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := doTokenRequest(ctx, r.http, r.tokens, func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/responses", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		if r.codex {
			setCodexHeaders(httpReq, token)
		}
		return httpReq, nil
	})
	if err != nil {
		var credentialErr *credentialError
		if errors.As(err, &credentialErr) {
			return nil, &Error{Provider: r.provider, Status: 401, Message: err.Error()}
		}
		return nil, &Error{Provider: r.provider, Status: 503, Message: err.Error()}
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &Error{Provider: r.provider, Status: 503, Message: err.Error()}
	}
	var parsed responsesResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, &Error{Provider: r.provider, Status: resp.StatusCode, Message: "invalid json: " + truncate(string(payload), 200)}
	}
	if resp.StatusCode != http.StatusOK {
		message := truncate(string(payload), 300)
		if parsed.Error != nil && parsed.Error.Message != "" {
			message = parsed.Error.Message
		}
		return nil, &Error{Provider: r.provider, Status: resp.StatusCode, Message: message}
	}

	out := &Response{
		Model: parsed.Model, Provider: r.provider, StopReason: StopEndTurn,
		Usage:     Usage{Input: parsed.Usage.InputTokens, Output: parsed.Usage.OutputTokens, Cached: parsed.Usage.InputTokensDetails.CachedTokens},
		RateLimit: parseRateLimitHeaders(resp.Header, r.provider),
	}
	if out.Model == "" {
		out.Model = r.model
	}
	if state, err := json.Marshal(parsed.Output); err == nil && len(parsed.Output) > 0 {
		out.ProviderState = &ProviderState{Provider: r.provider, Kind: "responses_output", Data: state}
	}
	for _, rawItem := range parsed.Output {
		var item responsesOutput
		if json.Unmarshal(rawItem, &item) != nil {
			continue
		}
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" || content.Type == "text" {
					out.Text += content.Text
				}
			}
		case "function_call":
			arguments := item.Arguments
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Input: json.RawMessage(arguments)})
		}
	}
	if len(out.ToolCalls) > 0 {
		out.StopReason = StopToolUse
	} else if parsed.Status == "incomplete" {
		out.StopReason = StopLength
	}
	return out, nil
}

func setCodexHeaders(req *http.Request, token string) {
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 (Odin)")
	req.Header.Set("originator", "codex_cli_rs")
	if accountID := jwtStringClaim(token, "https://api.openai.com/auth", "chatgpt_account_id"); accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
}

func jwtStringClaim(token, namespace, key string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if nested, ok := claims[namespace].(map[string]any); ok {
		if value, ok := nested[key].(string); ok {
			return value
		}
	}
	if value, ok := claims[key].(string); ok {
		return value
	}
	return ""
}
