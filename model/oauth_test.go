package model

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOAuthRefreshRotatesTokenAndForwardsHeaders(t *testing.T) {
	path := t.TempDir() + "/provider.json"
	if err := writeToken(path, &OAuthToken{
		AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	source := NewOAuthSource(OAuthConfig{
		Path: path, ClientID: "client", TokenURL: "https://auth.test/token",
		Headers: map[string]string{"User-Agent": "provider-client/1"},
	})
	source.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("User-Agent") != "provider-client/1" {
			t.Fatalf("user-agent = %q", req.Header.Get("User-Agent"))
		}
		raw, _ := url.QueryUnescape(readBody(t, req))
		if !strings.Contains(raw, "refresh_token=old-refresh") || !strings.Contains(raw, "client_id=client") {
			t.Fatalf("form = %q", raw)
		}
		return jsonResponse(http.StatusOK, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`), nil
	})}

	got, err := source.Token(context.Background())
	if err != nil || got != "new-access" {
		t.Fatalf("token=%q err=%v", got, err)
	}
	raw, err := readToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if raw.RefreshToken != "new-refresh" || raw.LastRefresh.IsZero() {
		t.Fatalf("persisted token = %+v", raw)
	}
}

func TestOAuthRecoverUsesTokenRotatedByAnotherProcess(t *testing.T) {
	path := t.TempDir() + "/provider.json"
	if err := writeToken(path, &OAuthToken{
		AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	source := NewOAuthSource(OAuthConfig{
		Path: path, ClientID: "client", TokenURL: "https://auth.test/token",
	})
	source.cached = &OAuthToken{AccessToken: "old-access", RefreshToken: "old-refresh"}
	source.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("recovery should use the durable token without refreshing")
		return nil, nil
	})}

	got, retry, err := source.Recover(context.Background(), http.StatusUnauthorized, "old-access")
	if err != nil || !retry || got != "new-access" {
		t.Fatalf("token=%q retry=%t err=%v", got, retry, err)
	}
}

func TestOAuthRecoverForceRefreshesRejectedToken(t *testing.T) {
	path := t.TempDir() + "/provider.json"
	if err := writeToken(path, &OAuthToken{
		AccessToken: "rejected", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	source := NewOAuthSource(OAuthConfig{
		Path: path, ClientID: "client", TokenURL: "https://auth.test/token",
	})
	source.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := url.QueryUnescape(readBody(t, req))
		if !strings.Contains(raw, "refresh_token=old-refresh") {
			t.Fatalf("form = %q", raw)
		}
		return jsonResponse(http.StatusOK, `{"access_token":"replacement","refresh_token":"new-refresh","expires_in":3600}`), nil
	})}

	got, retry, err := source.Recover(context.Background(), http.StatusUnauthorized, "rejected")
	if err != nil || !retry || got != "replacement" {
		t.Fatalf("token=%q retry=%t err=%v", got, retry, err)
	}
	persisted, err := readToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "replacement" || persisted.RefreshToken != "new-refresh" {
		t.Fatalf("persisted token = %+v", persisted)
	}
}

func readBody(t *testing.T, req *http.Request) string {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
