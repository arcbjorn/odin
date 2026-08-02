package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/arcbjorn/odin/model"
)

// fakeSwitcher stands in for profile.Switcher so the gateway's /model handling
// is testable without a provider, a credential, or a network call.
type fakeSwitcher struct {
	mu         sync.Mutex
	target     string
	overridden bool
	transient  bool
	configured []string
	options    []model.ProviderModels
	optionsErr error
	switchErr  error
	resetErr   error
	verifyErr  error
	verifyRes  model.VerifyResult
	switched   []string
	scopes     []model.SwitchScope
	verified   []string
	resets     int
}

func (f *fakeSwitcher) Current() model.Selection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return model.Selection{Target: f.target, Overridden: f.overridden, Transient: f.transient}
}

func (f *fakeSwitcher) Configured() []string { return f.configured }

func (f *fakeSwitcher) Options(context.Context) ([]model.ProviderModels, error) {
	return f.options, f.optionsErr
}

func (f *fakeSwitcher) Switch(_ context.Context, input string, scope model.SwitchScope) (model.SwitchChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switched = append(f.switched, input)
	f.scopes = append(f.scopes, scope)
	if f.switchErr != nil {
		return model.SwitchChange{}, f.switchErr
	}
	previous := f.target
	f.target = input
	f.overridden = true
	f.transient = scope == model.SwitchOnce
	return model.SwitchChange{
		Target:          input,
		Previous:        previous,
		ProviderChanged: !strings.HasPrefix(input, strings.SplitN(previous, "/", 2)[0]+"/"),
		ResolvedVia:     "catalog",
		Transient:       scope == model.SwitchOnce,
	}, nil
}

func (f *fakeSwitcher) Verify(_ context.Context, input string, scope model.SwitchScope) (model.VerifyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = append(f.verified, input)
	f.scopes = append(f.scopes, scope)
	if f.verifyErr != nil {
		return f.verifyRes, f.verifyErr
	}
	res := f.verifyRes
	if res.Target == "" {
		res.Target = f.target
	}
	if input != "" {
		res.Switched = true
		f.target = input
		f.overridden = true
	}
	return res, nil
}

func (f *fakeSwitcher) Reset() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	if f.resetErr != nil {
		return "", f.resetErr
	}
	f.target = f.configured[0]
	f.overridden = false
	f.transient = false
	return f.target, nil
}

func switcherGateway(t *testing.T) (*Telegram, *fakeTelegram, *fakeSwitcher, *fakeAgent) {
	t.Helper()
	agent := &fakeAgent{reply: "x"}
	g, fake := newGateway(t, agent, []int64{1})
	sw := &fakeSwitcher{
		target:     "primary/gpt-5.6-terra",
		configured: []string{"primary/gpt-5.6-terra", "backup/glm-5.2"},
	}
	g.switcher = sw
	return g, fake, sw, agent
}

func lastMessage(t *testing.T, fake *fakeTelegram) string {
	t.Helper()
	msgs := fake.messages()
	if len(msgs) == 0 {
		t.Fatal("no message sent")
	}
	return msgs[len(msgs)-1]
}

// Bare /model reports what runs now, the fallbacks behind it, and how to
// switch — all without reaching the agent.
func TestModelReportWithSwitcher(t *testing.T) {
	g, fake, _, agent := switcherGateway(t)

	g.respond(context.Background(), 1, "/model")

	last := lastMessage(t, fake)
	for _, want := range []string{"gpt-5.6-terra", "Fallback", "glm-5.2", "/model reset"} {
		if !strings.Contains(last, want) {
			t.Fatalf("report missing %q: %q", want, last)
		}
	}
	if agent.callCount() != 0 {
		t.Fatal("/model must not reach the agent")
	}
}

func TestModelSwitchAppliesAndConfirms(t *testing.T) {
	g, fake, sw, agent := switcherGateway(t)

	g.respond(context.Background(), 1, "/model backup/glm-5.2")

	if len(sw.switched) != 1 || sw.switched[0] != "backup/glm-5.2" {
		t.Fatalf("switcher received %v", sw.switched)
	}
	last := lastMessage(t, fake)
	if !strings.Contains(last, "backup/glm-5.2") {
		t.Fatalf("confirmation missing the new target: %q", last)
	}
	// The scope of a switch is the part users get wrong, so it is stated.
	if !strings.Contains(last, "scheduled jobs") {
		t.Fatalf("confirmation should state that jobs are unaffected: %q", last)
	}
	if agent.callCount() != 0 {
		t.Fatal("/model must not reach the agent")
	}
}

