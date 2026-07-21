package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Chain tries providers in order until one succeeds.
//
// It always restarts from the primary so recovered providers are retried.
//
// Unhealthy providers are skipped for a cooldown so a known-dead primary
// doesn't burn a timeout on every single turn, but the cooldown expires and
// the primary is retried on its own.
type Chain struct {
	providers []Provider
	log       *slog.Logger
	cooldown  time.Duration

	mu     sync.Mutex
	health map[string]time.Time // provider name -> skip-until
}

// ChainConfig configures a fallback chain.
type ChainConfig struct {
	Providers []Provider
	Logger    *slog.Logger
	// Cooldown is how long to skip a provider after a retryable failure.
	// Defaults to 5 minutes.
	Cooldown time.Duration
}

// NewChain builds a fallback chain. Order matters: providers[0] is the primary.
func NewChain(cfg ChainConfig) (*Chain, error) {
	if len(cfg.Providers) == 0 {
		return nil, errors.New("chain needs at least one provider")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	cooldown := cfg.Cooldown
	if cooldown == 0 {
		cooldown = 5 * time.Minute
	}
	return &Chain{
		providers: cfg.Providers,
		log:       log,
		cooldown:  cooldown,
		health:    make(map[string]time.Time),
	}, nil
}

func (c *Chain) Name() string {
	names := make([]string, len(c.providers))
	for i, p := range c.providers {
		names[i] = p.Name()
	}
	return strings.Join(names, " -> ")
}

// Providers returns the configured providers in fallback order.
func (c *Chain) Providers() []Provider {
	return append([]Provider(nil), c.providers...)
}

// Complete tries each provider in order, starting from the primary every call.
func (c *Chain) Complete(ctx context.Context, req Request) (*Response, error) {
	var errs []error

	for i, p := range c.providers {
		if c.cooling(p.Name()) {
			c.log.Debug("skipping provider in cooldown", "provider", p.Name())
			continue
		}

		resp, err := p.Complete(ctx, req)
		if err == nil {
			if i > 0 {
				// Surface when work is served by a fallback.
				c.log.Warn("served by fallback provider",
					"provider", p.Name(), "position", i, "primary", c.providers[0].Name())
			}
			c.markHealthy(p.Name())
			return resp, nil
		}

		errs = append(errs, err)

		// The caller gave up (timeout, shutdown). Don't burn the rest of the
		// chain on a context that's already dead.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context ended during %s: %w", p.Name(), ctx.Err())
		}

		var perr *Error
		if errors.As(err, &perr) && !perr.Retryable() {
			// A 400 is our own malformed request; every provider will reject it
			// identically. Fail loudly rather than laundering a bug through the
			// whole chain and reporting the last provider's error.
			c.log.Error("non-retryable provider error, aborting chain",
				"provider", p.Name(), "status", perr.Status, "message", perr.Message)
			return nil, err
		}

		c.markUnhealthy(p.Name())
		c.log.Warn("provider failed, trying next", "provider", p.Name(), "error", err)
	}

	return nil, fmt.Errorf("all %d providers failed: %w", len(c.providers), errors.Join(errs...))
}

func (c *Chain) cooling(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.health[name]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(c.health, name)
		return false
	}
	return true
}

func (c *Chain) markUnhealthy(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health[name] = time.Now().Add(c.cooldown)
}

func (c *Chain) markHealthy(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.health, name)
}

// Status reports provider cooldowns for `odin status`.
func (c *Chain) Status() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.providers))
	now := time.Now()
	for _, p := range c.providers {
		if until, ok := c.health[p.Name()]; ok && now.Before(until) {
			out[p.Name()] = fmt.Sprintf("cooldown %s", until.Sub(now).Round(time.Second))
			continue
		}
		out[p.Name()] = "ok"
	}
	return out
}
