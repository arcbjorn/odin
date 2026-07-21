package model

import (
	"context"
	"net/http"
	"testing"
	"time"
)

type testPoolSource struct {
	token       string
	err         error
	replacement string
	recoveries  int
}

func (s *testPoolSource) Token(context.Context) (string, error) { return s.token, s.err }

func (s *testPoolSource) Recover(context.Context, int, string) (string, bool, error) {
	s.recoveries++
	if s.replacement == "" {
		return "", false, s.err
	}
	return s.replacement, true, nil
}

func TestTokenPoolRotatesAfterRateLimit(t *testing.T) {
	first := &testPoolSource{token: "first"}
	second := &testPoolSource{token: "second"}
	pool, err := NewTokenPool(TokenPoolConfig{
		Accounts: []AccountTokenSource{{Name: "one", Source: first}, {Name: "two", Source: second}},
		Cooldown: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 && req.Header.Get("Authorization") != "Bearer first" {
			t.Fatalf("first authorization = %q", req.Header.Get("Authorization"))
		}
		if requests == 2 && req.Header.Get("Authorization") != "Bearer second" {
			t.Fatalf("second authorization = %q", req.Header.Get("Authorization"))
		}
		status := http.StatusTooManyRequests
		if requests == 2 {
			status = http.StatusOK
		}
		return jsonResponse(status, `{}`), nil
	})}

	resp, err := doTokenRequest(context.Background(), client, pool, func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, "https://provider.test", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if token, err := pool.Token(context.Background()); err != nil || token != "second" {
		t.Fatalf("current token=%q err=%v", token, err)
	}
}

func TestTokenPoolRefreshesBeforeRotatingOnUnauthorized(t *testing.T) {
	first := &testPoolSource{token: "first", replacement: "refreshed"}
	second := &testPoolSource{token: "second"}
	pool, err := NewTokenPool(TokenPoolConfig{Accounts: []AccountTokenSource{
		{Name: "one", Source: first}, {Name: "two", Source: second},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if token, err := pool.Token(context.Background()); err != nil || token != "first" {
		t.Fatalf("initial token=%q err=%v", token, err)
	}
	token, retry, err := pool.Recover(context.Background(), http.StatusUnauthorized, "first")
	if err != nil || !retry || token != "refreshed" {
		t.Fatalf("recovery token=%q retry=%t err=%v", token, retry, err)
	}
	if first.recoveries != 1 {
		t.Fatalf("recoveries = %d", first.recoveries)
	}
}
