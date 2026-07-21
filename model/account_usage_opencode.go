package model

import (
	"context"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxDashboardResetSeconds = 370 * 24 * 60 * 60

var (
	openCodeWorkspaceIDPattern  = regexp.MustCompile(`^wrk_[A-Za-z0-9]+$`)
	openCodeUsagePercentPattern = regexp.MustCompile(`["']?usagePercent["']?\s*:\s*"?(-?\d+(?:\.\d+)?)"?`)
	openCodeResetSecondsPattern = regexp.MustCompile(`["']?resetInSec["']?\s*:\s*"?(-?\d+(?:\.\d+)?)"?`)
	openCodeDashboardFields     = []struct {
		label   string
		pattern *regexp.Regexp
	}{
		{label: "5-hour", pattern: dashboardObjectPattern("rollingUsage")},
		{label: "Weekly", pattern: dashboardObjectPattern("weeklyUsage")},
		{label: "Monthly", pattern: dashboardObjectPattern("monthlyUsage")},
	}
)

func dashboardObjectPattern(name string) *regexp.Regexp {
	pattern := `["']?` + regexp.QuoteMeta(name) + `["']?\s*:\s*(?:\$R\[\d+\]\s*=\s*)?\{([^{}]*)\}`
	return regexp.MustCompile(`(?s)` + pattern)
}

func (p *AccountUsageProvider) openCodeGoUsage(ctx context.Context) (*AccountUsage, error) {
	if p.workspaceID == "" || p.dashboardCookie == "" {
		return nil, fmt.Errorf("opencode go usage requires OPENCODE_GO_WORKSPACE_ID and OPENCODE_GO_AUTH_COOKIE")
	}
	if !openCodeWorkspaceIDPattern.MatchString(p.workspaceID) {
		return nil, fmt.Errorf("opencode go usage: invalid workspace id")
	}
	endpoint := "https://opencode.ai/workspace/" + p.workspaceID + "/go"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Cookie", openCodeCookieHeader(p.dashboardCookie))
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; odin/1)")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opencode go usage: http %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}
	now := time.Now()
	windows, err := parseOpenCodeGoUsage(string(payload), now)
	if err != nil {
		return nil, err
	}
	return &AccountUsage{
		Provider:  p.provider,
		Plan:      "OpenCode Go",
		FetchedAt: now,
		Windows:   windows,
	}, nil
}

func openCodeCookieHeader(raw string) string {
	for _, part := range strings.Split(raw, ";") {
		name, _, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && name == "auth" {
			return raw
		}
	}
	return "auth=" + raw
}

func parseOpenCodeGoUsage(document string, now time.Time) ([]AccountUsageWindow, error) {
	document = html.UnescapeString(document)
	document = strings.NewReplacer(`\"`, `"`, `\u0022`, `"`).Replace(document)
	var windows []AccountUsageWindow
	for _, field := range openCodeDashboardFields {
		match := field.pattern.FindStringSubmatch(document)
		if len(match) != 2 {
			continue
		}
		used, usedOK := captureDashboardNumber(openCodeUsagePercentPattern, match[1])
		resetSeconds, resetOK := captureDashboardNumber(openCodeResetSecondsPattern, match[1])
		if !usedOK || !resetOK || resetSeconds < 0 || resetSeconds > maxDashboardResetSeconds {
			continue
		}
		windows = append(windows, AccountUsageWindow{
			Label:       field.label,
			UsedPercent: clampPercent(used),
			ResetAt:     now.Add(time.Duration(math.Round(resetSeconds)) * time.Second),
		})
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("decode opencode go usage: dashboard contains no quota windows")
	}
	return windows, nil
}

func captureDashboardNumber(pattern *regexp.Regexp, body string) (float64, bool) {
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.ParseFloat(match[1], 64)
	return value, err == nil
}