// Everything after the command name is the target, including model ids that
// contain spaces-free slashes and dots.
func TestModelSwitchPassesFullArgument(t *testing.T) {
	g, _, sw, _ := switcherGateway(t)

	g.respond(context.Background(), 1, "/model moonshotai/kimi-k2-0905")

	if len(sw.switched) != 1 || sw.switched[0] != "moonshotai/kimi-k2-0905" {
		t.Fatalf("switcher received %v", sw.switched)
	}
}

// A rejected switch must say why and leave the conversation on its model.
func TestModelSwitchFailureIsReported(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	sw.switchErr = errors.New(`"glm" is ambiguous: backup/glm-4.9, backup/glm-5.2`)

	g.respond(context.Background(), 1, "/model glm")

	last := lastMessage(t, fake)
	if !strings.Contains(last, "ambiguous") {
		t.Fatalf("failure should be reported verbatim: %q", last)
	}
	if sw.overridden {
		t.Fatal("a failed switch must not mark an override")
	}
}

func TestModelResetRestoresConfiguredChain(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	sw.target, sw.overridden = "backup/glm-5.2", true

	g.respond(context.Background(), 1, "/model reset")

	if sw.resets != 1 {
		t.Fatalf("resets = %d, want 1", sw.resets)
	}
	last := lastMessage(t, fake)
	if !strings.Contains(last, "primary/gpt-5.6-terra") {
		t.Fatalf("reset confirmation missing the restored target: %q", last)
	}
}

func TestModelListRendersCatalogAndMarksActive(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	sw.options = []model.ProviderModels{
		{Provider: "primary", Configured: "gpt-5.6-terra", Models: []string{"gpt-5.6-terra", "gpt-5.5"}},
		{Provider: "backup", Configured: "glm-5.2", Err: "no live catalog on this endpoint"},
	}

	g.respond(context.Background(), 1, "/model list")

	last := lastMessage(t, fake)
	for _, want := range []string{"primary", "gpt-5.5", "active", "backup", "no live catalog"} {
		if !strings.Contains(last, want) {
			t.Fatalf("catalog listing missing %q: %q", want, last)
		}
	}
	// A provider without a catalog is still selectable at its configured model.
	if !strings.Contains(last, "glm-5.2") {
		t.Fatalf("a catalog-less provider must still show its configured model: %q", last)
	}
}

// A long catalog is clipped rather than paged across several chat messages.
func TestModelListClipsLongCatalogs(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	many := make([]string, 0, maxListedModels+5)
	for i := 0; i < maxListedModels+5; i++ {
		many = append(many, "model-"+string(rune('a'+i%26))+itoa(int64(i)))
	}
	sw.options = []model.ProviderModels{{Provider: "primary", Configured: many[0], Models: many}}

	g.respond(context.Background(), 1, "/model list")

	last := lastMessage(t, fake)
	if !strings.Contains(last, "5 more") {
		t.Fatalf("clipped listing should say how many were omitted: %q", last)
	}
	if strings.Contains(last, many[len(many)-1]) {
		t.Fatal("clipped listing should not include entries past the cap")
	}
}

func TestModelListReportsCatalogError(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	sw.optionsErr = errors.New("boom")

	g.respond(context.Background(), 1, "/model list")

	if last := lastMessage(t, fake); !strings.Contains(last, "boom") {
		t.Fatalf("listing failure should be reported: %q", last)
	}
}

// Without a switcher wired, /model stays the read-only report it was.
func TestModelSwitchIsInertWithoutASwitcher(t *testing.T) {
	agent := &fakeAgent{reply: "x"}
	g, fake := newGateway(t, agent, []int64{1})
	g.modelChain = []string{"primary/gpt-5.6-terra", "backup/glm-5.2"}

	g.respond(context.Background(), 1, "/model backup/glm-5.2")

	last := lastMessage(t, fake)
	if !strings.Contains(last, "Fallback") {
		t.Fatalf("expected the static report, got: %q", last)
	}
	if agent.callCount() != 0 {
		t.Fatal("/model must not reach the agent even when switching is unavailable")
	}
}

// A persisted-override warning has to surface: an override that silently
// reverts on restart is the drift this feature exists to avoid.
func TestModelSwitchSurfacesWarning(t *testing.T) {
	g, fake, _, _ := switcherGateway(t)
	g.switcher = &warningSwitcher{}

	g.respond(context.Background(), 1, "/model backup/glm-5.2")

	if last := lastMessage(t, fake); !strings.Contains(last, "could not persist") {
		t.Fatalf("warning not surfaced: %q", last)
	}
}

