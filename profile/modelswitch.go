package profile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arcbjorn/odin/model"
)

// catalogTimeout bounds one provider's model-list fetch. A dead endpoint must
// not hold up the answer to a chat command; the other providers still list.
const catalogTimeout = 20 * time.Second

// ModelTarget is one resolved destination: a provider from config.toml and a
// model it should serve.
type ModelTarget struct {
	Provider string
	Model    string
}

func (t ModelTarget) String() string {
	if t.Provider == "" {
		return ""
	}
	return t.Provider + "/" + t.Model
}

// Switcher resolves free-form model input against the configured providers and
// redirects the interactive Router at the result.
//
// It deliberately mirrors the split Hermes draws between its `hermes model`
// setup wizard and its in-session /model: this selects only among providers
// already present in config.toml. It never adds a provider, runs an OAuth
// flow, or prompts for a key. `odin auth` and config.toml own that, where the
// choice is reviewable in git instead of typed into a chat window.
//
// Scope is equally deliberate. A switch moves interactive turns only —
// scheduled jobs keep running the committed chain. An exploratory switch at
// 23:00 must not quietly become the model that runs the 07:00 job, which is
// the drift a config-driven scheduler exists to prevent.
type Switcher struct {
	profile *Profile
	router  *model.Router
	tokens  map[string]model.TokenSource
	log     *slog.Logger

	// catalogs holds one transport per configured provider, built at its
	// configured model and used only to read the live model list. Built once
	// at construction and never replaced, so it needs no lock.
	catalogs map[string]model.Provider

	mu sync.Mutex
	// current is the active target; transient marks it as SwitchOnce, which
	// deliberately left the stored choice untouched.
	current   ModelTarget
	transient bool
}

// NewSwitcher wires a router to the profile that produced it. The token
// sources must be the same ones the base chain was built from.
func NewSwitcher(p *Profile, router *model.Router, tokens map[string]model.TokenSource, log *slog.Logger) (*Switcher, error) {
	if router == nil {
		return nil, errors.New("switcher needs a router")
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Switcher{
		profile:  p,
		router:   router,
		tokens:   tokens,
		log:      log,
		catalogs: make(map[string]model.Provider, len(p.Config.Providers)),
	}
	for _, pc := range p.Config.Providers {
		transport, err := buildTransport(pc, tokens[pc.Name], log)
		if err != nil {
			return nil, err
		}
		s.catalogs[pc.Name] = transport
	}
	if len(p.Config.Providers) > 0 {
		primary := p.Config.Providers[0]
		s.current = ModelTarget{Provider: primary.Name, Model: primary.Model}
	}
	return s, nil
}

// Target reports the active provider and model.
func (s *Switcher) Target() ModelTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// Current reports the active selection. It satisfies the gateway's contract.
func (s *Switcher) Current() model.Selection {
	s.mu.Lock()
	target, transient := s.current, s.transient
	s.mu.Unlock()
	return model.Selection{
		Target:     target.String(),
		Overridden: s.router.Overridden(),
		Transient:  transient,
	}
}

// Configured lists the committed chain as "provider/model", primary first.
// Offline: this is what /model falls back to when no catalog is reachable.
func (s *Switcher) Configured() []string {
	out := make([]string, 0, len(s.profile.Config.Providers))
	for _, pc := range s.profile.Config.Providers {
		out = append(out, pc.Name+"/"+pc.Model)
	}
	return out
}

// Options lists every switchable model per configured provider.
//
// A provider whose catalog is unreachable is still listed, carrying the
// reason: its configured model stays a valid target, so an unavailable
// catalog must not remove the provider from the menu.
func (s *Switcher) Options(ctx context.Context) ([]model.ProviderModels, error) {
	out := make([]model.ProviderModels, 0, len(s.profile.Config.Providers))
	for _, pc := range s.profile.Config.Providers {
		entry := model.ProviderModels{Provider: pc.Name, Configured: pc.Model}
		models, err := s.catalogModels(ctx, pc.Name)
		switch {
		case errors.Is(err, model.ErrCatalogUnsupported):
			entry.Err = "no live catalog on this endpoint"
		case err != nil:
			entry.Err = err.Error()
		default:
			entry.Models = models
		}
		out = append(out, entry)
	}
	return out, nil
}

// Switch resolves input and redirects interactive turns at the result.
//
// Accepted forms, in resolution order:
//
//	provider/model   explicit, when the leading segment names a configured
//	                 provider (model ids contain slashes, provider names are
//	                 validated never to, so this is unambiguous)
//	provider         that provider at its configured model
//	model            detected across configured providers' catalogs
func (s *Switcher) Switch(ctx context.Context, input string, scope model.SwitchScope) (model.SwitchChange, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return model.SwitchChange{}, errors.New("a model is required")
	}

	target, via, err := s.resolve(ctx, input)
	if err != nil {
		return model.SwitchChange{}, err
	}
	return s.apply(target, via, scope)
}

