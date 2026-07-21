package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Verification is the result of a provider-pinned live protocol check.
type Verification struct {
	Provider       string
	Model          string
	CatalogChecked bool
	ToolCall       bool
	Continuation   bool
}

// VerifyProvider checks model discovery, tool calling, provider-state replay,
// and the post-tool continuation against one provider. It never falls back.
func VerifyProvider(ctx context.Context, provider Provider) (*Verification, error) {
	providerName, modelName, _ := strings.Cut(provider.Name(), "/")
	result := &Verification{Provider: providerName, Model: modelName}

	if catalog, ok := provider.(ModelCatalog); ok {
		models, err := catalog.Models(ctx)
		if err != nil && !errors.Is(err, ErrCatalogUnsupported) {
			return result, fmt.Errorf("model catalog: %w", err)
		}
		if err == nil {
			result.CatalogChecked = true
			found := false
			for _, candidate := range models {
				if candidate == modelName {
					found = true
					break
				}
			}
			if !found {
				return result, fmt.Errorf("configured model %q is absent from the live catalog", modelName)
			}
		}
	}

	token := makeVerifyToken()
	tool := Tool{
		Name:        "odin_transport_probe",
		Description: "Return the supplied transport verification token.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"token": map[string]any{"type": "string"},
			},
			"required": []string{"token"},
		},
	}
	userPrompt := fmt.Sprintf(
		"Call odin_transport_probe exactly once with token %q. After its result, reply exactly ODIN_SMOKE_OK:%s.",
		token, token,
	)
	first, err := provider.Complete(ctx, Request{
		System:   "This is an automated provider transport check. Follow the requested tool protocol exactly.",
		Messages: []Message{{Role: RoleUser, Content: userPrompt}},
		Tools:    []Tool{tool}, MaxTokens: 4096, Effort: "high",
	})
	if err != nil {
		return result, fmt.Errorf("tool-call turn: %w", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != tool.Name {
		return result, fmt.Errorf("tool-call turn: expected one %s call, got %d", tool.Name, len(first.ToolCalls))
	}
	var args struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(first.ToolCalls[0].Input, &args); err != nil {
		return result, fmt.Errorf("tool-call turn: decode arguments: %w", err)
	}
	if args.Token != token {
		return result, fmt.Errorf("tool-call turn: token mismatch")
	}
	result.ToolCall = true

	second, err := provider.Complete(ctx, Request{
		System: "This is an automated provider transport check. Follow the requested tool protocol exactly.",
		Messages: []Message{
			{Role: RoleUser, Content: userPrompt},
			{
				Role: RoleAssistant, Content: first.Text, ToolCalls: first.ToolCalls,
				ProviderState: first.ProviderState,
			},
			{Role: RoleTool, ToolCallID: first.ToolCalls[0].ID, Name: tool.Name, Content: token},
		},
		MaxTokens: 1024, Effort: "high",
	})
	if err != nil {
		return result, fmt.Errorf("continuation turn: %w", err)
	}
	expected := "ODIN_SMOKE_OK:" + token
	if !strings.Contains(second.Text, expected) {
		return result, fmt.Errorf("continuation turn: expected %q, got %q", expected, truncate(second.Text, 200))
	}
	result.Continuation = true
	return result, nil
}

func makeVerifyToken() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "odin-smoke-fallback"
	}
	return "odin-smoke-" + hex.EncodeToString(raw[:])
}
