package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeTimezoneOverrideAndReset(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetTimezone("Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	zone, source, err := reloaded.Timezone()
	if err != nil || zone != "Asia/Tokyo" || source != "runtime override" {
		t.Fatalf("timezone = %q (%s), err=%v", zone, source, err)
	}
	if err := reloaded.SetTimezone(""); err != nil {
		t.Fatal(err)
	}
	zone, source, err = reloaded.Timezone()
	if err != nil || zone != "America/New_York" || source != "config" {
		t.Fatalf("reset timezone = %q (%s), err=%v", zone, source, err)
	}
}

func TestRuntimeTimezoneRejectsInvalidState(t *testing.T) {
	root := writeProfile(t, "default", minimalConfig, "# General assistant", true, true)
	p, err := Load(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.StateDir, "runtime.json"), []byte(`{"version":1,"timezone":"Mars/Olympus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Location(); err == nil {
		t.Fatal("expected invalid runtime timezone to fail")
	}
}
