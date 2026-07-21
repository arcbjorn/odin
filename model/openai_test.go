package model

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIReplaysReasoningContent(t *testing.T) {
	provider := NewOpenAI(OpenAIConfig{
		Provider: "opencode-go", Model: "kimi-k3", BaseURL: "https://opencode.test/v1",
		Tokens: StaticToken("token"),
	})
	call := 0
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call++
		var body struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if call == 2 {
			found := false
			for _, message := range body.Messages {
				if got, ok := message["reasoning_content"]; ok && string(got) == `"opaque thinking"` {
					found = true
				}
			}
			if !found {
				t.Fatalf("reasoning_content was not replayed: %#v", body.Messages)
			}
			return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`), nil
		}
		return jsonResponse(http.StatusOK, `{
			"choices":[{"message":{"content":"","reasoning_content":"opaque thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"query","arguments":"{}"}}]},"finish_reason":"tool_calls"}]
		}`), nil
	})}

	first, err := provider.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "go"}}, Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderState == nil || !strings.Contains(string(first.ProviderState.Data), "opaque thinking") {
		t.Fatalf("provider state = %#v", first.ProviderState)
	}
	_, err = provider.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: RoleUser, Content: "go"},
			{Role: RoleAssistant, ToolCalls: first.ToolCalls, ProviderState: first.ProviderState},
			{Role: RoleTool, ToolCallID: "c1", Content: "ok"},
		},
		Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenAISendsConfiguredHeaders(t *testing.T) {
	provider := NewOpenAI(OpenAIConfig{
		Provider: "grok", Model: "grok-build", BaseURL: "https://grok.test/v1",
		Tokens: StaticToken("token"), DropEffort: true,
		Headers: map[string]string{
			"X-XAI-Token-Auth":      "xai-grok-cli",
			"x-grok-model-override": "grok-build",
		},
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" || req.Header.Get("x-grok-model-override") != "grok-build" {
			t.Fatalf("headers = %#v", req.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["reasoning_effort"]; exists {
			t.Fatalf("Grok Build request included reasoning_effort: %#v", body)
		}
		return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})}

	response, err := provider.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hello"}}, Effort: "high",
	})
	if err != nil || response.Text != "ok" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
