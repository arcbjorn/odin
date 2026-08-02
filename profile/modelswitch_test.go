package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arcbjorn/odin/model"
)

// catalogServer serves an OpenAI-shaped /models list.
func catalogServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		body := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{}
		for _, id := range ids {
			body.Data = append(body.Data, struct {
				ID string `json:"id"`
			}{ID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// twoProviderProfile lays out a profile whose chain is primary -> backup, each
// backed by its own catalog endpoint.
func twoProviderProfile(t *testing.T, primaryModels, backupModels []string) (*Profile, string) {
	t.Helper()
	primary := catalogServer(t, primaryModels...)
	backup := catalogServer(t, backupModels...)

	t.Setenv("TEST_PRIMARY_KEY", "primary-key")
	t.Setenv("TEST_BACKUP_KEY", "backup-key")

	config := fmt.Sprintf(`
toolsets = ["skills"]
timezone = "UTC"

[[providers]]
kind = "openai"
name = "primary"
model = %q
base_url = %q
api_key_env = "TEST_PRIMARY_KEY"

[[providers]]
kind = "openai"
name = "backup"
model = %q
base_url = %q
api_key_env = "TEST_BACKUP_KEY"
`, primaryModels[0], primary.URL, backupModels[0], backup.URL)

	root := writeProfile(t, "default", config, "# Test agent", false, true)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p, root
}

func newTestSwitcher(t *testing.T, p *Profile) (*Switcher, *model.Router) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens, err := providerTokens(p)
	if err != nil {
		t.Fatalf("providerTokens: %v", err)
	}
	base, err := buildProviderWith(p, tokens, log)
	if err != nil {
		t.Fatalf("buildProviderWith: %v", err)
	}
	router := model.NewRouter(base)
	sw, err := NewSwitcher(p, router, tokens, log)
	if err != nil {
		t.Fatalf("NewSwitcher: %v", err)
	}
	return sw, router
}

func TestSwitchExplicitProviderAndModel(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra", "gpt-5.5"}, []string{"glm-5.2"})
	sw, _ := newTestSwitcher(t, p)

	change, err := sw.Switch(context.Background(), "primary/gpt-5.5", model.SwitchPersistent)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if change.Target != "primary/gpt-5.5" {
		t.Fatalf("target = %q, want primary/gpt-5.5", change.Target)
	}
	if change.ProviderChanged {
		t.Fatal("same provider, different model must not report a provider change")
	}
	if change.Previous != "primary/gpt-5.6-terra" {
		t.Fatalf("previous = %q", change.Previous)
	}
}

// A model id that itself contains a slash must not be mistaken for a
// provider prefix. Only a leading segment naming a configured provider is one.
func TestSwitchTreatsSlashedModelIDAsAModel(t *testing.T) {
	p, _ := twoProviderProfile(t,
		[]string{"gpt-5.6-terra"},
		[]string{"glm-5.2", "moonshotai/kimi-k2-0905"})
	sw, _ := newTestSwitcher(t, p)

	change, err := sw.Switch(context.Background(), "moonshotai/kimi-k2-0905", model.SwitchPersistent)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if change.Target != "backup/moonshotai/kimi-k2-0905" {
		t.Fatalf("target = %q, want the model resolved on backup", change.Target)
	}
	if !change.ProviderChanged {
		t.Fatal("moving to the backup provider must report a provider change")
	}
}

func TestSwitchBareProviderUsesItsConfiguredModel(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	sw, _ := newTestSwitcher(t, p)

	change, err := sw.Switch(context.Background(), "backup", model.SwitchPersistent)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if change.Target != "backup/glm-5.2" {
		t.Fatalf("target = %q, want backup at its configured model", change.Target)
	}
	if change.ResolvedVia != "configured default" {
		t.Fatalf("resolved via %q", change.ResolvedVia)
	}
}

// The equivalent of Hermes's detect_provider_for_model: a bare model name
// finds its provider without the user naming one.
func TestSwitchDetectsProviderFromBareModelName(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2", "glm-4.9"})
	sw, _ := newTestSwitcher(t, p)

	change, err := sw.Switch(context.Background(), "glm-4.9", model.SwitchPersistent)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if change.Target != "backup/glm-4.9" {
		t.Fatalf("target = %q, want backup/glm-4.9", change.Target)
	}
}

// An input matching several catalog entries must ask rather than guess.
func TestSwitchRejectsAmbiguousModel(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2", "glm-4.9"})
	sw, _ := newTestSwitcher(t, p)

	_, err := sw.Switch(context.Background(), "glm", model.SwitchPersistent)
	if err == nil {
		t.Fatal("an ambiguous model must not resolve silently")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error should name the ambiguity, got: %v", err)
	}
}

func TestSwitchRejectsUnknownModel(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	sw, router := newTestSwitcher(t, p)

	before := router.Name()
	if _, err := sw.Switch(context.Background(), "no-such-model", model.SwitchPersistent); err == nil {
		t.Fatal("an unknown model must fail")
	}
	if router.Name() != before {
		t.Fatal("a failed switch must leave the active target untouched")
	}
	if router.Overridden() {
		t.Fatal("a failed switch must not mark the router as overridden")
	}
}

// Switching must not cost the resilience the chain exists for: the selected
// provider is promoted and the rest stay behind it as fallbacks.
func TestSwitchPromotesProviderAndKeepsFallback(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	sw, router := newTestSwitcher(t, p)

	if _, err := sw.Switch(context.Background(), "backup", model.SwitchPersistent); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	name := router.Name()
	if !strings.HasPrefix(name, "backup/glm-5.2") {
		t.Fatalf("chain = %q, want the switched provider promoted to primary", name)
	}
	if !strings.Contains(name, "primary/gpt-5.6-terra") {
		t.Fatalf("chain = %q, want the other provider retained as fallback", name)
	}
}

// The configured chain must survive any number of switches: it is what Reset
// restores and what scheduled jobs keep running.
func TestSwitchLeavesTheConfiguredChainIntact(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra", "gpt-5.5"}, []string{"glm-5.2"})
	sw, router := newTestSwitcher(t, p)

	base := router.Base()
	configured := base.Name()

	if _, err := sw.Switch(context.Background(), "primary/gpt-5.5", model.SwitchPersistent); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if router.Base() != base || base.Name() != configured {
		t.Fatalf("base chain changed to %q, want %q", base.Name(), configured)
	}

	target, err := sw.Reset()
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if target != "primary/gpt-5.6-terra" {
		t.Fatalf("reset target = %q", target)
	}
	if router.Overridden() {
		t.Fatal("after Reset the router must be back on the configured chain")
	}
}

// A switch is persisted, and a fresh process re-applies it without touching
// the network — an unreachable catalog at boot must not discard the choice.
func TestSwitchPersistsAndIsReappliedOffline(t *testing.T) {
	p, root := twoProviderProfile(t, []string{"gpt-5.6-terra", "gpt-5.5"}, []string{"glm-5.2"})
	sw, _ := newTestSwitcher(t, p)

	if _, err := sw.Switch(context.Background(), "primary/gpt-5.5", model.SwitchPersistent); err != nil {
		t.Fatalf("Switch: %v", err)
	}

	reloaded, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	provider, modelID, err := reloaded.ModelOverride()
	if err != nil {
		t.Fatalf("ModelOverride: %v", err)
	}
	if provider != "primary" || modelID != "gpt-5.5" {
		t.Fatalf("stored override = %s/%s", provider, modelID)
	}

	sw2, router2 := newTestSwitcher(t, reloaded)
	if err := sw2.applyStored(provider, modelID); err != nil {
		t.Fatalf("applyStored: %v", err)
	}
	if !strings.HasPrefix(router2.Name(), "primary/gpt-5.5") {
		t.Fatalf("restored chain = %q", router2.Name())
	}
	if got := sw2.Current(); got.Target != "primary/gpt-5.5" || !got.Overridden {
		t.Fatalf("Current = %+v", got)
	}
}

// An override naming a provider that has since left config.toml cannot be
// served, so it must be reported rather than applied.
func TestApplyStoredRejectsUnconfiguredProvider(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	sw, router := newTestSwitcher(t, p)

	err := sw.applyStored("removed-provider", "some-model")
	if err == nil {
		t.Fatal("an override for an unconfigured provider must fail")
	}
	if router.Overridden() {
		t.Fatal("a rejected override must leave the configured chain in place")
	}
}

// Both overrides share one state file. Writing either must preserve the other,
// or setting a model would silently reset the traveller's timezone.
func TestModelOverrideAndTimezoneOverrideCoexist(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.SetTimezone("Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetModelOverride("opencode-go", "glm-4.9"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	zone, source, err := reloaded.Timezone()
	if err != nil || zone != "Asia/Tokyo" || source != "runtime override" {
		t.Fatalf("timezone = %q (%s), err=%v — setting a model clobbered it", zone, source, err)
	}
	provider, modelID, err := reloaded.ModelOverride()
	if err != nil || provider != "opencode-go" || modelID != "glm-4.9" {
		t.Fatalf("model override = %s/%s, err=%v", provider, modelID, err)
	}

	// And the reverse: changing the timezone must keep the model override.
	if err := reloaded.SetTimezone("Europe/Lisbon"); err != nil {
		t.Fatal(err)
	}
	again, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	provider, modelID, err = again.ModelOverride()
	if err != nil || provider != "opencode-go" || modelID != "glm-4.9" {
		t.Fatalf("model override = %s/%s after a timezone change, err=%v", provider, modelID, err)
	}
}

func TestClearingModelOverrideRestoresConfig(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetModelOverride("opencode-go", "glm-4.9"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetModelOverride("", ""); err != nil {
		t.Fatal(err)
	}
	provider, modelID, err := p.ModelOverride()
	if err != nil || provider != "" || modelID != "" {
		t.Fatalf("override = %s/%s, err=%v, want cleared", provider, modelID, err)
	}
}

// The whole point of routing interactive turns separately: a /model switch
// must not reach the provider that scheduled jobs run on.
func TestScheduledJobsKeepTheConfiguredProviderAfterASwitch(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	rt, err := Build(p, log)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	jobProvider := rt.Provider.Name()
	if rt.Router.Base() != rt.Provider {
		t.Fatal("the router must wrap the same chain the job loop calls")
	}

	if _, err := rt.Switcher.Switch(context.Background(), "backup", model.SwitchPersistent); err != nil {
		t.Fatalf("Switch: %v", err)
	}

	if rt.Provider.Name() != jobProvider {
		t.Fatalf("job provider changed to %q, want %q", rt.Provider.Name(), jobProvider)
	}
	if !strings.HasPrefix(rt.Router.Name(), "backup/") {
		t.Fatalf("interactive provider = %q, want the switched target", rt.Router.Name())
	}
	if rt.Chat == rt.Loop {
		t.Fatal("interactive and scheduled turns must not share one loop")
	}
}

// A profile started with a stored override comes up on it, and status can see
// that the configured chain is not what chat is using.
func TestBuildAppliesStoredOverride(t *testing.T) {
	p, root := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	if err := p.SetModelOverride("backup", "glm-5.2"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := Build(reloaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	if got := rt.Switcher.Current(); got.Target != "backup/glm-5.2" || !got.Overridden {
		t.Fatalf("Current = %+v, want the stored override applied", got)
	}
}

// A stale override must not stop the agent from starting — it is dropped, and
// the configured chain runs.
func TestBuildDropsStaleOverride(t *testing.T) {
	p, root := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	if err := p.SetModelOverride("provider-that-left", "some-model"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := Build(reloaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build must survive a stale override: %v", err)
	}
	defer rt.Close()

	if rt.Switcher.Current().Overridden {
		t.Fatal("a stale override must be dropped, not applied")
	}
	stored, _, err := reloaded.ModelOverride()
	if err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Fatalf("stale override %q should have been cleared from state", stored)
	}
}

func TestOptionsListsEveryProviderEvenWhenACatalogFails(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	// Point the backup at a dead port so its catalog fetch fails.
	for i := range p.Config.Providers {
		if p.Config.Providers[i].Name == "backup" {
			p.Config.Providers[i].BaseURL = "http://127.0.0.1:1"
		}
	}
	sw, _ := newTestSwitcher(t, p)

	options, err := sw.Options(context.Background())
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if len(options) != 2 {
		t.Fatalf("got %d providers, want both listed", len(options))
	}
	if len(options[0].Models) == 0 {
		t.Fatal("the reachable provider should list its catalog")
	}
	if options[1].Err == "" {
		t.Fatal("the unreachable provider must carry a reason")
	}
	if options[1].Configured != "glm-5.2" {
		t.Fatalf("configured = %q, want the model that is still selectable", options[1].Configured)
	}
}

// An endpoint with no catalog must stay switchable: refusing the model would
// make those providers unusable for a reason the user cannot fix.
func TestSwitchAcceptsModelOnProviderWithoutACatalog(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	for i := range p.Config.Providers {
		if p.Config.Providers[i].Name == "backup" {
			p.Config.Providers[i].BaseURL = "http://127.0.0.1:1"
		}
	}
	sw, _ := newTestSwitcher(t, p)

	change, err := sw.Switch(context.Background(), "backup/some-private-model", model.SwitchPersistent)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if change.Target != "backup/some-private-model" {
		t.Fatalf("target = %q", change.Target)
	}
	if change.ResolvedVia != "unverified (no catalog)" {
		t.Fatalf("resolved via %q, want the unverified path to be visible", change.ResolvedVia)
	}
}

func TestSwitchRejectsEmptyInput(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	sw, _ := newTestSwitcher(t, p)

	if _, err := sw.Switch(context.Background(), "   ", model.SwitchPersistent); err == nil {
		t.Fatal("empty input must be rejected")
	}
}

// A `once` switch moves the conversation but leaves the stored choice alone,
// so a restart returns to what was committed rather than to an experiment.
func TestSwitchOnceDoesNotPersist(t *testing.T) {
	p, root := twoProviderProfile(t, []string{"gpt-5.6-terra", "gpt-5.5"}, []string{"glm-5.2"})
	sw, router := newTestSwitcher(t, p)

	change, err := sw.Switch(context.Background(), "primary/gpt-5.5", model.SwitchOnce)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if !change.Transient {
		t.Fatal("a once switch must report itself as transient")
	}
	if !strings.HasPrefix(router.Name(), "primary/gpt-5.5") {
		t.Fatalf("router = %q, want the switch applied in-process", router.Name())
	}
	if got := sw.Current(); !got.Transient || !got.Overridden {
		t.Fatalf("Current = %+v, want a transient override", got)
	}

	reloaded, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	provider, modelID, err := reloaded.ModelOverride()
	if err != nil {
		t.Fatal(err)
	}
	if provider != "" || modelID != "" {
		t.Fatalf("stored override = %s/%s, want nothing written", provider, modelID)
	}
}

// A once switch on top of a stored one must not erase it: the restart goes
// back to the stored choice, not to config.
func TestSwitchOnceLeavesAStoredOverrideIntact(t *testing.T) {
	p, root := twoProviderProfile(t, []string{"gpt-5.6-terra", "gpt-5.5"}, []string{"glm-5.2"})
	sw, _ := newTestSwitcher(t, p)

	if _, err := sw.Switch(context.Background(), "backup", model.SwitchPersistent); err != nil {
		t.Fatalf("persistent switch: %v", err)
	}
	if _, err := sw.Switch(context.Background(), "primary/gpt-5.5", model.SwitchOnce); err != nil {
		t.Fatalf("once switch: %v", err)
	}

	reloaded, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	provider, modelID, err := reloaded.ModelOverride()
	if err != nil {
		t.Fatal(err)
	}
	if provider != "backup" || modelID != "glm-5.2" {
		t.Fatalf("stored override = %s/%s, want the earlier persistent choice kept", provider, modelID)
	}
}

// Reset clears both the transient and the stored choice.
func TestResetClearsTransientState(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra", "gpt-5.5"}, []string{"glm-5.2"})
	sw, _ := newTestSwitcher(t, p)

	if _, err := sw.Switch(context.Background(), "primary/gpt-5.5", model.SwitchOnce); err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := sw.Current(); got.Transient || got.Overridden {
		t.Fatalf("Current = %+v, want the configured chain back", got)
	}
}

// Verification runs against the target alone. A fallback answering for it
// would report a working model that does not work.
func TestVerifyFailureLeavesTheTargetUnselected(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	sw, router := newTestSwitcher(t, p)
	before := router.Name()

	// The stub serves a catalog but no completions, so the tool probe fails.
	_, err := sw.Verify(context.Background(), "backup/glm-5.2", model.SwitchPersistent)
	if err == nil {
		t.Fatal("a model that cannot run the tool exchange must fail verification")
	}
	if router.Name() != before {
		t.Fatalf("router = %q, want the failed target not selected", router.Name())
	}
	if router.Overridden() {
		t.Fatal("a failed verification must not leave an override behind")
	}
}

func TestVerifyRejectsUnknownTarget(t *testing.T) {
	p, _ := twoProviderProfile(t, []string{"gpt-5.6-terra"}, []string{"glm-5.2"})
	sw, _ := newTestSwitcher(t, p)

	if _, err := sw.Verify(context.Background(), "no-such-model", model.SwitchPersistent); err == nil {
		t.Fatal("an unresolvable target must fail before any network call")
	}
}
