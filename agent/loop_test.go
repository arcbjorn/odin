package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/arcbjorn/odin/model"
)

// scriptedProvider replays a fixed sequence of responses, then repeats the
// last one forever — which is what a model stuck in a loop actually does.
type scriptedProvider struct {
	responses []*model.Response
	requests  []model.Request
	calls     int
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Complete(_ context.Context, req model.Request) (*model.Response, error) {
	s.calls++
	s.requests = append(s.requests, req)
	if s.calls <= len(s.responses) {
		return s.responses[s.calls-1], nil
	}
	return s.responses[len(s.responses)-1], nil
}

func textResponse(text string) *model.Response {
	return &model.Response{Text: text, StopReason: model.StopEndTurn}
}

func toolResponse(id, name, input string) *model.Response {
	return &model.Response{
		StopReason: model.StopToolUse,
		ToolCalls:  []model.ToolCall{{ID: id, Name: name, Input: json.RawMessage(input)}},
	}
}

func quietLoop(t *testing.T, cfg Config) *Loop {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	l, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

func echoTool(name string, fn Handler) Tool {
	return Tool{
		Def: model.Tool{
			Name:        name,
			Description: "test tool",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
				"required":   []string{"q"},
			},
		},
		Handle: fn,
	}
}

func userMsg(text string) []model.Message {
	return []model.Message{{Role: model.RoleUser, Content: text}}
}

func TestLoopReturnsPlainAnswer(t *testing.T) {
	p := &scriptedProvider{responses: []*model.Response{textResponse("task saved")}}
	l := quietLoop(t, Config{Provider: p, Tools: NewRegistry()})

	res, err := l.Run(context.Background(), userMsg("save my task"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "task saved" {
		t.Fatalf("got %q", res.Text)
	}
	if res.Turns != 1 {
		t.Fatalf("expected 1 turn, got %d", res.Turns)
	}
}

func TestLoopExecutesToolAndContinues(t *testing.T) {
	reg := NewRegistry()
	var gotInput string
	reg.MustRegister(echoTool("sqlite", func(_ context.Context, in json.RawMessage) (string, error) {
		gotInput = string(in)
		return "1 row", nil
	}))

	p := &scriptedProvider{responses: []*model.Response{
		toolResponse("c1", "sqlite", `{"q":"select 1"}`),
		textResponse("done"),
	}}
	l := quietLoop(t, Config{Provider: p, Tools: reg})

	res, err := l.Run(context.Background(), userMsg("query"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotInput != `{"q":"select 1"}` {
		t.Fatalf("handler got %q", gotInput)
	}
	if res.Text != "done" || res.ToolCalls != 1 {
		t.Fatalf("text=%q toolCalls=%d", res.Text, res.ToolCalls)
	}
	// user + assistant(tool_use) + tool result + assistant(final)
	if len(res.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(res.Messages))
	}
}

func TestLoopPreservesProviderState(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(echoTool("sqlite", func(context.Context, json.RawMessage) (string, error) {
		return "1 row", nil
	}))
	state := &model.ProviderState{Provider: "codex", Kind: "responses_output", Data: json.RawMessage(`[{"type":"reasoning"}]`)}
	first := toolResponse("c1", "sqlite", `{"q":"select 1"}`)
	first.ProviderState = state
	p := &scriptedProvider{responses: []*model.Response{first, textResponse("done")}}
	l := quietLoop(t, Config{Provider: p, Tools: reg})

	if _, err := l.Run(context.Background(), userMsg("query")); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 || len(p.requests[1].Messages) < 2 {
		t.Fatalf("requests = %#v", p.requests)
	}
	if got := p.requests[1].Messages[1].ProviderState; got != state {
		t.Fatalf("provider state = %#v, want %#v", got, state)
	}
}

// A model that repeats the same malformed call must hit the exact limit.
func TestGuardrailStopsRepeatedIdenticalFailure(t *testing.T) {
	reg := NewRegistry()
	attempts := 0
	reg.MustRegister(echoTool("cronjob", func(context.Context, json.RawMessage) (string, error) {
		attempts++
		return "", errors.New("missing required field: schedule")
	}))

	// Model repeats the identical malformed call forever.
	p := &scriptedProvider{responses: []*model.Response{
		toolResponse("c1", "cronjob", `{"action":"create"}`),
	}}
	l := quietLoop(t, Config{Provider: p, Tools: reg, MaxTurns: 200, ExactFailureLimit: 3})

	res, err := l.Run(context.Background(), userMsg("remind me"))
	if !errors.Is(err, ErrGuardrail) {
		t.Fatalf("expected ErrGuardrail, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts before block, got %d", attempts)
	}
	if res.Turns > 5 {
		t.Fatalf("guardrail let the loop run %d turns", res.Turns)
	}
	// The user must be told what blocked, not handed silence.
	if res.Text == "" {
		t.Fatal("expected a blocker message in Result.Text")
	}
}

// A model that varies its arguments slightly each time never trips the
// exact-repeat check, so the per-tool limit has to catch it.
func TestGuardrailStopsVaryingFailures(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(echoTool("sqlite", func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("syntax error")
	}))

	responses := make([]*model.Response, 0, 30)
	for i := 0; i < 30; i++ {
		responses = append(responses, toolResponse(
			"c", "sqlite", `{"q":"select `+string(rune('a'+i))+`"}`))
	}
	p := &scriptedProvider{responses: responses}
	l := quietLoop(t, Config{Provider: p, Tools: reg, MaxTurns: 200, ToolFailureLimit: 8})

	res, err := l.Run(context.Background(), userMsg("query"))
	if !errors.Is(err, ErrGuardrail) {
		t.Fatalf("expected ErrGuardrail, got %v", err)
	}
	if res.ToolCalls > 10 {
		t.Fatalf("expected halt near the limit, got %d tool calls", res.ToolCalls)
	}
}

// A tool that fails once then succeeds must not be penalized — transient
// errors are normal and the counter has to reset.
func TestGuardrailResetsAfterSuccess(t *testing.T) {
	reg := NewRegistry()
	calls := 0
	reg.MustRegister(echoTool("web", func(context.Context, json.RawMessage) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("timeout")
		}
		return "ok", nil
	}))

	p := &scriptedProvider{responses: []*model.Response{
		toolResponse("c1", "web", `{"q":"x"}`),
		toolResponse("c2", "web", `{"q":"x"}`),
		textResponse("fetched"),
	}}
	l := quietLoop(t, Config{Provider: p, Tools: reg})

	res, err := l.Run(context.Background(), userMsg("fetch"))
	if err != nil {
		t.Fatalf("transient failure should not trip the guardrail: %v", err)
	}
	if res.Text != "fetched" {
		t.Fatalf("got %q", res.Text)
	}
}

// Key order and whitespace must not defeat the exact-repeat check.
func TestGuardrailNormalizesJSONKeyOrder(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(echoTool("t", func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("nope")
	}))

	p := &scriptedProvider{responses: []*model.Response{
		toolResponse("c1", "t", `{"a":1,"q":"x"}`),
		toolResponse("c2", "t", `{"q":"x","a":1}`),
		toolResponse("c3", "t", `{ "a" : 1 , "q" : "x" }`),
		toolResponse("c4", "t", `{"a":1,"q":"x"}`),
	}}
	l := quietLoop(t, Config{Provider: p, Tools: reg, MaxTurns: 50, ExactFailureLimit: 3})

	if _, err := l.Run(context.Background(), userMsg("go")); !errors.Is(err, ErrGuardrail) {
		t.Fatalf("reordered keys should hash identically: %v", err)
	}
}

