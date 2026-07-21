// Package agent runs the tool-use loop.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"odin/model"
)

// Handler executes one tool call. Input is the raw JSON the model produced;
// the returned string goes back to the model verbatim as the tool result.
//
// Return an error for failures the model should see and correct (bad
// arguments, missing file). The loop feeds the error text back rather than
// aborting the turn — but repeated identical failures trip the guardrail.
type Handler func(ctx context.Context, input json.RawMessage) (string, error)

// Tool is a callable tool: its definition plus its implementation.
type Tool struct {
	Def    model.Tool
	Handle Handler
}

// Registry is a profile's tool set. A profile can call only registered tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool. Duplicate names are a programming error, not a runtime
// condition — fail loudly rather than silently shadowing an existing tool.
func (r *Registry) Register(t Tool) error {
	if t.Def.Name == "" {
		return fmt.Errorf("tool has no name")
	}
	if t.Handle == nil {
		return fmt.Errorf("tool %q has no handler", t.Def.Name)
	}
	// Guardrail on the tool surface itself: a schema with required fields
	// buried under many optionals is what let a weaker model emit the same
	// malformed call 162 times. Keep tool schemas small and flat.
	if n := len(schemaProps(t.Def.Schema)); n > 6 {
		return fmt.Errorf("tool %q has %d properties; keep tool schemas small (<=6) so weaker models can fill them", t.Def.Name, n)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Def.Name]; exists {
		return fmt.Errorf("tool %q already registered", t.Def.Name)
	}
	r.tools[t.Def.Name] = t
	return nil
}

// MustRegister is Register for startup wiring, where a failure is fatal.
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Defs returns the tool definitions to send to the provider, sorted by name.
//
// Sorted deliberately: the tool list renders at the front of the prompt, so a
// non-deterministic order changes the cached prefix on every request and
// silently destroys the prompt cache.
func (r *Registry) Defs() []model.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]model.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// Lookup finds a tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names lists registered tool names, sorted. Used by `odin status`.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func schemaProps(schema map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	return props
}
