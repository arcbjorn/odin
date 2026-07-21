package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	codexClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexTokenURL        = "https://auth.openai.com/oauth/token"
	xaiClientID          = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiDeviceURL         = "https://auth.x.ai/oauth2/device/code"
	xaiTokenURL          = "https://auth.x.ai/oauth2/token"
	xaiScope             = "openid profile email offline_access grok-cli:access api:access"
	claudeClientID       = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeTokenURL       = "https://platform.claude.com/v1/oauth/token"
	claudeRedirectURL    = "https://console.anthropic.com/oauth/code/callback"
	claudeScope          = "org:create_api_key user:profile user:inference"
	minimaxClientID      = "78257093-7e40-4613-99e0-527b14b39113"
	minimaxPortalURL     = "https://api.minimax.io"
	minimaxScope         = "group_id profile model.completion"
	minimaxUserCodeGrant = "urn:ietf:params:oauth:grant-type:user_code"
)

// LoginUI keeps browser interaction outside the model package. Odin can run
// headless: device flows only display a URL/code, while PKCE flows ask the
// operator to paste the code shown after authorization.
type LoginUI struct {
	DeviceCode        func(userCode, verifyURL string)
	AuthorizationCode func(authorizeURL string) (string, error)
}

// NewSubscriptionSource constructs the runtime token source for a supported
// first-party subscription flow.
func NewSubscriptionSource(subscription, path string) (TokenSource, error) {
	switch subscription {
	case "codex":
		return NewOAuthSource(OAuthConfig{
			Path: path, ClientID: codexClientID, TokenURL: codexTokenURL,
			Headers: map[string]string{"Accept": "application/json", "User-Agent": "odin/1"},
		}), nil
	case "claude":
		return NewOAuthSource(OAuthConfig{
			Path: path, ClientID: claudeClientID, TokenURL: claudeTokenURL,
			Headers: map[string]string{"Accept": "application/json", "User-Agent": "axios/1.7.9"},
		}), nil
	case "xai":
		return NewOAuthSource(OAuthConfig{
			Path: path, ClientID: xaiClientID, TokenURL: xaiTokenURL, Scope: xaiScope,
			RefreshSkew: time.Hour, Headers: map[string]string{"Accept": "application/json"},
		}), nil
	case "minimax":
		return NewOAuthSource(OAuthConfig{
			Path: path, ClientID: minimaxClientID, TokenURL: minimaxPortalURL + "/oauth/token",
			RefreshSkew: time.Minute, Headers: map[string]string{"Accept": "application/json"},
		}), nil
	default:
		return nil, fmt.Errorf("unknown subscription %q", subscription)
	}
}

// LoginSubscription obtains an independent profile-scoped credential.
func LoginSubscription(ctx context.Context, subscription, path string, ui LoginUI) error {
	switch subscription {
	case "codex":
		return loginCodex(ctx, path, ui.DeviceCode)
	case "claude":
		return loginClaude(ctx, path, ui.AuthorizationCode)
	case "xai":
		source, _ := NewSubscriptionSource(subscription, path)
		return source.(*OAuthSource).DeviceLogin(ctx, xaiDeviceURL, ui.DeviceCode)
	case "minimax":
		return loginMiniMax(ctx, path, ui.DeviceCode)
	default:
		return fmt.Errorf("unknown subscription %q", subscription)
	}
}

func loginCodex(ctx context.Context, path string, prompt func(string, string)) error {
	client := &http.Client{Timeout: 20 * time.Second}
	var start struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     any    `json:"interval"`
	}
	if err := postJSON(ctx, client, "https://auth.openai.com/api/accounts/deviceauth/usercode", map[string]string{
		"client_id": codexClientID,
	}, nil, &start); err != nil {
		return fmt.Errorf("request Codex device code: %w", err)
	}
	if start.DeviceAuthID == "" || start.UserCode == "" {
		return errors.New("Codex device response missing device_auth_id or user_code")
	}
	if prompt != nil {
		prompt(start.UserCode, "https://auth.openai.com/codex/device")
	}
	interval := 5 * time.Second
	switch value := start.Interval.(type) {
	case float64:
		interval = time.Duration(maxInt(3, int(value))) * time.Second
	case string:
		if parsed, err := strconv.Atoi(value); err == nil {
			interval = time.Duration(maxInt(3, parsed)) * time.Second
		}
	}

	deadline := time.Now().Add(15 * time.Minute)
	var exchange struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	for time.Now().Before(deadline) {
		if err := waitContext(ctx, interval); err != nil {
			return err
		}
		err := postJSON(ctx, client, "https://auth.openai.com/api/accounts/deviceauth/token", map[string]string{
			"device_auth_id": start.DeviceAuthID, "user_code": start.UserCode,
		}, nil, &exchange)
		if err == nil {
			if exchange.AuthorizationCode != "" && exchange.CodeVerifier != "" {
				break
			}
			return errors.New("Codex device response missing authorization_code or code_verifier")
		}
		var statusErr *httpStatusError
		if !errors.As(err, &statusErr) || (statusErr.Status != 403 && statusErr.Status != 404) {
			return fmt.Errorf("poll Codex device login: %w", err)
		}
	}
	if exchange.AuthorizationCode == "" {
		return errors.New("Codex device login timed out")
	}

	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {exchange.AuthorizationCode},
		"redirect_uri": {"https://auth.openai.com/deviceauth/callback"},
		"client_id":    {codexClientID}, "code_verifier": {exchange.CodeVerifier},
	}
	tok, err := exchangeToken(ctx, client, codexTokenURL, form, map[string]string{"User-Agent": "odin/1"})
	if err != nil {
		return fmt.Errorf("exchange Codex authorization: %w", err)
	}
	return persistLogin(path, tok)
}

