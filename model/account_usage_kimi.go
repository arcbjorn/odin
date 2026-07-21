package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type kimiUsageBody struct {
	User struct {
		Region     string `json:"region"`
		Membership struct {
			Level string `json:"level"`
		} `json:"membership"`
	} `json:"user"`
	Usage  kimiUsageDetail `json:"usage"`
	Limits []struct {
		Name   string          `json:"name"`
		Title  string          `json:"title"`
		Scope  string          `json:"scope"`
		Window kimiUsageWindow `json:"window"`
		Detail kimiUsageDetail `json:"detail"`
	} `json:"limits"`
}

type kimiUsageWindow struct {
	Duration int    `json:"duration"`
	TimeUnit string `json:"timeUnit"`
}

type kimiUsageDetail struct {
	Name      string          `json:"name"`
	Title     string          `json:"title"`
	Limit     json.RawMessage `json:"limit"`
	Used      json.RawMessage `json:"used"`
	Remaining json.RawMessage `json:"remaining"`
	ResetTime json.RawMessage `json:"resetTime"`
	ResetAt   json.RawMessage `json:"reset_at"`
}

func (p *AccountUsageProvider) kimiUsage(ctx context.Context) (*AccountUsage, error) {
	if err := p.requireTokens(); err != nil {
		return nil, err
	}
	if p.baseURL == "" {
		return nil, fmt.Errorf("kimi usage has no base URL")
	}
	resp, err := doTokenRequest(ctx, p.http, p.tokens, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/usages", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
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
		return nil, fmt.Errorf("kimi usage: http %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}

	var body kimiUsageBody
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("decode kimi usage: %w", err)
	}
	now := time.Now()
	out := &AccountUsage{
		Provider:  p.provider,
		Plan:      kimiPlan(body.User.Membership.Level),
		FetchedAt: now,
	}
	appendKimiWindow(out, "Weekly", body.Usage)
	for i, limit := range body.Limits {
		label := firstNonEmpty(limit.Name, limit.Title, limit.Scope)
		if label == "" {
			label = kimiWindowLabel(limit.Window, i)
		}
		appendKimiWindow(out, label, limit.Detail)
	}
	if body.User.Region != "" {
		out.Details = append(out.Details, "Region: "+body.User.Region)
	}
	if len(out.Windows) == 0 {
		return nil, fmt.Errorf("decode kimi usage: response contains no quota windows")
	}
	return out, nil
}

func appendKimiWindow(out *AccountUsage, label string, detail kimiUsageDetail) {
	limit, hasLimit := rawNumber(detail.Limit)
	used, hasUsed := rawNumber(detail.Used)
	if !hasUsed {
		if remaining, ok := rawNumber(detail.Remaining); ok && hasLimit {
			used = limit - remaining
			hasUsed = true
		}
	}
	if !hasLimit || !hasUsed || limit <= 0 {
		return
	}
	reset := parseAccountTime(detail.ResetTime)
	if reset.IsZero() {
		reset = parseAccountTime(detail.ResetAt)
	}
	out.Windows = append(out.Windows, AccountUsageWindow{
		Label:       firstNonEmpty(detail.Name, detail.Title, label),
		UsedPercent: clampPercent(used / limit * 100),
		ResetAt:     reset,
	})
}

func rawNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return finiteFloat(number.String())
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	return finiteFloat(strings.TrimSpace(text))
}

func finiteFloat(value string) (float64, bool) {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

func kimiPlan(level string) string {
	level = strings.TrimPrefix(strings.TrimSpace(level), "LEVEL_")
	if level == "" {
		return "Kimi Code"
	}
	return "Kimi Code " + titleSlug(level)
}

func kimiWindowLabel(window kimiUsageWindow, index int) string {
	if window.Duration > 0 {
		unit := strings.ToUpper(window.TimeUnit)
		switch {
		case strings.Contains(unit, "MINUTE") && window.Duration%60 == 0:
			return fmt.Sprintf("%d-hour", window.Duration/60)
		case strings.Contains(unit, "MINUTE"):
			return fmt.Sprintf("%d-minute", window.Duration)
		case strings.Contains(unit, "HOUR"):
			return fmt.Sprintf("%d-hour", window.Duration)
		case strings.Contains(unit, "DAY"):
			return fmt.Sprintf("%d-day", window.Duration)
		}
	}
	return fmt.Sprintf("Limit %d", index+1)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
