package model

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAnthropicSubscriptionUsesBearerIdentity(t *testing.T) {
	provider := NewAnthropic(AnthropicConfig{
		Provider: "claude", Model: "claude-opus-4-8", BaseURL: "https://api.anthropic.com/v1",
		Tokens: StaticToken("oauth-token"), Bearer: true, OAuthIdentity: true,
		UserAgent: "claude-code/test (external, cli)",
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := req.Header.Get("x-api-key"); got != "" {
			t.Fatalf("unexpected x-api-key = %q", got)
		}
		if got := req.Header.Get("anthropic-beta"); got != "claude-code-20250219,oauth-2025-04-20" {
			t.Fatalf("anthropic-beta = %q", got)
		}
		var body struct {
			System []antSystem `json:"system"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.System) != 2 || body.System[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." {
			t.Fatalf("system = %+v", body.System)
		}
		return jsonResponse(http.StatusOK, `{"model":"claude-opus-4-8","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`), nil
	})}

	response, err := provider.Complete(context.Background(), Request{
		System: "be direct", Messages: []Message{{Role: RoleUser, Content: "hello"}}, Effort: "high",
	})
	if err != nil || response.Text != "ok" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestAnthropicCompatibleCanDropThinking(t *testing.T) {
	provider := NewAnthropic(AnthropicConfig{
		Provider: "opencode-go", Model: "minimax-m2.7", BaseURL: "https://opencode.ai/zen/go/v1",
		Tokens: StaticToken("key"), DropThinking: true,
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-api-key") != "key" {
			t.Fatal("OpenCode Anthropic route must use x-api-key")
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["thinking"]; exists {
			t.Fatalf("third-party request contains thinking: %#v", body)
		}
		return jsonResponse(http.StatusOK, `{"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`), nil
	})}
	if _, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
}
