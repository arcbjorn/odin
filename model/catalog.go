package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ModelCatalog is implemented by transports with a live model-list endpoint.
type ModelCatalog interface {
	Provider
	Models(ctx context.Context) ([]string, error)
}

// ErrCatalogUnsupported means the provider has no known model-list endpoint.
var ErrCatalogUnsupported = errors.New("model catalog is not supported")

func (o *OpenAI) Models(ctx context.Context) ([]string, error) {
	return fetchOpenAIModels(ctx, o.http, o.tokens, o.baseURL+"/models", func(req *http.Request, _ string) {
		for name, value := range o.headers {
			req.Header.Set(name, value)
		}
	})
}

func (r *Responses) Models(ctx context.Context) ([]string, error) {
	endpoint := r.baseURL + "/models"
	if r.codex {
		endpoint += "?client_version=1.0.0"
	}
	return fetchOpenAIModels(ctx, r.http, r.tokens, endpoint, func(req *http.Request, token string) {
		if r.codex {
			setCodexHeaders(req, token)
		}
	})
}

func (a *Anthropic) Models(ctx context.Context) ([]string, error) {
	if a.dropThinking && !a.oauthIdentity {
		return nil, ErrCatalogUnsupported
	}
	return fetchModels(ctx, a.http, a.tokens, a.baseURL+"/models", func(req *http.Request, token string) {
		if a.bearer {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set("x-api-key", token)
		}
		req.Header.Set("anthropic-version", anthropicVersion)
		if a.oauthIdentity {
			req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
			if a.userAgent != "" {
				req.Header.Set("User-Agent", a.userAgent)
			}
		}
	}, func(payload []byte) ([]string, error) {
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(body.Data))
		for _, item := range body.Data {
			if id := strings.TrimSpace(item.ID); id != "" {
				ids = append(ids, id)
			}
		}
		sort.SliceStable(ids, func(i, j int) bool {
			return anthropicModelRank(ids[i]) < anthropicModelRank(ids[j])
		})
		return dedupeModels(ids), nil
	})
}

func fetchOpenAIModels(
	ctx context.Context,
	client *http.Client,
	tokens TokenSource,
	endpoint string,
	decorate func(*http.Request, string),
) ([]string, error) {
	return fetchModels(ctx, client, tokens, endpoint, func(req *http.Request, token string) {
		req.Header.Set("Authorization", "Bearer "+token)
		if decorate != nil {
			decorate(req, token)
		}
	}, func(payload []byte) ([]string, error) {
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Models []struct {
				ID         string `json:"id"`
				Slug       string `json:"slug"`
				Visibility string `json:"visibility"`
				Priority   *int   `json:"priority"`
			} `json:"models"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(body.Data)+len(body.Models))
		for _, item := range body.Data {
			if id := strings.TrimSpace(item.ID); id != "" {
				ids = append(ids, id)
			}
		}
		if len(body.Models) > 0 {
			sort.SliceStable(body.Models, func(i, j int) bool {
				return catalogPriority(body.Models[i].Priority) < catalogPriority(body.Models[j].Priority)
			})
			for _, item := range body.Models {
				if visibility := strings.ToLower(strings.TrimSpace(item.Visibility)); visibility == "hide" || visibility == "hidden" {
					continue
				}
				id := strings.TrimSpace(item.Slug)
				if id == "" {
					id = strings.TrimSpace(item.ID)
				}
				if id != "" {
					ids = append(ids, id)
				}
			}
		} else {
			sort.Strings(ids)
		}
		return dedupeModels(ids), nil
	})
}

func catalogPriority(priority *int) int {
	if priority == nil {
		return 10_000
	}
	return *priority
}

func fetchModels(
	ctx context.Context,
	client *http.Client,
	tokens TokenSource,
	endpoint string,
	decorate func(*http.Request, string),
	decode func([]byte) ([]string, error),
) ([]string, error) {
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return nil, fmt.Errorf("invalid model catalog url: %w", err)
	}
	resp, err := doTokenRequest(ctx, client, tokens, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		decorate(req, token)
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model catalog: http %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}
	models, err := decode(payload)
	if err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	if len(models) == 0 {
		return nil, errors.New("model catalog returned no models")
	}
	return models, nil
}

func dedupeModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	return out
}

func anthropicModelRank(model string) string {
	rank := "3"
	switch {
	case strings.Contains(model, "opus"):
		rank = "0"
	case strings.Contains(model, "sonnet"):
		rank = "1"
	case strings.Contains(model, "haiku"):
		rank = "2"
	}
	return rank + model
}
