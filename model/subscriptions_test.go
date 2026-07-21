package model

import (
	"testing"
	"time"
)

func TestSubscriptionSourcesUseProviderRefreshPolicy(t *testing.T) {
	for _, test := range []struct {
		name string
		skew time.Duration
	}{
		{name: "codex", skew: 2 * time.Minute},
		{name: "claude", skew: 2 * time.Minute},
		{name: "xai", skew: time.Hour},
		{name: "minimax", skew: time.Minute},
	} {
		source, err := NewSubscriptionSource(test.name, "/tmp/not-read.json")
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		oauth := source.(*OAuthSource)
		if oauth.skew != test.skew || oauth.clientID == "" || oauth.tokenURL == "" {
			t.Fatalf("%s source = %+v", test.name, oauth)
		}
	}
}

func TestPlanKeysHaveNoOAuthSubscriptionSource(t *testing.T) {
	for _, subscription := range []string{"qwen", "kimi"} {
		if _, err := NewSubscriptionSource(subscription, "/tmp/not-read.json"); err == nil {
			t.Fatalf("%s plan must not use an OAuth credential source", subscription)
		}
	}
}

func TestTokenExpiryAcceptsMiniMaxAbsoluteMilliseconds(t *testing.T) {
	want := time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)
	got := tokenExpiry(0, want.UnixMilli())
	if !got.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got, want)
	}
}
