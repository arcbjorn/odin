package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitBucket is one provider quota window captured from response headers.
type RateLimitBucket struct {
	Limit      int
	Remaining  int
	ResetAfter time.Duration
	CapturedAt time.Time
}

// RateLimit contains the common minute and hourly request/token quotas.
type RateLimit struct {
	Provider     string
	CapturedAt   time.Time
	RequestsMin  RateLimitBucket
	RequestsHour RateLimitBucket
	TokensMin    RateLimitBucket
	TokensHour   RateLimitBucket
}

// AccountUsageWindow is one subscription quota window.
type AccountUsageWindow struct {
	Label       string
	UsedPercent float64
	ResetAt     time.Time
}

// AccountUsage is a provider subscription usage snapshot.
type AccountUsage struct {
	Provider  string
	Plan      string
	FetchedAt time.Time
	Windows   []AccountUsageWindow
	Details   []string
}

// AccountUsageReporter is implemented by subscription transports that expose
// an account-level usage endpoint.
type AccountUsageReporter interface {
	Provider
	AccountUsage(ctx context.Context) (*AccountUsage, error)
}

// ErrUsageUnsupported means the provider has no known account-usage endpoint.
var ErrUsageUnsupported = errors.New("account usage is not supported")

func parseRateLimitHeaders(headers http.Header, provider string) *RateLimit {
	hasAny := false
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), "x-ratelimit-") {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return nil
	}
	now := time.Now()
	bucket := func(resource, suffix string) RateLimitBucket {
		tag := resource + suffix
		return RateLimitBucket{
			Limit:      headerInt(headers.Get("x-ratelimit-limit-" + tag)),
			Remaining:  headerInt(headers.Get("x-ratelimit-remaining-" + tag)),
			ResetAfter: headerDuration(headers.Get("x-ratelimit-reset-" + tag)),
			CapturedAt: now,
		}
	}
	return &RateLimit{
		Provider: provider, CapturedAt: now,
		RequestsMin: bucket("requests", ""), RequestsHour: bucket("requests", "-1h"),
		TokensMin: bucket("tokens", ""), TokensHour: bucket("tokens", "-1h"),
	}
}

func headerInt(value string) int {
	n, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return int(n)
}

func headerDuration(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return duration
	}
	seconds, _ := strconv.ParseFloat(value, 64)
	return time.Duration(seconds * float64(time.Second))
}

// FormatRateLimitCompact renders the populated rate-limit buckets in one line.
func FormatRateLimitCompact(limit *RateLimit) string {
	if limit == nil {
		return ""
	}
	var parts []string
	add := func(label string, bucket RateLimitBucket) {
		if bucket.Limit > 0 {
			parts = append(parts, fmt.Sprintf("%s %d/%d", label, bucket.Remaining, bucket.Limit))
		}
	}
	add("RPM", limit.RequestsMin)
	add("RPH", limit.RequestsHour)
	add("TPM", limit.TokensMin)
	add("TPH", limit.TokensHour)
	return strings.Join(parts, "; ")
}

