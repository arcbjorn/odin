package model

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeProvider struct {
	name  string
	err   error
	calls int
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Complete(context.Context, Request) (*Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &Response{Text: "ok", Provider: f.name, StopReason: StopEndTurn}, nil
}

func quietChain(t *testing.T, providers ...Provider) *Chain {
	t.Helper()
	c, err := NewChain(ChainConfig{
		Providers: providers,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	return c
}

// The 2026-07-14 scenario: primary 403s "out of credits" on every turn and the
// first fallback must pick up the work immediately.
func TestChainFailsOverOnOutOfCredits(t *testing.T) {
	primary := &fakeProvider{name: "grok", err: &Error{Provider: "grok", Status: 403, Message: "out of credits"}}
	backup := &fakeProvider{name: "glm"}

	resp, err := quietChain(t, primary, backup).Complete(context.Background(), Request{})
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	if resp.Provider != "glm" {
		t.Fatalf("expected glm to serve, got %q", resp.Provider)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Fatalf("expected one call each, got primary=%d backup=%d", primary.calls, backup.calls)
	}
}

// A 400 is our own malformed request. Every provider rejects it identically,
// so the chain must surface it instead of laundering it through every leg.
func TestChainAbortsOnNonRetryable(t *testing.T) {
	primary := &fakeProvider{name: "grok", err: &Error{Provider: "grok", Status: 400, Message: "bad schema"}}
	backup := &fakeProvider{name: "glm"}

	if _, err := quietChain(t, primary, backup).Complete(context.Background(), Request{}); err == nil {
		t.Fatal("expected non-retryable error to abort the chain")
	}
	if backup.calls != 0 {
		t.Fatalf("backup should not be tried on a 400, got %d calls", backup.calls)
	}
}

// The chain must restart from the primary each call. Sticking on a fallback is
// how a recovered primary goes unused and daily work silently stays on a
// weaker model.
func TestChainRetriesPrimaryAfterCooldown(t *testing.T) {
	primary := &fakeProvider{name: "grok", err: &Error{Provider: "grok", Status: 503, Message: "down"}}
	backup := &fakeProvider{name: "glm"}

	c := quietChain(t, primary, backup)
	c.cooldown = time.Millisecond

	ctx := context.Background()
	if _, err := c.Complete(ctx, Request{}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	primary.err = nil // primary recovers
	time.Sleep(5 * time.Millisecond)

	resp, err := c.Complete(ctx, Request{})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if resp.Provider != "grok" {
		t.Fatalf("expected primary to be retried after cooldown, got %q", resp.Provider)
	}
}

// While a provider is cooling down it must be skipped, so a known-dead primary
// doesn't burn a full timeout on every turn.
func TestChainSkipsProviderInCooldown(t *testing.T) {
	primary := &fakeProvider{name: "grok", err: &Error{Provider: "grok", Status: 429, Message: "rate limited"}}
	backup := &fakeProvider{name: "glm"}

	c := quietChain(t, primary, backup)
	c.cooldown = time.Hour

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.Complete(ctx, Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if primary.calls != 1 {
		t.Fatalf("expected primary tried once then skipped, got %d calls", primary.calls)
	}
	if backup.calls != 3 {
		t.Fatalf("expected backup to serve all 3, got %d", backup.calls)
	}
	if got := c.Status()["grok"]; got == "ok" {
		t.Fatal("expected grok to report cooldown in Status()")
	}
}

func TestChainReportsAllFailures(t *testing.T) {
	a := &fakeProvider{name: "a", err: &Error{Provider: "a", Status: 500, Message: "boom"}}
	b := &fakeProvider{name: "b", err: &Error{Provider: "b", Status: 503, Message: "down"}}

	if _, err := quietChain(t, a, b).Complete(context.Background(), Request{}); err == nil {
		t.Fatal("expected error when every provider fails")
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := map[int]bool{
		400: false, // our bug
		401: true,  // expired token, another provider may work
		403: true,  // out of credits / region block
		429: true,
		500: true,
		503: true,
	}
	for status, want := range cases {
		if got := (&Error{Status: status}).Retryable(); got != want {
			t.Errorf("status %d: Retryable() = %v, want %v", status, got, want)
		}
	}
}
