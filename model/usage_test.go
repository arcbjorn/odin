package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseRateLimitHeaders(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-ratelimit-limit-requests", "100")
	headers.Set("x-ratelimit-remaining-requests", "73")
	headers.Set("x-ratelimit-reset-requests", "45.5")
	headers.Set("x-ratelimit-limit-tokens-1h", "20000")
	headers.Set("x-ratelimit-remaining-tokens-1h", "12500")
	headers.Set("x-ratelimit-reset-tokens-1h", "32m")

	limit := parseRateLimitHeaders(headers, "test")
	if limit == nil {
		t.Fatal("expected rate-limit data")
	}
	if limit.RequestsMin.Limit != 100 || limit.RequestsMin.Remaining != 73 || limit.RequestsMin.ResetAfter != 45500*time.Millisecond {
		t.Fatalf("requests/min = %+v", limit.RequestsMin)
	}
	if limit.TokensHour.Limit != 20000 || limit.TokensHour.Remaining != 12500 || limit.TokensHour.ResetAfter != 32*time.Minute {
		t.Fatalf("tokens/hour = %+v", limit.TokensHour)
	}
	if got := FormatRateLimitCompact(limit); got != "RPM 73/100; TPH 12500/20000" {
		t.Fatalf("compact = %q", got)
	}
}

func TestCodexAccountUsage(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-test"},
	})
	token := "x." + base64.RawURLEncoding.EncodeToString(claims) + ".y"
	provider := NewResponses(ResponsesConfig{
		Provider: "codex", Model: "gpt-5", BaseURL: "https://chatgpt.test/backend-api/codex",
		Tokens: StaticToken(token), Codex: true,
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		if req.Header.Get("ChatGPT-Account-ID") != "acct-test" {
			t.Fatalf("account header = %q", req.Header.Get("ChatGPT-Account-ID"))
		}
		return jsonResponse(http.StatusOK, `{
			"plan_type":"chatgpt_plus",
			"rate_limit":{"primary_window":{"used_percent":25,"reset_at":1900000000},"secondary_window":{"used_percent":40,"reset_at":1900100000}},
			"rate_limit_reset_credits":{"available_count":2},
			"credits":{"has_credits":true,"balance":7.5}
		}`), nil
	})}

	usage, err := provider.AccountUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Plan != "ChatGPT Plus" || len(usage.Windows) != 2 || usage.Windows[0].UsedPercent != 25 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Windows[0].ResetAt.Unix() != 1900000000 || strings.Join(usage.Details, ",") != "Banked resets: 2,Credits: $7.50" {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestClaudeAccountUsage(t *testing.T) {
	provider := NewAnthropic(AnthropicConfig{
		Provider: "claude", Model: "claude-opus-4-8", Tokens: StaticToken("token"),
		Bearer: true, OAuthIdentity: true, UserAgent: "claude-code/test",
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.anthropic.com/api/oauth/usage" {
			t.Fatalf("url = %s", req.URL)
		}
		if req.Header.Get("anthropic-beta") != "oauth-2025-04-20" || req.Header.Get("User-Agent") != "claude-code/test" {
			t.Fatalf("headers = %#v", req.Header)
		}
		return jsonResponse(http.StatusOK, `{
			"five_hour":{"utilization":0.5,"resets_at":"2030-01-02T03:04:05Z"},
			"seven_day":{"utilization":75},
			"extra_usage":{"is_enabled":true,"used_credits":1.5,"monthly_limit":10,"currency":"USD"}
		}`), nil
	})}

	usage, err := provider.AccountUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Windows) != 2 || usage.Windows[0].UsedPercent != 50 || usage.Windows[1].UsedPercent != 75 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(usage.Details) != 1 || usage.Details[0] != "Extra usage: 1.50 / 10.00 USD" {
		t.Fatalf("details = %#v", usage.Details)
	}
}