// apply moves the router onto an already resolved target.
func (s *Switcher) apply(target ModelTarget, via string, scope model.SwitchScope) (model.SwitchChange, error) {
	previous := s.Target()
	provider, err := s.buildSwitched(target)
	if err != nil {
		return model.SwitchChange{}, err
	}

	s.router.Switch(provider)
	s.mu.Lock()
	s.current = target
	s.transient = scope == model.SwitchOnce
	s.mu.Unlock()

	change := model.SwitchChange{
		Target:          target.String(),
		Previous:        previous.String(),
		ProviderChanged: target.Provider != previous.Provider,
		ResolvedVia:     via,
		Transient:       scope == model.SwitchOnce,
	}
	if scope == model.SwitchOnce {
		// Deliberately leaves the stored choice alone: a restart returns to
		// whatever was committed, which is the whole point of asking for one.
		return change, nil
	}

	// Persist after the swap, not before: the user asked for this turn to use
	// the new model, and a read-only state directory should not block that.
	// The failure is reported rather than swallowed, because an override that
	// silently reverts on restart is exactly the drift this avoids.
	if err := s.profile.SetModelOverride(target.Provider, target.Model); err != nil {
		s.log.Warn("could not persist model override", "target", target.String(), "error", err)
		change.Warning = "active for this process only — could not persist: " + err.Error()
	}
	return change, nil
}

// Reset restores the chain from config.toml and clears the stored override.
func (s *Switcher) Reset() (string, error) {
	s.router.Reset()

	var target ModelTarget
	if len(s.profile.Config.Providers) > 0 {
		primary := s.profile.Config.Providers[0]
		target = ModelTarget{Provider: primary.Name, Model: primary.Model}
	}
	s.mu.Lock()
	s.current = target
	s.transient = false
	s.mu.Unlock()

	if err := s.profile.SetModelOverride("", ""); err != nil {
		return target.String(), fmt.Errorf("clear stored override: %w", err)
	}
	return target.String(), nil
}

// Verify runs a live protocol check — catalog, tool call, and the post-tool
// continuation — against a target.
//
// An empty input checks whatever is running now. Naming a target checks that
// one and moves onto it only if it passes, so a model that cannot hold up the
// tool exchange is found here rather than halfway through a real turn.
//
// The check runs against the target alone, never the chain: a fallback
// answering for it would report success for a model that does not work.
func (s *Switcher) Verify(ctx context.Context, input string, scope model.SwitchScope) (model.VerifyResult, error) {
	input = strings.TrimSpace(input)

	target := s.Target()
	via := ""
	switching := input != ""
	if switching {
		resolved, resolvedVia, err := s.resolve(ctx, input)
		if err != nil {
			return model.VerifyResult{}, err
		}
		target, via = resolved, resolvedVia
	}

	pinned, err := s.pinned(target)
	if err != nil {
		return model.VerifyResult{}, err
	}
	verification, err := model.VerifyProvider(ctx, pinned)
	if err != nil {
		return model.VerifyResult{Target: target.String()}, err
	}

	result := model.VerifyResult{
		Target:         target.String(),
		CatalogChecked: verification.CatalogChecked,
		ToolCall:       verification.ToolCall,
		Continuation:   verification.Continuation,
	}
	if !switching {
		return result, nil
	}
	if _, err := s.apply(target, via, scope); err != nil {
		return result, err
	}
	result.Switched = true
	return result, nil
}