func TestUnknownToolIsRecoverable(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(echoTool("sqlite", func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}))

	p := &scriptedProvider{responses: []*model.Response{
		toolResponse("c1", "hallucinated", `{}`),
		textResponse("recovered"),
	}}
	l := quietLoop(t, Config{Provider: p, Tools: reg})

	res, err := l.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("unknown tool should be recoverable: %v", err)
	}
	if res.Text != "recovered" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestPanicInHandlerBecomesError(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(echoTool("bad", func(context.Context, json.RawMessage) (string, error) {
		panic("boom")
	}))

	p := &scriptedProvider{responses: []*model.Response{
		toolResponse("c1", "bad", `{}`),
		textResponse("survived"),
	}}
	l := quietLoop(t, Config{Provider: p, Tools: reg})

	res, err := l.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("panic should not escape the loop: %v", err)
	}
	if res.Text != "survived" {
		t.Fatalf("got %q", res.Text)
	}
}

func TestMaxTurnsBounded(t *testing.T) {
	reg := NewRegistry()
	n := 0
	// Succeeds every time, so no guardrail fires — only MaxTurns can stop it.
	reg.MustRegister(echoTool("t", func(context.Context, json.RawMessage) (string, error) {
		n++
		return "ok", nil
	}))

	responses := make([]*model.Response, 0, 40)
	for i := 0; i < 40; i++ {
		responses = append(responses, toolResponse("c", "t", `{"q":"`+string(rune('a'+i%26))+`"}`))
	}
	p := &scriptedProvider{responses: responses}
	l := quietLoop(t, Config{Provider: p, Tools: reg, MaxTurns: 5})

	res, err := l.Run(context.Background(), userMsg("go"))
	if err == nil {
		t.Fatal("expected max-turns error")
	}
	if res.Turns != 5 {
		t.Fatalf("expected 5 turns, got %d", res.Turns)
	}
}

