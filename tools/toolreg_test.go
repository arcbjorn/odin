package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newToolReg(t *testing.T, registryURL string) *ToolReg {
	t.Helper()
	r, err := NewToolReg(ToolRegConfig{Name: "health", RegistryURL: registryURL, Token: "tok_test"})
	if err != nil {
		t.Fatalf("NewToolReg: %v", err)
	}
	return r
}

func TestNewToolRegRequiresNameURLAndToken(t *testing.T) {
	cases := []ToolRegConfig{
		{Name: "", RegistryURL: "https://api.example.com/v1/tools", Token: "x"},
		{Name: "Health", RegistryURL: "https://api.example.com/v1/tools", Token: "x"},
		{Name: "1health", RegistryURL: "https://api.example.com/v1/tools", Token: "x"},
		{Name: "he-alth", RegistryURL: "https://api.example.com/v1/tools", Token: "x"},
		{Name: strings.Repeat("a", 25), RegistryURL: "https://api.example.com/v1/tools", Token: "x"},
		{Name: "health", RegistryURL: "", Token: "x"},
		{Name: "health", RegistryURL: "   ", Token: "x"},
		{Name: "health", RegistryURL: "not-a-url", Token: "x"},
		{Name: "health", RegistryURL: "ftp://api.example.com", Token: "x"},
		{Name: "health", RegistryURL: "https://api.example.com/v1/tools", Token: ""},
	}
	for _, cfg := range cases {
		if _, err := NewToolReg(cfg); err == nil {
			t.Errorf("expected config %+v to be refused", cfg)
		}
	}
}

// Tool names derive from the registry name, so two registries can coexist in
// one profile without colliding.
func TestToolNamesDeriveFromRegistryName(t *testing.T) {
	r, err := NewToolReg(ToolRegConfig{
		Name: "finance", RegistryURL: "https://api.invalid/tools", Token: "x",
	})
	if err != nil {
		t.Fatalf("NewToolReg: %v", err)
	}
	tools := r.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Def.Name != "finance_tools" || tools[1].Def.Name != "finance_tool" {
		t.Errorf("names = %q, %q", tools[0].Def.Name, tools[1].Def.Name)
	}
}

// The token authenticates every call; list GETs exactly the configured URL —
// no path beyond it is assumed.
func TestListSendsBearerToTheConfiguredURL(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tools":[{"name":"health.get_today"}]}`))
	}))
	defer server.Close()

	r := newToolReg(t, server.URL+"/v1/tools")
	out, err := r.handleList(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if gotAuth != "Bearer tok_test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/tools" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(out, "health.get_today") {
		t.Errorf("output missing entry name: %q", out)
	}
}

func TestInvokePostsWrappedInputUnderTheRegistry(t *testing.T) {
	var gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"local_health_day":"2026-08-15"}`))
	}))
	defer server.Close()

	r := newToolReg(t, server.URL+"/v1/tools")
	raw, _ := json.Marshal(map[string]any{
		"name":  "health.get_sleep",
		"input": map[string]any{"day": "2026-08-15"},
	})
	out, err := r.handleInvoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("handleInvoke: %v", err)
	}
	if gotPath != "/v1/tools/health.get_sleep" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"input"`) || !strings.Contains(gotBody, `"day"`) {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.Contains(out, "2026-08-15") {
		t.Errorf("output = %q", out)
	}
}

func TestInvokeOmittedInputBecomesEmptyObject(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	r := newToolReg(t, server.URL+"/v1/tools")
	if _, err := r.handleInvoke(context.Background(), json.RawMessage(`{"name":"connections.list"}`)); err != nil {
		t.Fatalf("handleInvoke: %v", err)
	}
	if gotBody != `{"input":{}}` {
		t.Errorf("body = %q", gotBody)
	}
}

// The entry name becomes a path segment; anything that could steer the
// request off the registry is refused before a request exists.
func TestInvokeRejectsPathSteeringNames(t *testing.T) {
	r := newToolReg(t, "https://api.invalid/v1/tools")
	for _, name := range []string{
		"",
		"../privacy",
		"health/../../v1/privacy/delete",
		"health.get_today?x=1",
		"a b",
		"x%2F..",
		"tools#frag",
	} {
		raw, _ := json.Marshal(map[string]string{"name": name})
		if _, err := r.handleInvoke(context.Background(), raw); err == nil {
			t.Errorf("expected name %q to be refused", name)
		}
	}
}

// A non-2xx becomes an error the model can read — bounded, with the typed
// code visible — never a success string.
func TestErrorStatusSurfacesBoundedSnippet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"detail":{"code":"forbidden"}}`))
	}))
	defer server.Close()

	r := newToolReg(t, server.URL+"/v1/tools")
	raw, _ := json.Marshal(map[string]string{"name": "connections.sync"})
	_, err := r.handleInvoke(context.Background(), raw)
	if err == nil {
		t.Fatal("expected an error for http 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error = %v", err)
	}
}

func TestOversizedResponseIsTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", maxToolRegBytes+100)))
	}))
	defer server.Close()

	r := newToolReg(t, server.URL+"/v1/tools")
	out, err := r.handleList(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if len(out) > maxToolRegBytes+len("\n[truncated]") {
		t.Errorf("output not truncated: %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "[truncated]") {
		t.Error("missing truncation marker")
	}
}