func TestKimiAccountUsage(t *testing.T) {
	provider := testUsageProvider(AccountUsageKimi, "https://kimi.test/coding/v1", "sk-kimi-test")
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != "https://kimi.test/coding/v1/usages" {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer sk-kimi-test" {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		return jsonResponse(http.StatusOK, `{
			"user":{"region":"global","membership":{"level":"LEVEL_ULTRA"}},
			"usage":{"limit":"1000","used":"250","remaining":"750","resetTime":"2030-01-02T03:04:05Z"},
			"limits":[{"window":{"duration":300,"timeUnit":"MINUTE"},"detail":{"limit":"100","remaining":"60","resetTime":"2030-01-01T01:00:00Z"}}]
		}`), nil
	})}

	usage, err := provider.AccountUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Plan != "Kimi Code Ultra" || len(usage.Windows) != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Windows[0].Label != "Weekly" || usage.Windows[0].UsedPercent != 25 {
		t.Fatalf("weekly = %+v", usage.Windows[0])
	}
	if usage.Windows[1].Label != "5-hour" || usage.Windows[1].UsedPercent != 40 {
		t.Fatalf("rolling = %+v", usage.Windows[1])
	}
	if usage.Windows[0].ResetAt.Format(time.RFC3339) != "2030-01-02T03:04:05Z" {
		t.Fatalf("weekly reset = %s", usage.Windows[0].ResetAt)
	}
}

func TestGrokAccountUsage(t *testing.T) {
	provider := testUsageProvider(AccountUsageGrok, "https://unused.test/v1", "grok-token")
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig" {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer grok-token" || req.Header.Get("x-grpc-web") != "1" {
			t.Fatalf("headers = %#v", req.Header)
		}
		body, _ := io.ReadAll(req.Body)
		if !bytes.Equal(body, []byte{0, 0, 0, 0, 0}) {
			t.Fatalf("body = %v", body)
		}

		inner := append(protoFixed32Field(1, 42.5), protoLengthField(5, protoVarintField(1, 2_000_000_000))...)
		message := protoLengthField(1, inner)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"grpc-status": []string{"0"}},
			Body:       io.NopCloser(bytes.NewReader(grpcTestFrame(0, message))),
		}
		return resp, nil
	})}

	usage, err := provider.AccountUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Plan != "Grok subscription" || len(usage.Windows) != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Windows[0].UsedPercent != 42.5 || usage.Windows[0].ResetAt.Unix() != 2_000_000_000 {
		t.Fatalf("monthly = %+v", usage.Windows[0])
	}
}

func TestGrokAccountUsageRejectsGRPCTrailerError(t *testing.T) {
	provider := testUsageProvider(AccountUsageGrok, "https://unused.test/v1", "grok-token")
	provider.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		trailer := grpcTestFrame(0x80, []byte("grpc-status: 16\r\ngrpc-message: Invalid%20token\r\n"))
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(trailer))}, nil
	})}

	_, err := provider.AccountUsage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Invalid token") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenCodeGoAccountUsage(t *testing.T) {
	base := NewOpenAI(OpenAIConfig{Provider: "go", Model: "glm-5", BaseURL: "https://opencode.ai/zen/go/v1", Tokens: StaticToken("api-key")})
	provider := WithAccountUsage(base, AccountUsageConfig{
		Kind: AccountUsageOpenCodeGo, Provider: "go", Tokens: StaticToken("api-key"),
		WorkspaceID: "wrk_01ABCDEF", DashboardCookie: "cookie-value",
	})
	provider.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://opencode.ai/workspace/wrk_01ABCDEF/go" {
			t.Fatalf("url = %s", req.URL)
		}
		if req.Header.Get("Cookie") != "auth=cookie-value" {
			t.Fatalf("cookie = %q", req.Header.Get("Cookie"))
		}
		document := `<script>self.__next_f.push([1,"{\"rollingUsage\":{\"usagePercent\":12.5,\"resetInSec\":3600},\"weeklyUsage\":{\"usagePercent\":\"25\",\"resetInSec\":\"7200\"},\"monthlyUsage\":{\"usagePercent\":50,\"resetInSec\":10800}}"])</script>`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(document))}, nil
	})}

	before := time.Now()
	usage, err := provider.AccountUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Plan != "OpenCode Go" || len(usage.Windows) != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Windows[0].Label != "5-hour" || usage.Windows[0].UsedPercent != 12.5 ||
		usage.Windows[1].UsedPercent != 25 || usage.Windows[2].UsedPercent != 50 {
		t.Fatalf("windows = %+v", usage.Windows)
	}
	if usage.Windows[0].ResetAt.Before(before.Add(3599 * time.Second)) {
		t.Fatalf("rolling reset = %s", usage.Windows[0].ResetAt)
	}
}

func TestOpenCodeGoUsageRequiresExplicitDashboardAuth(t *testing.T) {
	provider := testUsageProvider(AccountUsageOpenCodeGo, "https://opencode.ai/zen/go/v1", "api-key")
	_, err := provider.AccountUsage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "OPENCODE_GO_WORKSPACE_ID") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenCodeGoParserReadsResourceReferences(t *testing.T) {
	document := `<script>$R[24]($R[18],$R[30]={rollingUsage:$R[31]={status:"ok",resetInSec:18000,usagePercent:0},weeklyUsage:$R[32]={status:"ok",resetInSec:162822,usagePercent:31},monthlyUsage:$R[33]={status:"ok",resetInSec:1404782,usagePercent:21}});</script>`
	now := time.Unix(1_800_000_000, 0)
	windows, err := parseOpenCodeGoUsage(document, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 || windows[0].UsedPercent != 0 || windows[1].UsedPercent != 31 || windows[2].UsedPercent != 21 {
		t.Fatalf("windows = %+v", windows)
	}
	if !windows[0].ResetAt.Equal(now.Add(5 * time.Hour)) {
		t.Fatalf("rolling reset = %s", windows[0].ResetAt)
	}
}

func TestGrokBillingZeroUsageMarker(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := uint64(now.Add(24 * time.Hour).Unix())
	inner := append(protoLengthField(5, protoVarintField(1, reset)), protoLengthField(6, protoVarintField(1, 1))...)
	used, resetAt, err := parseGrokBilling(grpcTestFrame(0, protoLengthField(1, inner)), now)
	if err != nil {
		t.Fatal(err)
	}
	if used != 0 || resetAt.Unix() != int64(reset) {
		t.Fatalf("used = %v, reset = %s", used, resetAt)
	}
}

func TestOpenCodeCookieHeader(t *testing.T) {
	tests := map[string]string{
		"token":                   "auth=token",
		"auth=token":              "auth=token",
		"other=value; auth=token": "other=value; auth=token",
		"token-containing-auth=x": "auth=token-containing-auth=x",
	}
	for input, want := range tests {
		if got := openCodeCookieHeader(input); got != want {
			t.Errorf("cookie header for %q = %q, want %q", input, got, want)
		}
	}
}

func TestUsageNumberRejectsNonFiniteValues(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`"NaN"`), json.RawMessage(`"+Inf"`)} {
		if value, ok := rawNumber(raw); ok {
			t.Errorf("rawNumber(%s) = %v, true", raw, value)
		}
	}
}

func TestAccountUsageWrapperPreservesModelCatalog(t *testing.T) {
	base := NewOpenAI(OpenAIConfig{
		Provider: "kimi", Model: "k3", BaseURL: "https://kimi.test/v1", Tokens: StaticToken("token"),
	})
	base.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://kimi.test/v1/models" {
			t.Fatalf("url = %s", req.URL)
		}
		return jsonResponse(http.StatusOK, `{"data":[{"id":"k3"}]}`), nil
	})}
	provider := WithAccountUsage(base, AccountUsageConfig{
		Kind: AccountUsageKimi, Provider: "kimi", BaseURL: "https://kimi.test/v1", Tokens: StaticToken("token"),
	})

	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "k3" {
		t.Fatalf("models = %v", models)
	}
}

func testUsageProvider(kind AccountUsageKind, baseURL, token string) *AccountUsageProvider {
	providerName := string(kind)
	base := NewOpenAI(OpenAIConfig{Provider: providerName, Model: "test", BaseURL: baseURL, Tokens: StaticToken(token)})
	return WithAccountUsage(base, AccountUsageConfig{
		Kind: kind, Provider: providerName, BaseURL: baseURL, Tokens: StaticToken(token),
	})
}

func protoVarintBytes(value uint64) []byte {
	var data []byte
	for value >= 0x80 {
		data = append(data, byte(value&0x7f)|0x80)
		value >>= 7
	}
	return append(data, byte(value))
}

func protoVarintField(field byte, value uint64) []byte {
	return append([]byte{field << 3}, protoVarintBytes(value)...)
}

func protoLengthField(field byte, payload []byte) []byte {
	data := append([]byte{field<<3 | 2}, protoVarintBytes(uint64(len(payload)))...)
	return append(data, payload...)
}

func protoFixed32Field(field byte, value float32) []byte {
	data := []byte{field<<3 | 5, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(data[1:], math.Float32bits(value))
	return data
}

func grpcTestFrame(flags byte, payload []byte) []byte {
	data := make([]byte, 5, 5+len(payload))
	data[0] = flags
	binary.BigEndian.PutUint32(data[1:], uint32(len(payload)))
	return append(data, payload...)
}
