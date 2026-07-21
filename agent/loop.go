package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/arcbjorn/odin/model"
)

// Defaults for Config. Stopping with a clear blocker is safer than looping.
const (
	DefaultMaxTurns  = 20
	DefaultMaxTokens = 4096

	// defaultExactFailureLimit blocks a repeatedly failing call.
	defaultExactFailureLimit = 3

	// defaultToolFailureLimit halts the turn after this many failures of any
	// kind from one tool, catching a model that varies its arguments slightly
	// each time and so never trips the exact-repeat check.
	defaultToolFailureLimit = 8
)

// ErrGuardrail is returned when the loop stops because a tool call kept
// failing. The turn is abandoned and the blocker reported, rather than burning
// the remaining turns.
var ErrGuardrail = errors.New("tool loop guardrail tripped")

// Config configures a Loop.
type Config struct {
	Provider model.Provider
	Tools    *Registry
	Logger   *slog.Logger

	// System is the stable prompt prefix (SOUL + skills). Kept byte-identical
	// across turns so the provider's prompt cache hits.
	System string

	MaxTurns  int
	MaxTokens int
	Effort    string

	// ExactFailureLimit and ToolFailureLimit override the guardrail defaults.
	ExactFailureLimit int
	ToolFailureLimit  int
}

// Loop runs the tool-use conversation.
type Loop struct {
	cfg Config
	log *slog.Logger
}

// New builds a Loop, applying defaults.
func New(cfg Config) (*Loop, error) {
	if cfg.Provider == nil {
		return nil, errors.New("loop needs a provider")
	}
	if cfg.Tools == nil {
		cfg.Tools = NewRegistry()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	if cfg.ExactFailureLimit <= 0 {
		cfg.ExactFailureLimit = defaultExactFailureLimit
	}
	if cfg.ToolFailureLimit <= 0 {
		cfg.ToolFailureLimit = defaultToolFailureLimit
	}
	return &Loop{cfg: cfg, log: cfg.Logger}, nil
}

// Result is the outcome of one Run.
type Result struct {
	Text      string
	Messages  []model.Message
	Turns     int
	Usage     model.Usage
	RateLimit *model.RateLimit
	ToolCalls int

	// Provider and Model record who actually served the final turn — the
	// fallback chain may have moved off the primary mid-conversation.
	Provider string
	Model    string
}

// Run drives the conversation until the model stops calling tools, the turn
// budget is exhausted, or a guardrail trips.
func (l *Loop) Run(ctx context.Context, history []model.Message) (*Result, error) {
	messages := append([]model.Message(nil), history...)
	defs := l.cfg.Tools.Defs()
	guard := newGuardrail(l.cfg.ExactFailureLimit, l.cfg.ToolFailureLimit)

	result := &Result{}

	for turn := 1; turn <= l.cfg.MaxTurns; turn++ {
		result.Turns = turn

		resp, err := l.cfg.Provider.Complete(ctx, model.Request{
			System:    l.cfg.System,
			Messages:  messages,
			Tools:     defs,
			MaxTokens: l.cfg.MaxTokens,
			Effort:    l.cfg.Effort,
		})
		if err != nil {
			return result, fmt.Errorf("turn %d: %w", turn, err)
		}

		result.Usage.Input += resp.Usage.Input
		result.Usage.Output += resp.Usage.Output
		result.Usage.Cached += resp.Usage.Cached
		result.RateLimit = resp.RateLimit
		result.Provider = resp.Provider
		result.Model = resp.Model

		messages = append(messages, model.Message{
			Role:          model.RoleAssistant,
			Content:       resp.Text,
			ToolCalls:     resp.ToolCalls,
			ProviderState: resp.ProviderState,
		})

		if len(resp.ToolCalls) == 0 {
			result.Text = resp.Text
			result.Messages = messages
			if resp.StopReason == model.StopLength {
				// Truncated mid-thought. Surface it rather than passing off a
				// half-written answer as complete.
				l.log.Warn("response hit max_tokens", "turn", turn, "max_tokens", l.cfg.MaxTokens)
			}
			return result, nil
		}

		for _, call := range resp.ToolCalls {
			result.ToolCalls++

			if blocked, reason := guard.check(call); blocked {
				l.log.Error("guardrail blocked repeated failing tool call",
					"tool", call.Name, "reason", reason, "turn", turn)
				result.Text = fmt.Sprintf("Stopped: %s. Last error from %s: %s",
					reason, call.Name, guard.lastError(call.Name))
				result.Messages = messages
				return result, fmt.Errorf("%w: %s", ErrGuardrail, reason)
			}

			output, err := l.invoke(ctx, call)
			if err != nil {
				guard.recordFailure(call, err)
				l.log.Warn("tool call failed", "tool", call.Name, "turn", turn, "error", err)
				// Feed the error back so the model can correct itself. The
				// guardrail bounds how many times that can go wrong.
				output = "Error: " + err.Error()
			} else {
				guard.recordSuccess(call)
			}

			messages = append(messages, model.Message{
				Role:       model.RoleTool,
				Content:    output,
				ToolCallID: call.ID,
				Name:       call.Name,
			})
		}
	}

	result.Messages = messages
	result.Text = "Stopped: reached the maximum number of turns without finishing."
	return result, fmt.Errorf("exceeded max turns (%d)", l.cfg.MaxTurns)
}

// invoke runs one tool, converting a panic in a handler into an error so one
// bad tool cannot take down the gateway.
func (l *Loop) invoke(ctx context.Context, call model.ToolCall) (output string, err error) {
	tool, ok := l.cfg.Tools.Lookup(call.Name)
	if !ok {
		// Name the available tools: a model that hallucinated a tool can
		// usually recover from this on the next turn.
		return "", fmt.Errorf("unknown tool %q; available: %v", call.Name, l.cfg.Tools.Names())
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool %q panicked: %v", call.Name, r)
		}
	}()

	return tool.Handle(ctx, call.Input)
}

