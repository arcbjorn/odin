package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestResponsesCodexRequestAndToolCall(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-test"},
	})
	token := "x." + base64.RawURLEncoding.EncodeToString(claims) + ".y"

	provider := NewResponses(ResponsesConfig{
		Provider: "codex", Model: "gpt-5.5", BaseURL: "https://chatgpt.test/backend-api/codex",
		Tokens: StaticToken(token), Codex: true,
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		if got := req.Header.Get("originator"); got != "codex_cli_rs" {
			t.Fatalf("originator = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-ID"); got != "acct-test" {
			t.Fatalf("account header = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["instructions"] != "stable system" || body["store"] != false {
			t.Fatalf("body = %#v", body)
		}
		return jsonResponse(http.StatusOK, `{
			"model":"gpt-5.5","status":"completed",
			"output":[{"type":"function_call","call_id":"call-1","name":"query","arguments":"{\"sql\":\"select 1\"}"}],
			"usage":{"input_tokens":12,"output_tokens":4,"input_tokens_details":{"cached_tokens":8}}
		}`), nil
	})}

	response, err := provider.Complete(context.Background(), Request{
		System: "stable system", Messages: []Message{{Role: RoleUser, Content: "go"}},
		Tools: []Tool{{Name: "query", Schema: map[string]any{"type": "object"}}}, Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != StopToolUse || len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "query" {
		t.Fatalf("response = %+v", response)
	}
	if response.Usage.Cached != 8 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestResponsesReplaysOpaqueOutput(t *testing.T) {
	provider := NewResponses(ResponsesConfig{
		Provider: "codex", Model: "gpt-5.6-sol", BaseURL: "https://chatgpt.test/backend-api/codex",
		Tokens: StaticToken("token"), Codex: true,
	})
	call := 0
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call++
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if call == 2 {
			joinedBytes, err := json.Marshal(body.Input)
			if err != nil {
				t.Fatal(err)
			}
			joined := string(joinedBytes)
			if !strings.Contains(joined, `"type":"reasoning"`) || !strings.Contains(joined, `"encrypted_content":"opaque"`) {
				t.Fatalf("opaque response output was not replayed: %s", joined)
			}
			return jsonResponse(http.StatusOK, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`), nil
		}
		return jsonResponse(http.StatusOK, `{"status":"completed","output":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"function_call","call_id":"c1","name":"query","arguments":"{}"}]}`), nil
	})}

	first, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderState == nil {
		t.Fatal("missing provider state")
	}
	_, err = provider.Complete(context.Background(), Request{Messages: []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleAssistant, ToolCalls: first.ToolCalls, ProviderState: first.ProviderState},
		{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestXAIResponsesOmitsReasoningEffort(t *testing.T) {
	provider := NewResponses(ResponsesConfig{
		Provider: "xai", Model: "grok-4.20", BaseURL: "https://api.x.ai/v1",
		Tokens: StaticToken("token"), XAI: true,
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["reasoning"]; exists {
			t.Fatalf("xAI request must omit unsupported reasoning effort: %#v", body)
		}
		return jsonResponse(http.StatusOK, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`), nil
	})}

	response, err := provider.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hello"}}, Effort: "high",
	})
	if err != nil || response.Text != "ok" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