// pinned builds the target on its own, with no fallback behind it.
func (s *Switcher) pinned(target ModelTarget) (model.Provider, error) {
	pc, ok := s.configured(target.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", target.Provider)
	}
	pc.Model = target.Model
	return buildTransport(pc, s.tokens[pc.Name], s.log)
}

// applyStored re-applies a persisted override at startup.
//
// No catalog is consulted: startup stays offline and fast, and a provider that
// is merely unreachable at boot must not silently discard the user's choice.
// An override naming a provider no longer in config.toml is dropped, since
// nothing can serve it.
func (s *Switcher) applyStored(providerName, modelID string) error {
	pc, ok := s.configured(providerName)
	if !ok {
		return fmt.Errorf("provider %q is no longer configured", providerName)
	}
	if modelID == "" {
		modelID = pc.Model
	}
	target := ModelTarget{Provider: providerName, Model: modelID}
	provider, err := s.buildSwitched(target)
	if err != nil {
		return err
	}
	s.router.Switch(provider)
	s.mu.Lock()
	s.current = target
	s.transient = false
	s.mu.Unlock()
	return nil
}

// resolve maps free-form input onto a configured provider and a model.
func (s *Switcher) resolve(ctx context.Context, input string) (ModelTarget, string, error) {
	// Explicit "provider/model". Split on the first slash only, and only
	// accept it when the leading segment is a configured provider — model ids
	// such as "moonshotai/kimi-k2" legitimately contain slashes.
	if name, rest, ok := strings.Cut(input, "/"); ok {
		if pc, found := s.configured(name); found {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				return ModelTarget{Provider: pc.Name, Model: pc.Model}, "configured default", nil
			}
			return s.resolveOnProvider(ctx, pc, rest)
		}
	}

	// A bare provider name selects that provider at its configured model.
	if pc, found := s.configured(input); found {
		return ModelTarget{Provider: pc.Name, Model: pc.Model}, "configured default", nil
	}

	return s.detect(ctx, input)
}

// resolveOnProvider validates a model against one provider's catalog. A
// provider with no reachable catalog accepts the model as given: refusing it
// would make those endpoints unswitchable for a reason the user cannot fix.
func (s *Switcher) resolveOnProvider(ctx context.Context, pc ProviderConfig, modelID string) (ModelTarget, string, error) {
	models, err := s.catalogModels(ctx, pc.Name)
	if err != nil {
		return ModelTarget{Provider: pc.Name, Model: modelID}, "unverified (no catalog)", nil
	}
	for _, id := range models {
		if id == modelID {
			return ModelTarget{Provider: pc.Name, Model: id}, "catalog", nil
		}
	}
	for _, id := range models {
		if strings.EqualFold(id, modelID) {
			return ModelTarget{Provider: pc.Name, Model: id}, "catalog (case-insensitive)", nil
		}
	}
	if matches := matchModels(models, modelID); len(matches) == 1 {
		return ModelTarget{Provider: pc.Name, Model: matches[0]}, "catalog (unique match)", nil
	} else if len(matches) > 1 {
		return ModelTarget{}, "", fmt.Errorf("%q is ambiguous on %s: %s", modelID, pc.Name, strings.Join(clip(matches, 8), ", "))
	}
	return ModelTarget{}, "", fmt.Errorf("%s does not offer %q", pc.Name, modelID)
}

