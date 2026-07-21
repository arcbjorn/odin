package model

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

type verifyProvider struct {
	requests []Request
}

func (v *verifyProvider) Name() string { return "test/model-1" }

func (v *verifyProvider) Models(context.Context) ([]string, error) {
	return []string{"model-1", "model-2"}, nil
}

func (v *verifyProvider) Complete(_ context.Context, req Request) (*Response, error) {
	v.requests = append(v.requests, req)
	if len(v.requests) == 1 {
		token := regexp.MustCompile(`odin-smoke-[a-z0-9]+`).FindString(req.Messages[0].Content)
		args, _ := json.Marshal(map[string]string{"token": token})
		return &Response{
			Model: "model-1", Provider: "test", StopReason: StopToolUse,
			ToolCalls:     []ToolCall{{ID: "c1", Name: "odin_transport_probe", Input: args}},
			ProviderState: &ProviderState{Provider: "test", Kind: "test", Data: json.RawMessage(`{"opaque":true}`)},
		}, nil
	}
	return &Response{
		Model: "model-1", Provider: "test", StopReason: StopEndTurn,
		Text: "ODIN_SMOKE_OK:" + req.Messages[len(req.Messages)-1].Content,
	}, nil
}

func TestVerifyProviderChecksToolContinuation(t *testing.T) {
	provider := &verifyProvider{}
	result, err := VerifyProvider(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CatalogChecked || !result.ToolCall || !result.Continuation {
		t.Fatalf("result = %+v", result)
	}
	if len(provider.requests) != 2 || provider.requests[1].Messages[1].ProviderState == nil {
		t.Fatalf("requests = %#v", provider.requests)
	}
}

func TestVerifyProviderRejectsMissingModel(t *testing.T) {
	provider := &missingCatalogProvider{verifyProvider: verifyProvider{}}
	_, err := VerifyProvider(context.Background(), provider)
	if err == nil {
		t.Fatal("expected missing configured model to fail")
	}
	// A stale model id should be a one-line fix: the error must name the real
	// options, not just say the configured one is wrong.
	if !strings.Contains(err.Error(), "other") {
		t.Fatalf("error should list available models, got: %v", err)
	}
}

type missingCatalogProvider struct {
	verifyProvider
}

func (m *missingCatalogProvider) Models(context.Context) ([]string, error) {
	return []string{"other"}, nil
}
