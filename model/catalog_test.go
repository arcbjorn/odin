package model

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestCodexModelsUsesLiveCatalog(t *testing.T) {
	provider := NewResponses(ResponsesConfig{
		Provider: "codex", Model: "gpt-5", BaseURL: "https://chatgpt.test/backend-api/codex",
		Tokens: StaticToken("token"), Codex: true,
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/codex/models" || req.URL.Query().Get("client_version") != "1.0.0" {
			t.Fatalf("url = %s", req.URL)
		}
		if req.Header.Get("originator") != "codex_cli_rs" {
			t.Fatalf("headers = %#v", req.Header)
		}
		return jsonResponse(http.StatusOK, `{"models":[
			{"slug":"gpt-low","priority":20},
			{"slug":"hidden","priority":1,"visibility":"hide"},
			{"slug":"gpt-high","priority":10}
		]}`), nil
	})}

	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gpt-high", "gpt-low"}; !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestOpenAIModelsSortsAndDeduplicates(t *testing.T) {
	provider := NewOpenAI(OpenAIConfig{
		Provider: "openai", Model: "gpt", BaseURL: "https://api.test/v1", Tokens: StaticToken("token"),
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/models" || req.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request = %s %#v", req.URL, req.Header)
		}
		return jsonResponse(http.StatusOK, `{"data":[{"id":"z"},{"id":"a"},{"id":"a"}]}`), nil
	})}

	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "z"}; !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestAnthropicModelsUsesOAuthIdentity(t *testing.T) {
	provider := NewAnthropic(AnthropicConfig{
		Provider: "claude", Model: "claude", BaseURL: "https://api.anthropic.test/v1",
		Tokens: StaticToken("token"), Bearer: true, OAuthIdentity: true, UserAgent: "claude-code/test",
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/models" || req.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request = %s %#v", req.URL, req.Header)
		}
		if req.Header.Get("anthropic-version") != anthropicVersion || req.Header.Get("User-Agent") != "claude-code/test" {
			t.Fatalf("headers = %#v", req.Header)
		}
		return jsonResponse(http.StatusOK, `{"data":[{"id":"claude-haiku"},{"id":"claude-opus"},{"id":"claude-sonnet"}]}`), nil
	})}

	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude-opus", "claude-sonnet", "claude-haiku"}; !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}
