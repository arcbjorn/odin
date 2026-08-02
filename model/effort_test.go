package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// effortServer records the reasoning_effort of every chat/completions request
// and can reject the first N with the 400 a model that cannot reason returns.
type effortServer struct {
	mu       sync.Mutex
	efforts  []string
	reject   int
	rejected int
}

func (e *effortServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		payload, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(payload, &body)

		e.mu.Lock()
		e.efforts = append(e.efforts, body.ReasoningEffort)
		reject := e.rejected < e.reject
		if reject {
			e.rejected++
		}
		e.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if reject {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'reasoning_effort' is not supported with this model."}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *effortServer) sent() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.efforts...)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestProviderEffortOverridesTheProfileDefault(t *testing.T) {
	stub := &effortServer{}
	srv := stub.start(t)

	p := NewOpenAI(OpenAIConfig{
		Provider: "primary", Model: "m1", BaseURL: srv.URL,
		Tokens: StaticToken("k"), Effort: "low", Logger: quietLog(),
	})
	if _, err := p.Complete(context.Background(), Request{Effort: "high"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := stub.sent(); len(got) != 1 || got[0] != "low" {
		t.Fatalf("sent effort %v, want the provider's own level to win", got)
	}
}

func TestRequestEffortUsedWhenProviderHasNoOverride(t *testing.T) {
	stub := &effortServer{}
	srv := stub.start(t)

	p := NewOpenAI(OpenAIConfig{
		Provider: "primary", Model: "m1", BaseURL: srv.URL,
		Tokens: StaticToken("k"), Logger: quietLog(),
	})
	if _, err := p.Complete(context.Background(), Request{Effort: "high"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := stub.sent(); len(got) != 1 || got[0] != "high" {
		t.Fatalf("sent effort %v, want the profile default", got)
	}
}

func TestDropEffortSendsNothing(t *testing.T) {
	stub := &effortServer{}
	srv := stub.start(t)

	p := NewOpenAI(OpenAIConfig{
		Provider: "primary", Model: "m1", BaseURL: srv.URL,
		Tokens: StaticToken("k"), DropEffort: true, Logger: quietLog(),
	})
	if _, err := p.Complete(context.Background(), Request{Effort: "high"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := stub.sent(); len(got) != 1 || got[0] != "" {
		t.Fatalf("sent effort %v, want none", got)
	}
}

// The failure mode /model introduces: switching onto a model that rejects the
// reasoning hint used to 400 every turn, and the chain treats 400 as fatal
// rather than falling back. The transport must recover on its own.
func TestRejectedEffortIsDroppedAndRetried(t *testing.T) {
	stub := &effortServer{reject: 1}
	srv := stub.start(t)

	p := NewOpenAI(OpenAIConfig{
		Provider: "primary", Model: "m1", BaseURL: srv.URL,
		Tokens: StaticToken("k"), Logger: quietLog(),
	})
	resp, err := p.Complete(context.Background(), Request{Effort: "high"})
	if err != nil {
		t.Fatalf("a rejected effort hint must not fail the turn: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("text = %q", resp.Text)
	}
	got := stub.sent()
	if len(got) != 2 {
		t.Fatalf("requests = %v, want the call retried once", got)
	}
	if got[0] != "high" || got[1] != "" {
		t.Fatalf("requests = %v, want the retry to omit the hint", got)
	}
}

// Once a model has rejected the hint, later turns must not pay for the probe
// again — one wasted round trip per process, not per turn.
func TestRejectedEffortLatchesOff(t *testing.T) {
	stub := &effortServer{reject: 1}
	srv := stub.start(t)

	p := NewOpenAI(OpenAIConfig{
		Provider: "primary", Model: "m1", BaseURL: srv.URL,
		Tokens: StaticToken("k"), Logger: quietLog(),
	})
	for i := 0; i < 3; i++ {
		if _, err := p.Complete(context.Background(), Request{Effort: "high"}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	got := stub.sent()
	// turn 1: "high" (rejected) + "" (retry); turns 2 and 3: "" only.
	if len(got) != 4 {
		t.Fatalf("requests = %v, want 4", got)
	}
	for _, effort := range got[1:] {
		if effort != "" {
			t.Fatalf("requests = %v, want the hint dropped after the first rejection", got)
		}
	}
}

// A 400 that has nothing to do with reasoning must stay fatal — laundering a
// genuinely malformed request through a retry would hide our own bug.
func TestUnrelatedBadRequestIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"messages: field required"}}`)
	}))
	t.Cleanup(srv.Close)

	p := NewOpenAI(OpenAIConfig{
		Provider: "primary", Model: "m1", BaseURL: srv.URL,
		Tokens: StaticToken("k"), Logger: quietLog(),
	})
	_, err := p.Complete(context.Background(), Request{Effort: "high"})
	if err == nil {
		t.Fatal("an unrelated 400 must still fail")
	}
	var perr *Error
	if !errors.As(err, &perr) || perr.Status != http.StatusBadRequest {
		t.Fatalf("want a 400 provider error, got %v", err)
	}
}

func TestEffortRejectedDetection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"reasoning_effort 400", &Error{Status: 400, Message: "Unsupported parameter: 'reasoning_effort'"}, true},
		{"thinking 400", &Error{Status: 400, Message: "thinking is not supported"}, true},
		{"unrelated 400", &Error{Status: 400, Message: "messages: field required"}, false},
		{"429 mentioning effort", &Error{Status: 429, Message: "effort quota exceeded"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effortRejected(tc.err); got != tc.want {
				t.Fatalf("effortRejected = %v, want %v", got, tc.want)
			}
		})
	}
}