// detect finds which configured provider serves a bare model name, the
// equivalent of Hermes's detect_provider_for_model. Configured models are
// checked first so the common case costs no network call.
func (s *Switcher) detect(ctx context.Context, modelID string) (ModelTarget, string, error) {
	for _, pc := range s.profile.Config.Providers {
		if pc.Model == modelID {
			return ModelTarget{Provider: pc.Name, Model: pc.Model}, "configured default", nil
		}
	}

	type candidate struct{ target ModelTarget }
	var exact, fuzzy []candidate
	var reachable bool

	for _, pc := range s.profile.Config.Providers {
		models, err := s.catalogModels(ctx, pc.Name)
		if err != nil {
			continue
		}
		reachable = true
		for _, id := range models {
			switch {
			case id == modelID:
				exact = append(exact, candidate{ModelTarget{pc.Name, id}})
			case strings.EqualFold(id, modelID):
				exact = append(exact, candidate{ModelTarget{pc.Name, id}})
			}
		}
		for _, id := range matchModels(models, modelID) {
			fuzzy = append(fuzzy, candidate{ModelTarget{pc.Name, id}})
		}
	}

	// Chain order is preference order: the first provider that serves the
	// model wins, rather than asking the user to disambiguate a choice
	// config.toml already ranked.
	if len(exact) > 0 {
		return exact[0].target, "catalog", nil
	}
	if len(fuzzy) == 1 {
		return fuzzy[0].target, "catalog (unique match)", nil
	}
	if len(fuzzy) > 1 {
		names := make([]string, 0, len(fuzzy))
		for _, c := range fuzzy {
			names = append(names, c.target.String())
		}
		sort.Strings(names)
		return ModelTarget{}, "", fmt.Errorf("%q is ambiguous: %s", modelID, strings.Join(clip(names, 8), ", "))
	}
	if !reachable {
		return ModelTarget{}, "", fmt.Errorf("no provider catalog is reachable; name the provider explicitly, e.g. %s/%s",
			s.profile.Config.Providers[0].Name, modelID)
	}
	return ModelTarget{}, "", fmt.Errorf("no configured provider offers %q", modelID)
}

// buildSwitched assembles a chain with target promoted to primary at the
// requested model, keeping the other configured providers as fallbacks at
// their own models. Switching a model must not cost the resilience the
// fallback chain exists to provide.
func (s *Switcher) buildSwitched(target ModelTarget) (model.Provider, error) {
	var selected ProviderConfig
	found := false
	rest := make([]ProviderConfig, 0, len(s.profile.Config.Providers))
	for _, pc := range s.profile.Config.Providers {
		if pc.Name == target.Provider && !found {
			pc.Model = target.Model
			selected = pc
			found = true
			continue
		}
		rest = append(rest, pc)
	}
	if !found {
		return nil, fmt.Errorf("provider %q is not configured", target.Provider)
	}

	ordered := append([]ProviderConfig{selected}, rest...)
	providers := make([]model.Provider, 0, len(ordered))
	for _, pc := range ordered {
		transport, err := buildTransport(pc, s.tokens[pc.Name], s.log)
		if err != nil {
			return nil, err
		}
		providers = append(providers, transport)
	}
	if len(providers) == 1 {
		return providers[0], nil
	}
	return model.NewChain(model.ChainConfig{Providers: providers, Logger: s.log})
}

func (s *Switcher) configured(name string) (ProviderConfig, bool) {
	for _, pc := range s.profile.Config.Providers {
		if pc.Name == name {
			return pc, true
		}
	}
	return ProviderConfig{}, false
}

// catalogModels fetches one provider's live model list under its own timeout.
func (s *Switcher) catalogModels(ctx context.Context, name string) ([]string, error) {
	transport, ok := s.catalogs[name]
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", name)
	}
	catalog, ok := transport.(model.ModelCatalog)
	if !ok {
		return nil, model.ErrCatalogUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	return catalog.Models(ctx)
}

// matchModels returns catalog entries containing needle, case-insensitively.
func matchModels(models []string, needle string) []string {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return nil
	}
	var out []string
	for _, id := range models {
		if strings.Contains(strings.ToLower(id), needle) {
			out = append(out, id)
		}
	}
	return out
}

func clip(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	return append(append([]string(nil), items[:max]...), "…")
}
