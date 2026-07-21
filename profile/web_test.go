package profile

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

const webConfig = `
toolsets = ["tracker", "web"]

[[providers]]
kind = "openai"
name = "opencode-go"
model = "glm-5.2"
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_GO_API_KEY"
`

// Fetch is the only web capability the profile actually needs; search stays
// absent until a backend is configured.
func TestWebToolsetRegistersFetchOnly(t *testing.T) {
	root := writeProfile(t, "default", webConfig, "# General assistant", true, false)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	if _, ok := rt.Tools.Lookup("fetch_url"); !ok {
		t.Fatal("fetch_url not registered")
	}
	if _, ok := rt.Tools.Lookup("search_web"); ok {
		t.Fatal("search_web registered with no backend configured")
	}
}

// Setting search_url is the whole switch-on for SearXNG later.
func TestSearchURLEnablesSearchTool(t *testing.T) {
	cfg := webConfig + "\n[web]\nsearch_url = \"http://127.0.0.1:8080\"\n"
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	if _, ok := rt.Tools.Lookup("search_web"); !ok {
		t.Fatal("search_web not registered despite search_url being set")
	}
}

func TestWebConfigParses(t *testing.T) {
	cfg := webConfig + `
[web]
reader_url = "https://reader.internal/"
reader_key_env = "JINA_API_KEY"
search_url = "http://127.0.0.1:8080"
`
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Config.Web.ReaderURL != "https://reader.internal/" {
		t.Errorf("reader_url = %q", p.Config.Web.ReaderURL)
	}
	if p.Config.Web.ReaderKeyEnv != "JINA_API_KEY" {
		t.Errorf("reader_key_env = %q", p.Config.Web.ReaderKeyEnv)
	}
	if p.Config.Web.SearchURL != "http://127.0.0.1:8080" {
		t.Errorf("search_url = %q", p.Config.Web.SearchURL)
	}
}

// A missing reader key is a warning, not a failure: it only lowers the rate
// limit, and the keyless tier is fine for a few fetches a day.
func TestMissingReaderKeyIsNotFatal(t *testing.T) {
	cfg := webConfig + "\n[web]\nreader_key_env = \"JINA_API_KEY\"\n"
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")
	t.Setenv("JINA_API_KEY", "")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("a missing reader key should not block startup: %v", err)
	}
	defer rt.Close()

	if _, ok := rt.Tools.Lookup("fetch_url"); !ok {
		t.Fatal("fetch_url should still be registered")
	}
}

// A bad search URL must fail at startup rather than on first use.
func TestInvalidSearchURLIsFatal(t *testing.T) {
	cfg := webConfig + "\n[web]\nsearch_url = \"not a url\"\n"
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)
	t.Setenv("OPENCODE_GO_API_KEY", "test-key")

	p, err := Load(root, "default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected an invalid search_url to fail the build")
	}
}

func TestUnknownWebKeyIsFatal(t *testing.T) {
	cfg := webConfig + "\n[web]\nfirecrawl_key = \"fc-abc\"\n"
	root := writeProfile(t, "default", cfg, "# General assistant", true, false)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("expected an unknown [web] key to be refused")
	}
	if !strings.Contains(err.Error(), "firecrawl_key") {
		t.Fatalf("error should name the bad key, got: %v", err)
	}
}