func (r *Responses) AccountUsage(ctx context.Context) (*AccountUsage, error) {
	if !r.codex {
		return nil, ErrUsageUnsupported
	}
	base := strings.TrimRight(r.baseURL, "/")
	base = strings.TrimSuffix(base, "/codex")
	prefix := base + "/api/codex"
	if strings.Contains(base, "/backend-api") {
		prefix = base + "/wham"
	}
	resp, err := doTokenRequest(ctx, r.http, r.tokens, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, prefix+"/usage", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "codex-cli")
		if accountID := jwtStringClaim(token, "https://api.openai.com/auth", "chatgpt_account_id"); accountID != "" {
			req.Header.Set("ChatGPT-Account-ID", accountID)
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage: http %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}
	var body struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   accountWindow `json:"primary_window"`
			Secondary accountWindow `json:"secondary_window"`
		} `json:"rate_limit"`
		ResetCredits struct {
			Available int `json:"available_count"`
		} `json:"rate_limit_reset_credits"`
		Credits struct {
			Has       bool    `json:"has_credits"`
			Unlimited bool    `json:"unlimited"`
			Balance   float64 `json:"balance"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("decode codex usage: %w", err)
	}
	out := &AccountUsage{Provider: r.provider, Plan: titleSlug(body.PlanType), FetchedAt: time.Now()}
	appendAccountWindow(out, "Session", body.RateLimit.Primary)
	appendAccountWindow(out, "Weekly", body.RateLimit.Secondary)
	if body.ResetCredits.Available > 0 {
		out.Details = append(out.Details, fmt.Sprintf("Banked resets: %d", body.ResetCredits.Available))
	}
	if body.Credits.Has {
		if body.Credits.Unlimited {
			out.Details = append(out.Details, "Credits: unlimited")
		} else {
			out.Details = append(out.Details, fmt.Sprintf("Credits: $%.2f", body.Credits.Balance))
		}
	}
	return out, nil
}

func (a *Anthropic) AccountUsage(ctx context.Context) (*AccountUsage, error) {
	if !a.oauthIdentity {
		return nil, ErrUsageUnsupported
	}
	resp, err := doTokenRequest(ctx, a.http, a.tokens, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		if a.userAgent != "" {
			req.Header.Set("User-Agent", a.userAgent)
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude usage: http %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}
	var body struct {
		FiveHour       accountWindow `json:"five_hour"`
		SevenDay       accountWindow `json:"seven_day"`
		SevenDayOpus   accountWindow `json:"seven_day_opus"`
		SevenDaySonnet accountWindow `json:"seven_day_sonnet"`
		ExtraUsage     struct {
			Enabled      bool    `json:"is_enabled"`
			UsedCredits  float64 `json:"used_credits"`
			MonthlyLimit float64 `json:"monthly_limit"`
			Currency     string  `json:"currency"`
		} `json:"extra_usage"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("decode claude usage: %w", err)
	}
	out := &AccountUsage{Provider: a.provider, FetchedAt: time.Now()}
	appendAccountWindow(out, "Current session", body.FiveHour)
	appendAccountWindow(out, "Current week", body.SevenDay)
	appendAccountWindow(out, "Opus week", body.SevenDayOpus)
	appendAccountWindow(out, "Sonnet week", body.SevenDaySonnet)
	if body.ExtraUsage.Enabled {
		currency := body.ExtraUsage.Currency
		if currency == "" {
			currency = "USD"
		}
		out.Details = append(out.Details, fmt.Sprintf("Extra usage: %.2f / %.2f %s", body.ExtraUsage.UsedCredits, body.ExtraUsage.MonthlyLimit, currency))
	}
	return out, nil
}

type accountWindow struct {
	UsedPercent *float64        `json:"used_percent"`
	Utilization *float64        `json:"utilization"`
	ResetAt     json.RawMessage `json:"reset_at"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

func appendAccountWindow(out *AccountUsage, label string, window accountWindow) {
	used := window.UsedPercent
	if used == nil {
		used = window.Utilization
		if used != nil && *used <= 1 {
			value := *used * 100
			used = &value
		}
	}
	if used == nil {
		return
	}
	reset := parseAccountTime(window.ResetAt)
	if reset.IsZero() {
		reset = parseAccountTime(window.ResetsAt)
	}
	out.Windows = append(out.Windows, AccountUsageWindow{Label: label, UsedPercent: *used, ResetAt: reset})
}

func parseAccountTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return time.Time{}
	}
	var stamp string
	if json.Unmarshal(raw, &stamp) == nil {
		if parsed, err := time.Parse(time.RFC3339, stamp); err == nil {
			return parsed
		}
		return time.Time{}
	}
	var seconds float64
	if json.Unmarshal(raw, &seconds) == nil {
		whole := int64(seconds)
		return time.Unix(whole, int64((seconds-float64(whole))*float64(time.Second))).UTC()
	}
	return time.Time{}
}

func titleSlug(value string) string {
	parts := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(value))
	for i := range parts {
		switch strings.ToLower(parts[i]) {
		case "chatgpt":
			parts[i] = "ChatGPT"
		case "api":
			parts[i] = "API"
		default:
			parts[i] = strings.ToUpper(parts[i][:1]) + strings.ToLower(parts[i][1:])
		}
	}
	return strings.Join(parts, " ")
}