func loginClaude(ctx context.Context, path string, prompt func(string) (string, error)) error {
	if prompt == nil {
		return errors.New("Claude login needs an authorization-code prompt")
	}
	verifier, challenge, err := pkcePair()
	if err != nil {
		return err
	}
	state, err := randomText(32)
	if err != nil {
		return err
	}
	params := url.Values{
		"code": {"true"}, "client_id": {claudeClientID}, "response_type": {"code"},
		"redirect_uri": {claudeRedirectURL}, "scope": {claudeScope},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state},
	}
	codeWithState, err := prompt("https://claude.ai/oauth/authorize?" + params.Encode())
	if err != nil {
		return err
	}
	code, receivedState, ok := strings.Cut(strings.TrimSpace(codeWithState), "#")
	if !ok || receivedState != state {
		return errors.New("Claude authorization state mismatch")
	}
	payload := map[string]string{
		"grant_type": "authorization_code", "client_id": claudeClientID, "code": code,
		"state": receivedState, "redirect_uri": claudeRedirectURL, "code_verifier": verifier,
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var parsed tokenResponse
	if err := postJSON(ctx, client, claudeTokenURL, payload, map[string]string{"User-Agent": "axios/1.7.9"}, &parsed); err != nil {
		return fmt.Errorf("exchange Claude authorization: %w", err)
	}
	tok := &OAuthToken{AccessToken: parsed.AccessToken, RefreshToken: parsed.RefreshToken, TokenType: parsed.TokenType}
	tok.ExpiresAt = tokenExpiry(parsed.ExpiresIn, parsed.ExpiredIn)
	if tok.AccessToken == "" {
		return errors.New("Claude token response missing access_token")
	}
	return persistLogin(path, tok)
}

func loginMiniMax(ctx context.Context, path string, prompt func(string, string)) error {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return err
	}
	state, err := randomText(16)
	if err != nil {
		return err
	}
	form := url.Values{
		"response_type": {"code"}, "client_id": {minimaxClientID}, "scope": {minimaxScope},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state},
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var start struct {
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		State           string `json:"state"`
		ExpiredIn       int64  `json:"expired_in"`
		Interval        int64  `json:"interval"`
	}
	if err := postForm(ctx, client, minimaxPortalURL+"/oauth/code", form, map[string]string{
		"Accept": "application/json", "x-request-id": requestID(),
	}, &start); err != nil {
		return fmt.Errorf("request MiniMax user code: %w", err)
	}
	if start.UserCode == "" || start.VerificationURI == "" || start.State != state {
		return errors.New("MiniMax authorization response is incomplete or has mismatched state")
	}
	if prompt != nil {
		prompt(start.UserCode, start.VerificationURI)
	}
	deadline := expiryFromExpiredIn(start.ExpiredIn)
	interval := 2 * time.Second
	if start.Interval > 0 {
		interval = maxDuration(2*time.Second, time.Duration(start.Interval)*time.Millisecond)
	}
	for time.Now().Before(deadline) {
		poll := url.Values{
			"grant_type": {minimaxUserCodeGrant}, "client_id": {minimaxClientID},
			"user_code": {start.UserCode}, "code_verifier": {verifier},
		}
		var parsed struct {
			Status       string `json:"status"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			TokenType    string `json:"token_type"`
			ExpiredIn    int64  `json:"expired_in"`
		}
		if err := postForm(ctx, client, minimaxPortalURL+"/oauth/token", poll, map[string]string{"Accept": "application/json"}, &parsed); err != nil {
			return fmt.Errorf("poll MiniMax login: %w", err)
		}
		if parsed.Status == "success" {
			tok := &OAuthToken{AccessToken: parsed.AccessToken, RefreshToken: parsed.RefreshToken, TokenType: parsed.TokenType, ExpiresAt: expiryFromExpiredIn(parsed.ExpiredIn)}
			if tok.AccessToken == "" || tok.RefreshToken == "" {
				return errors.New("MiniMax token response missing required tokens")
			}
			return persistLogin(path, tok)
		}
		if parsed.Status == "error" {
			return errors.New("MiniMax denied authorization")
		}
		if err := waitContext(ctx, interval); err != nil {
			return err
		}
	}
	return errors.New("MiniMax authorization timed out")
}

func persistLogin(path string, tok *OAuthToken) error {
	tok.LastRefresh = time.Now().UTC()
	if err := writeToken(path, tok); err != nil {
		return fmt.Errorf("persist credentials: %w", err)
	}
	return nil
}

type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, truncate(e.Body, 300))
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, body any, headers map[string]string, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(client, req, headers, out)
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doRequest(client, req, headers, out)
}

func doRequest(client *http.Client, req *http.Request, headers map[string]string, out any) error {
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{Status: resp.StatusCode, Body: string(raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func exchangeToken(ctx context.Context, client *http.Client, endpoint string, form url.Values, headers map[string]string) (*OAuthToken, error) {
	var parsed tokenResponse
	if err := postForm(ctx, client, endpoint, form, headers, &parsed); err != nil {
		return nil, err
	}
	if parsed.AccessToken == "" {
		return nil, errors.New("token response missing access_token")
	}
	return &OAuthToken{
		AccessToken: parsed.AccessToken, RefreshToken: parsed.RefreshToken,
		TokenType: parsed.TokenType, ExpiresAt: tokenExpiry(parsed.ExpiresIn, parsed.ExpiredIn),
	}, nil
}

func pkcePair() (string, string, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func randomText(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func requestID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(raw)
}

func expiryFromExpiredIn(value int64) time.Time {
	if value > time.Now().UnixMilli()/2 {
		return time.UnixMilli(value)
	}
	return time.Now().Add(time.Duration(maxInt64(1, value)) * time.Second)
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