// The tool list renders at the front of the prompt; unstable ordering silently
// breaks the prompt cache on every request.
func TestDefsAreSortedForCacheStability(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"web", "sqlite", "file", "skills"} {
		reg.MustRegister(echoTool(name, func(context.Context, json.RawMessage) (string, error) {
			return "", nil
		}))
	}
	for i := 0; i < 5; i++ {
		defs := reg.Defs()
		want := []string{"file", "skills", "sqlite", "web"}
		for j, d := range defs {
			if d.Name != want[j] {
				t.Fatalf("run %d: defs[%d]=%q, want %q", i, j, d.Name, want[j])
			}
		}
	}
}

// Oversized schemas are the root cause of the 162-call incident: required
// fields buried under verbose trailing optionals.
func TestRegistryRejectsOversizedSchema(t *testing.T) {
	reg := NewRegistry()
	props := map[string]any{}
	for i := 0; i < 15; i++ {
		props[string(rune('a'+i))] = map[string]any{"type": "string"}
	}
	err := reg.Register(Tool{
		Def: model.Tool{
			Name:   "cronjob",
			Schema: map[string]any{"type": "object", "properties": props},
		},
		Handle: func(context.Context, json.RawMessage) (string, error) { return "", nil },
	})
	if err == nil {
		t.Fatal("expected a 15-property tool schema to be rejected")
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	reg := NewRegistry()
	tool := echoTool("t", func(context.Context, json.RawMessage) (string, error) { return "", nil })
	if err := reg.Register(tool); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := reg.Register(tool); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}
}

func TestUsageAccumulatesAcrossTurns(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(echoTool("t", func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}))

	withUsage := func(r *model.Response, in, out, cached int) *model.Response {
		r.Usage = model.Usage{Input: in, Output: out, Cached: cached}
		return r
	}
	p := &scriptedProvider{responses: []*model.Response{
		withUsage(toolResponse("c1", "t", `{"q":"x"}`), 100, 20, 0),
		withUsage(textResponse("done"), 150, 30, 90),
	}}
	p.responses[1].RateLimit = &model.RateLimit{Provider: "fake", RequestsMin: model.RateLimitBucket{Limit: 100, Remaining: 75}}
	l := quietLoop(t, Config{Provider: p, Tools: reg})

	res, err := l.Run(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage.Input != 250 || res.Usage.Output != 50 || res.Usage.Cached != 90 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if res.RateLimit == nil || res.RateLimit.RequestsMin.Remaining != 75 {
		t.Fatalf("rate limit = %+v", res.RateLimit)
	}
}
