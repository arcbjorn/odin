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
	configured []string
	options    []model.ProviderModels
	optionsErr error
	switchErr  error
	resetErr   error
	switched   []string
	resets     int
}

func (f *fakeSwitcher) Current() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target, f.overridden
}

func (f *fakeSwitcher) Configured() []string { return f.configured }

func (f *fakeSwitcher) Options(context.Context) ([]model.ProviderModels, error) {
	return f.options, f.optionsErr
}

func (f *fakeSwitcher) Switch(_ context.Context, input string) (model.SwitchChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switched = append(f.switched, input)
	if f.switchErr != nil {
		return model.SwitchChange{}, f.switchErr
	}
	previous := f.target
	f.target = input
	f.overridden = true
	return model.SwitchChange{
		Target:          input,
		Previous:        previous,
		ProviderChanged: !strings.HasPrefix(input, strings.SplitN(previous, "/", 2)[0]+"/"),
		ResolvedVia:     "catalog",
	}, nil
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

func (w *warningSwitcher) Switch(context.Context, string) (model.SwitchChange, error) {
	return model.SwitchChange{
		Target:  "backup/glm-5.2",
		Warning: "active for this process only — could not persist: read-only file system",
	}, nil
}
