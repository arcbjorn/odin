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
	// Include asks codex to return the encrypted reasoning item. Without it, a
	// turn that reasons before acting completes with the reasoning sealed and
	// no tool call surfaced — end_turn, empty text, zero calls.
	Include []string `json:"include,omitempty"`
	Store   bool     `json:"store"`
	// Stream is required by the codex backend, which rejects non-streaming
	// requests with 400 "Stream must be set to true". Other Responses hosts
	// (xAI) accept a single JSON response, so this is set only for codex.
	Stream bool `json:"stream,omitempty"`
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
	Description string         `json:"description"`
	// Strict must be present. The codex Responses backend silently ignores a
	// function tool that omits it — the model then never sees a callable tool
	// and replies with plain text and no tool call.
	Strict     bool           `json:"strict"`
	Parameters map[string]any `json:"parameters"`
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
		Model:        r.model,
		Instructions: req.System,
		Store:        false,
		// The codex backend refuses non-streaming requests; xAI accepts them.
		Stream: r.codex,
	}
	// The codex proxy rejects max_output_tokens with 400 "Unsupported
	// parameter"; the standard Responses API accepts it. Send it everywhere
	// except codex.
	if !r.codex {
		body.MaxOutputTokens = req.MaxTokens
	}
	if req.Effort != "" && !r.xai {
		body.Reasoning = &responsesReasoning{Effort: req.Effort}
		body.Reasoning.Summary = "auto"
		if r.codex {
			// codex reasons before acting and seals the reasoning item; asking
			// for it back is what lets the following tool call surface.
			body.Include = []string{"reasoning.encrypted_content"}
		}
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
		params := tool.Schema
		if params == nil {
			// The backend rejects a null parameters field; an empty object
			// schema is the valid "no arguments" shape.
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		body.Tools = append(body.Tools, responsesTool{
			Type: "function", Name: tool.Name, Description: tool.Description,
			Strict: false, Parameters: params,
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
			httpReq.Header.Set("Accept", "text/event-stream")
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
	// A streamed codex reply is SSE, and the tool call arrives as a sequence
	// of function_call_arguments.delta events — it is not present, assembled,
	// in any single JSON object. Project the event stream into a Response
	// directly. A non-200 arrives as a normal JSON body even when we asked for
	// a stream, so only project on success.
	if body.Stream && resp.StatusCode == http.StatusOK {
		out, perr := projectResponsesStream(payload)
		if perr != nil {
			return nil, &Error{Provider: r.provider, Status: 502, Message: perr.Error()}
		}
		out.Provider = r.provider
		if out.ProviderState != nil {
			out.ProviderState.Provider = r.provider
		}
		if out.Model == "" {
			out.Model = r.model
		}
		out.RateLimit = parseRateLimitHeaders(resp.Header, r.provider)
		return out, nil
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

// projectResponsesStream assembles a Response from a Responses API SSE stream.
//
// The stream cannot be reduced to one JSON object: a tool call is delivered as
// an output_item.added (with empty arguments) followed by a run of
// function_call_arguments.delta events that must be concatenated. Text arrives
// the same way via output_text.delta. The terminal response.completed event
// carries usage and the fully-assembled output items, which are also used as
// the opaque provider-state for replay.
func projectResponsesStream(payload []byte) (*Response, error) {
	out := &Response{StopReason: StopEndTurn}

	// Tool calls, keyed by their streaming item id, with arguments accumulated
	// across delta events. order preserves emission order for a stable result.
	type pendingCall struct {
		callID, name string
		args         strings.Builder
	}
	calls := map[string]*pendingCall{}
	var order []string

	var completedOutput json.RawMessage
	sawTerminal := false

	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}

		var ev struct {
			Type   string `json:"type"`
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
			Item   struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
			Response struct {
				Status string          `json:"status"`
				Output json.RawMessage `json:"output"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
				Usage struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					InputTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}

		switch ev.Type {
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				if _, seen := calls[ev.Item.ID]; !seen {
					order = append(order, ev.Item.ID)
				}
				calls[ev.Item.ID] = &pendingCall{callID: ev.Item.CallID, name: ev.Item.Name}
			}
		case "response.function_call_arguments.delta":
			if c := calls[ev.ItemID]; c != nil {
				c.args.WriteString(ev.Delta)
			}
		case "response.output_text.delta":
			out.Text += ev.Delta
		case "response.completed", "response.failed", "response.incomplete":
			sawTerminal = true
			completedOutput = ev.Response.Output
			out.Usage = Usage{
				Input:  ev.Response.Usage.InputTokens,
				Output: ev.Response.Usage.OutputTokens,
				Cached: ev.Response.Usage.InputTokensDetails.CachedTokens,
			}
			if ev.Type == "response.failed" && ev.Response.Error != nil {
				return nil, fmt.Errorf("codex stream failed: %s", ev.Response.Error.Message)
			}
			if ev.Response.Status == "incomplete" {
				out.StopReason = StopLength
			}
		}
	}

	if !sawTerminal {
		return nil, fmt.Errorf("no terminal response event in codex stream")
	}

	for _, id := range order {
		c := calls[id]
		args := c.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: c.callID, Name: c.name, Input: json.RawMessage(args)})
	}
	if len(out.ToolCalls) > 0 {
		out.StopReason = StopToolUse
	}

	// The completed event's output array is the opaque state to replay on the
	// next turn (it carries the encrypted reasoning items codex requires back).
	if len(completedOutput) > 0 {
		out.ProviderState = &ProviderState{Kind: "responses_output", Data: completedOutput}
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
