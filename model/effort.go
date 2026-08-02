package model

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
)

// effortState decides the reasoning hint a transport actually sends.
//
// Three inputs, in order: a provider that has switched the hint off entirely
// (drop_effort), the provider's own configured level, and the profile-wide
// default carried on the request. The per-provider level exists because one
// value cannot fit a chain whose members are different model families — and
// because /model can now move a conversation onto a model the profile default
// was never chosen for.
//
// rejected is the safety net for exactly that case. A model that answers a
// reasoning hint with HTTP 400 would otherwise fail every turn: the chain
// treats 400 as a malformed request of our own making and aborts instead of
// falling back, which is right in general and fatal here. So the first
// rejection latches the hint off for this transport and the call is retried
// once without it.
type effortState struct {
	provider string
	// configured is the per-provider level from config.toml. Empty defers to
	// the profile-wide default on the request.
	configured string
	// disabled is drop_effort: never send a hint, do not probe for one.
	disabled bool
	log      *slog.Logger

	rejected atomic.Bool
}

func newEffortState(provider, configured string, disabled bool, log *slog.Logger) effortState {
	if log == nil {
		log = slog.Default()
	}
	return effortState{provider: provider, configured: configured, disabled: disabled, log: log}
}

// resolve returns the level to send, given the request's default.
func (e *effortState) resolve(requested string) string {
	if e.disabled || e.rejected.Load() {
		return ""
	}
	if e.configured != "" {
		return e.configured
	}
	return requested
}

// retryWithout reports whether err is this model refusing the reasoning hint,
// latching it off so the retry — and every later turn — omits it.
func (e *effortState) retryWithout(requested string, err error) bool {
	if e.resolve(requested) == "" || !effortRejected(err) {
		return false
	}
	if e.rejected.Swap(true) {
		return false // another turn already latched it; do not retry twice
	}
	e.log.Warn("model rejected the reasoning effort hint; dropping it for this provider",
		"provider", e.provider, "error", err)
	return true
}

// effortRejected reports whether a failure is a provider refusing the
// reasoning hint rather than a genuinely malformed request. Only a 400
// qualifies: any other status is either retryable elsewhere or unrelated.
func effortRejected(err error) bool {
	var perr *Error
	if !errors.As(err, &perr) || perr.Status != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(perr.Message)
	for _, marker := range []string{"reasoning_effort", "reasoning effort", "effort", "thinking", "reasoning"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
