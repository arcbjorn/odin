package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAkunaki(t *testing.T, baseURL string) *Akunaki {
	t.Helper()
	a, err := NewAkunaki(AkunakiConfig{BaseURL: baseURL, Token: "aksvc_test"})
	if err != nil {
		t.Fatalf("NewAkunaki: %v", err)
	}
	return a
}

func TestNewAkunakiRequiresBaseURLAndToken(t *testing.T) {
	cases := []AkunakiConfig{
		{BaseURL: "", Token: "aksvc_x"},
		{BaseURL: "https://api.example.com", Token: ""},
		{BaseURL: "   ", Token: "aksvc_x"},
		{BaseURL: "not-a-url", Token: "aksvc_x"},
		{BaseURL: "ftp://api.example.com", Token: "aksvc_x"},
	}
	for _, cfg := range cases {
		if _, err := NewAkunaki(cfg); err == nil {
			t.Errorf("expected config %+v to be refused", cfg)
		}
	}
}

// The token authenticates every call; the list call carries no body.
func TestListSendsBearerAndReturnsBody(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tools":[{"name":"health.get_today"}]}`))
	}))
	defer server.Close()

	a := newAkunaki(t, server.URL)
	out, err := a.handleList(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if gotAuth != "Bearer aksvc_test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/tools" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(out, "health.get_today") {
		t.Errorf("output missing tool name: %q", out)
	}
}

func TestInvokePostsWrappedInput(t *testing.T) {
	var gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"local_health_day":"2026-08-15"}`))
	}))
	defer server.Close()

	a := newAkunaki(t, server.URL)
	raw, _ := json.Marshal(map[string]any{
		"name":  "health.get_sleep",
		"input": map[string]any{"day": "2026-08-15"},
	})
	out, err := a.handleInvoke(context.Background(), raw)
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

	a := newAkunaki(t, server.URL)
	if _, err := a.handleInvoke(context.Background(), json.RawMessage(`{"name":"connections.list"}`)); err != nil {
		t.Fatalf("handleInvoke: %v", err)
	}
	if gotBody != `{"input":{}}` {
		t.Errorf("body = %q", gotBody)
	}
}

// The name becomes a path segment; anything that could steer the request off
// /v1/tools/ is refused before a request exists.
func TestInvokeRejectsPathSteeringNames(t *testing.T) {
	a := newAkunaki(t, "https://api.invalid")
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
		if _, err := a.handleInvoke(context.Background(), raw); err == nil {
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

	a := newAkunaki(t, server.URL)
	raw, _ := json.Marshal(map[string]string{"name": "connections.sync"})
	_, err := a.handleInvoke(context.Background(), raw)
	if err == nil {
		t.Fatal("expected an error for http 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error = %v", err)
	}
}

func TestOversizedResponseIsTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", maxAkunakiBytes+100)))
	}))
	defer server.Close()

	a := newAkunaki(t, server.URL)
	out, err := a.handleList(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if len(out) > maxAkunakiBytes+len("\n[truncated]") {
		t.Errorf("output not truncated: %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "[truncated]") {
		t.Error("missing truncation marker")
	}
}

func TestToolsAreExactlyListAndInvoke(t *testing.T) {
	a := newAkunaki(t, "https://api.invalid")
	tools := a.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	names := []string{tools[0].Def.Name, tools[1].Def.Name}
	if names[0] != "health_tools" || names[1] != "health_tool" {
		t.Errorf("names = %v", names)
	}
}
