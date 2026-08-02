package model

import (
	"context"
	"sync"
)

// ProviderModels is one configured provider's switchable models, as offered to
// a user picking a target. Err is set when the provider has no live catalog
// endpoint or the fetch failed — the provider is still listed, because its
// configured model remains a valid target either way.
type ProviderModels struct {
	Provider string
	// Configured is the model from config.toml, always a valid target.
	Configured string
	// Models is the live catalog, empty when Err is set.
	Models []string
	Err    string
}

// SwitchScope decides whether a switch outlives the process that made it.
type SwitchScope int

const (
	// SwitchPersistent writes the choice to the profile's runtime state, so a
	// restart comes back on it rather than silently reverting.
	SwitchPersistent SwitchScope = iota
	// SwitchOnce applies to this process only and leaves the stored choice
	// alone. Hermes defaults its in-session switches to this on the grounds
	// that they are usually exploratory; Odin makes it the explicit option
	// instead, because it targets a daemon rather than a shell session.
	SwitchOnce
)

// Selection is the active model choice, as reported to whoever asks.
type Selection struct {
	// Target is "provider/model".
	Target string
	// Overridden reports that the target differs from config.toml.
	Overridden bool
	// Transient reports a SwitchOnce choice: a restart returns to the stored
	// selection, or to config.toml if there is none.
	Transient bool
}

// VerifyResult is a live protocol check against a switch target: can this
// model actually be reached, and can it run the tool exchange the agent
// depends on.
type VerifyResult struct {
	Target         string
	CatalogChecked bool
	ToolCall       bool
	Continuation   bool
	// Switched reports whether the check was followed by moving onto it.
	Switched bool
}

// SwitchChange records one applied model switch, for reporting back to
// whoever asked for it.
type SwitchChange struct {
	// Transient mirrors SwitchOnce: the choice dies with this process.
	Transient bool

	// Target and Previous are "provider/model".
	Target   string
	Previous string
	// ProviderChanged distinguishes a different model on the same endpoint
	// from a move to another provider, which also drops the fallback chain.
	ProviderChanged bool
	// ResolvedVia names how free-form input was matched, so an unexpected
	// resolution is visible rather than silent.
	ResolvedVia string
	// Warning carries a non-fatal problem with an otherwise applied switch,
	// such as a target that could not be persisted.
	Warning string
}

// Router is a Provider whose target can be replaced while the agent runs, so
// an interactive session can switch models mid-conversation without
// rebuilding the loop or dropping history.
//
// The rest of the runtime keeps holding the Router, so a switch is one
// pointer swap instead of a re-wiring of the agent. Base is the chain
// assembled from config.toml and is what Reset restores; it is never mutated,
// so the configured fallback order survives any number of switches.
type Router struct {
	base Provider

	mu      sync.RWMutex
	current Provider
}

// NewRouter wraps base, starting with it as the active target.
func NewRouter(base Provider) *Router {
	return &Router{base: base, current: base}
}

// Name reports the active target, not the configured one — a caller logging
// "which model served this" must see the override.
func (r *Router) Name() string { return r.Current().Name() }

// Complete delegates to whichever provider is active when the call starts. The
// target is read once per request, so a switch landing mid-turn takes effect
// on the next request rather than halfway through the one in flight.
func (r *Router) Complete(ctx context.Context, req Request) (*Response, error) {
	return r.Current().Complete(ctx, req)
}

// Current returns the provider serving turns right now.
func (r *Router) Current() Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// Base returns the chain built from config.toml.
func (r *Router) Base() Provider { return r.base }

// Switch redirects subsequent turns to p. A nil p is ignored rather than
// leaving the router with no target.
func (r *Router) Switch(p Provider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = p
}

// Reset restores the configured chain.
func (r *Router) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = r.base
}

// Overridden reports whether the active target differs from config.toml.
func (r *Router) Overridden() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current != r.base
}
