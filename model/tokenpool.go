package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// AccountTokenSource is one named credential in a provider account pool.
type AccountTokenSource struct {
	Name   string
	Source TokenSource
}

// TokenPool rotates independent provider accounts when one is rejected or
// reaches an account-scoped quota. Tokens are hashed in the ownership map so
// raw credentials are not retained as map keys.
type TokenPool struct {
	accounts []AccountTokenSource
	cooldown time.Duration

	mu          sync.Mutex
	current     int
	cooling     map[int]time.Time
	tokenOwners map[[32]byte]int
}

// TokenPoolConfig configures a multi-account token source.
type TokenPoolConfig struct {
	Accounts []AccountTokenSource
	Cooldown time.Duration
}

// NewTokenPool builds a pool in selection order.
func NewTokenPool(cfg TokenPoolConfig) (*TokenPool, error) {
	if len(cfg.Accounts) < 2 {
		return nil, errors.New("token pool needs at least two accounts")
	}
	seen := make(map[string]bool, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		if account.Name == "" || account.Source == nil {
			return nil, errors.New("token pool accounts need a name and source")
		}
		if seen[account.Name] {
			return nil, fmt.Errorf("duplicate token pool account %q", account.Name)
		}
		seen[account.Name] = true
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	return &TokenPool{
		accounts: append([]AccountTokenSource(nil), cfg.Accounts...), cooldown: cooldown,
		cooling: make(map[int]time.Time), tokenOwners: make(map[[32]byte]int),
	}, nil
}

// Token returns a credential from the current healthy account, advancing past
// accounts whose local token source cannot produce a credential.
func (p *TokenPool) Token(ctx context.Context) (string, error) {
	indices := p.availableIndices()
	var errs []error
	for _, index := range indices {
		token, err := p.accounts[index].Source.Token(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.accounts[index].Name, err))
			p.cool(index)
			continue
		}
		p.remember(token, index)
		return token, nil
	}
	if len(errs) == 0 {
		return "", errors.New("all token pool accounts are cooling down")
	}
	return "", fmt.Errorf("all token pool accounts failed: %w", errors.Join(errs...))
}

// Recover refreshes the rejected account on 401 when possible, then rotates
// to another account for auth, billing, quota, and rate-limit failures.
func (p *TokenPool) Recover(ctx context.Context, status int, rejectedToken string) (string, bool, error) {
	if !poolRotatableStatus(status) {
		return "", false, nil
	}
	index := p.owner(rejectedToken)
	if status == http.StatusUnauthorized {
		if source, ok := p.accounts[index].Source.(RecoverableTokenSource); ok {
			replacement, retry, err := source.Recover(ctx, status, rejectedToken)
			if err == nil && retry {
				p.remember(replacement, index)
				return replacement, true, nil
			}
		}
	}

	p.cool(index)
	indices := p.availableIndices()
	for _, next := range indices {
		if next == index {
			continue
		}
		token, err := p.accounts[next].Source.Token(ctx)
		if err != nil {
			p.cool(next)
			continue
		}
		p.remember(token, next)
		return token, true, nil
	}
	// Preserve the provider's original status/body when no replacement exists;
	// callers use that status to decide whether to fail over providers.
	return "", false, nil
}

func poolRotatableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func (p *TokenPool) owner(token string) int {
	digest := sha256.Sum256([]byte(token))
	p.mu.Lock()
	defer p.mu.Unlock()
	if index, ok := p.tokenOwners[digest]; ok {
		return index
	}
	return p.current
}

func (p *TokenPool) remember(token string, index int) {
	digest := sha256.Sum256([]byte(token))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = index
	p.tokenOwners[digest] = index
}

func (p *TokenPool) cool(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooling[index] = time.Now().Add(p.cooldown)
	if p.current == index {
		p.current = (index + 1) % len(p.accounts)
	}
}

func (p *TokenPool) availableIndices() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]int, 0, len(p.accounts))
	for offset := 0; offset < len(p.accounts); offset++ {
		index := (p.current + offset) % len(p.accounts)
		until, cooling := p.cooling[index]
		if cooling && now.Before(until) {
			continue
		}
		delete(p.cooling, index)
		out = append(out, index)
	}
	return out
}