// guardrail is the tool-loop circuit breaker.
type guardrail struct {
	exactLimit int
	toolLimit  int

	exactFailures map[string]int    // hash(name+input) -> consecutive failures
	toolFailures  map[string]int    // tool name -> total failures
	lastErrors    map[string]string // tool name -> most recent error text
}

func newGuardrail(exactLimit, toolLimit int) *guardrail {
	return &guardrail{
		exactLimit:    exactLimit,
		toolLimit:     toolLimit,
		exactFailures: make(map[string]int),
		toolFailures:  make(map[string]int),
		lastErrors:    make(map[string]string),
	}
}

// check reports whether this call should be blocked before running.
func (g *guardrail) check(call model.ToolCall) (bool, string) {
	if n := g.exactFailures[callKey(call)]; n >= g.exactLimit {
		return true, fmt.Sprintf("the same %s call failed %d times", call.Name, n)
	}
	if n := g.toolFailures[call.Name]; n >= g.toolLimit {
		return true, fmt.Sprintf("%s failed %d times", call.Name, n)
	}
	return false, ""
}

func (g *guardrail) recordFailure(call model.ToolCall, err error) {
	g.exactFailures[callKey(call)]++
	g.toolFailures[call.Name]++
	g.lastErrors[call.Name] = err.Error()
}

// recordSuccess clears the exact-repeat counter for this call. The per-tool
// counter is intentionally not cleared: a tool that fails intermittently
// across a long turn should still eventually halt.
func (g *guardrail) recordSuccess(call model.ToolCall) {
	delete(g.exactFailures, callKey(call))
}

func (g *guardrail) lastError(name string) string {
	if e, ok := g.lastErrors[name]; ok {
		return e
	}
	return "(none recorded)"
}

// callKey identifies a (name, input) pair. Inputs can be large, so hash rather
// than retaining every argument string for the life of the turn.
func callKey(call model.ToolCall) string {
	h := sha256.New()
	h.Write([]byte(call.Name))
	h.Write([]byte{0})
	h.Write(normalizeJSON(call.Input))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeJSON re-marshals so semantically identical inputs that differ only
// in key order or whitespace hash the same. Falls back to raw bytes if the
// input is not valid JSON.
func normalizeJSON(raw json.RawMessage) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v) // Go sorts map keys on marshal
	if err != nil {
		return raw
	}
	return out
}
