package model

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// AccountUsageKind identifies a provider's account-level quota protocol.
type AccountUsageKind string

const (
	AccountUsageGrok       AccountUsageKind = "grok"
	AccountUsageKimi       AccountUsageKind = "kimi"
	AccountUsageOpenCodeGo AccountUsageKind = "opencode-go"
)

// AccountUsageConfig adds subscription quota reporting to any inference
// transport. Usage authentication remains explicit: the inference token is
// reused where the provider supports it, and OpenCode Go dashboard credentials
// are passed in by the profile builder from environment variables.
type AccountUsageConfig struct {
	Kind            AccountUsageKind
	Provider        string
	BaseURL         string
	Tokens          TokenSource
	WorkspaceID     string
	DashboardCookie string
	Timeout         time.Duration
}

// AccountUsageProvider decorates a provider without changing its inference
// protocol. This matters for OpenCode Go, whose models can use either the
// OpenAI-compatible or Anthropic-compatible transport.
type AccountUsageProvider struct {
	Provider
	kind            AccountUsageKind
	provider        string
	baseURL         string
	tokens          TokenSource
	workspaceID     string
	dashboardCookie string
	http            *http.Client
}

// WithAccountUsage adds the account usage capability to provider.
func WithAccountUsage(provider Provider, cfg AccountUsageConfig) *AccountUsageProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &AccountUsageProvider{
		Provider:        provider,
		kind:            cfg.Kind,
		provider:        cfg.Provider,
		baseURL:         strings.TrimRight(cfg.BaseURL, "/"),
		tokens:          cfg.Tokens,
		workspaceID:     strings.TrimSpace(cfg.WorkspaceID),
		dashboardCookie: strings.TrimSpace(cfg.DashboardCookie),
		http:            &http.Client{Timeout: timeout},
	}
}

// AccountUsage fetches provider-level subscription quota windows.
func (p *AccountUsageProvider) AccountUsage(ctx context.Context) (*AccountUsage, error) {
	switch p.kind {
	case AccountUsageKimi:
		return p.kimiUsage(ctx)
	case AccountUsageGrok:
		return p.grokUsage(ctx)
	case AccountUsageOpenCodeGo:
		return p.openCodeGoUsage(ctx)
	default:
		return nil, ErrUsageUnsupported
	}
}

// Models preserves the wrapped transport's optional live catalog capability.
// A decorator must not make `odin models` or provider verification lose a
// capability that existed before usage reporting was attached.
func (p *AccountUsageProvider) Models(ctx context.Context) ([]string, error) {
	catalog, ok := p.Provider.(ModelCatalog)
	if !ok {
		return nil, ErrCatalogUnsupported
	}
	return catalog.Models(ctx)
}

func (p *AccountUsageProvider) requireTokens() error {
	if p.tokens == nil {
		return fmt.Errorf("%s usage has no credential source", p.provider)
	}
	return nil
}

func clampPercent(value float64) float64 {
	if math.IsNaN(value) {
		return 0
	}
	return math.Max(0, math.Min(100, value))
}