type warningSwitcher struct{ fakeSwitcher }

func (w *warningSwitcher) Switch(context.Context, string, model.SwitchScope) (model.SwitchChange, error) {
	return model.SwitchChange{
		Target:  "backup/glm-5.2",
		Warning: "active for this process only — could not persist: read-only file system",
	}, nil
}

// `/model once` applies without persisting — the exploratory switch Hermes
// makes its default and Odin makes explicit.
func TestModelOnceSwitchesWithoutPersisting(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)

	g.respond(context.Background(), 1, "/model once backup/glm-5.2")

	if len(sw.switched) != 1 || sw.switched[0] != "backup/glm-5.2" {
		t.Fatalf("switcher received %v, want the target without the verb", sw.switched)
	}
	if len(sw.scopes) != 1 || sw.scopes[0] != model.SwitchOnce {
		t.Fatalf("scopes = %v, want SwitchOnce", sw.scopes)
	}
	if last := lastMessage(t, fake); !strings.Contains(last, "process only") {
		t.Fatalf("confirmation should state the switch is not stored: %q", last)
	}
}

func TestModelOnceWithoutTargetExplainsItself(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)

	g.respond(context.Background(), 1, "/model once")

	if len(sw.switched) != 0 {
		t.Fatalf("nothing should have been switched: %v", sw.switched)
	}
	if last := lastMessage(t, fake); !strings.Contains(last, "needs a target") {
		t.Fatalf("expected usage help, got: %q", last)
	}
}

// A transient override has to be visibly transient, or the next restart looks
// like the model silently changed itself.
func TestModelReportMarksTransientOverride(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	sw.target, sw.overridden, sw.transient = "backup/glm-5.2", true, true

	g.respond(context.Background(), 1, "/model")

	if last := lastMessage(t, fake); !strings.Contains(last, "process only") {
		t.Fatalf("report should mark a transient override: %q", last)
	}
}

func TestModelVerifyChecksRunningTarget(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	sw.verifyRes = model.VerifyResult{Target: "primary/gpt-5.6-terra", CatalogChecked: true}

	g.respond(context.Background(), 1, "/model verify")

	if len(sw.verified) != 1 || sw.verified[0] != "" {
		t.Fatalf("verified %v, want the running target", sw.verified)
	}
	last := lastMessage(t, fake)
	for _, want := range []string{"primary/gpt-5.6-terra", "catalog ok", "tool call ok", "continuation ok"} {
		if !strings.Contains(last, want) {
			t.Fatalf("verify report missing %q: %q", want, last)
		}
	}
	if strings.Contains(last, "Switched") {
		t.Fatalf("verifying the running target must not report a switch: %q", last)
	}
}

// The point of the command: a target that cannot hold up the tool exchange is
// caught here rather than mid-turn, and is not switched to.
func TestModelVerifyFailureDoesNotSwitch(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)
	sw.verifyErr = errors.New("tool call: model returned no tool_calls")
	sw.verifyRes = model.VerifyResult{Target: "backup/glm-5.2"}

	g.respond(context.Background(), 1, "/model verify backup/glm-5.2")

	last := lastMessage(t, fake)
	if !strings.Contains(last, "failed verification") || !strings.Contains(last, "no tool_calls") {
		t.Fatalf("failure should be reported with its reason: %q", last)
	}
	if sw.overridden {
		t.Fatal("a target that failed verification must not become active")
	}
}

func TestModelVerifySwitchesOnSuccess(t *testing.T) {
	g, fake, sw, _ := switcherGateway(t)

	g.respond(context.Background(), 1, "/model verify backup/glm-5.2")

	if len(sw.verified) != 1 || sw.verified[0] != "backup/glm-5.2" {
		t.Fatalf("verified %v", sw.verified)
	}
	if last := lastMessage(t, fake); !strings.Contains(last, "Switched") {
		t.Fatalf("a passing check should switch and say so: %q", last)
	}
}

// The two modifiers compose: check it, use it, but do not store it.
func TestModelOnceVerifyComposes(t *testing.T) {
	g, _, sw, _ := switcherGateway(t)

	g.respond(context.Background(), 1, "/model once verify backup/glm-5.2")

	if len(sw.verified) != 1 || sw.verified[0] != "backup/glm-5.2" {
		t.Fatalf("verified %v, want the target with both verbs stripped", sw.verified)
	}
	if len(sw.scopes) != 1 || sw.scopes[0] != model.SwitchOnce {
		t.Fatalf("scopes = %v, want SwitchOnce", sw.scopes)
	}
}
