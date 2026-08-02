package model

import (
	"context"
	"testing"
)

func TestRouterStartsOnBaseAndReportsNoOverride(t *testing.T) {
	base := &fakeProvider{name: "base/model-1"}
	r := NewRouter(base)

	if r.Name() != "base/model-1" {
		t.Fatalf("Name = %q, want the base provider", r.Name())
	}
	if r.Overridden() {
		t.Fatal("a fresh router must not report an override")
	}
	if r.Base() != Provider(base) {
		t.Fatal("Base must return the provider the router was built from")
	}
}

func TestRouterSwitchRedirectsAndResetRestores(t *testing.T) {
	base := &fakeProvider{name: "base/model-1"}
	next := &fakeProvider{name: "next/model-2"}
	r := NewRouter(base)

	r.Switch(next)
	if r.Name() != "next/model-2" {
		t.Fatalf("after Switch, Name = %q, want the new target", r.Name())
	}
	if !r.Overridden() {
		t.Fatal("a switched router must report an override")
	}

	if _, err := r.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if next.calls != 1 || base.calls != 0 {
		t.Fatalf("calls: base=%d next=%d, want the switched target to serve", base.calls, next.calls)
	}

	r.Reset()
	if r.Name() != "base/model-1" || r.Overridden() {
		t.Fatalf("after Reset, Name = %q overridden = %v, want the base back", r.Name(), r.Overridden())
	}
	if _, err := r.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("Complete after reset: %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base served %d calls after reset, want 1", base.calls)
	}
}

// A nil target would leave the router with nothing to call, turning a bad
// switch into a dead agent. It must be ignored instead.
func TestRouterIgnoresNilSwitch(t *testing.T) {
	base := &fakeProvider{name: "base/model-1"}
	r := NewRouter(base)

	r.Switch(nil)
	if r.Current() != Provider(base) {
		t.Fatal("a nil switch must leave the current target intact")
	}
	if _, err := r.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base served %d calls, want 1", base.calls)
	}
}
