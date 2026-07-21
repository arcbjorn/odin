package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// refreshSkew refreshes this far before actual expiry, so a token doesn't
// expire in flight between the check and the API call.
const refreshSkew = 120 * time.Second

// OAuthToken is the on-disk credential record. Written 0600, never logged.
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type,omitempty"`
	LastRefresh  time.Time `json:"last_refresh,omitempty"`
}

func (t *OAuthToken) expiring(skew time.Duration) bool {
	if t.AccessToken == "" {
		return true
	}
	if t.ExpiresAt.IsZero() {
		return false // no expiry recorded; assume valid until a 401 says otherwise
	}
	return time.Now().Add(skew).After(t.ExpiresAt)
}

// OAuthSource is a TokenSource backed by a refresh-token grant, with the token
// persisted to disk.
//
// Access tokens are short-lived (xAI issues 6h), so refresh sits on the hot
// path: every scheduled job will hit an expired token. Two things can run
// concurrently — the scheduler and the chat gateway — so refresh must be
// single-writer. If the provider rotates refresh tokens on use (common with
// offline_access), the loser of an unsynchronized race gets a token that was
// already consumed and is permanently invalid, which means the agent dies
// silently at 4am and only recovers via a manual re-auth.
//
// Two locks, deliberately: a process-local mutex, and an flock on a sidecar
// file so a second odin process (or `odin auth status`) can't race either.
type OAuthSource struct {
	path     string
	clientID string
	tokenURL string
	scope    string
	skew     time.Duration
	headers  map[string]string
	http     *http.Client

	mu     sync.Mutex
	cached *OAuthToken
}

// OAuthConfig configures an OAuth token source.
type OAuthConfig struct {
	// Path is the credential file, e.g. <profile>/auth/xai.json.
	Path     string
	ClientID string
	TokenURL string
	Scope    string
	// RefreshSkew overrides the default two-minute proactive refresh window.
	RefreshSkew time.Duration
	// Headers are added to device, token, and refresh requests. This is needed
	// by providers that validate the OAuth client's user-agent.
	Headers map[string]string
	Timeout time.Duration
}

// NewOAuthSource builds an OAuth-backed TokenSource. It does not touch disk
// until the first Token call.
func NewOAuthSource(cfg OAuthConfig) *OAuthSource {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	skew := cfg.RefreshSkew
	if skew == 0 {
		skew = refreshSkew
	}
	return &OAuthSource{
		path:     cfg.Path,
		clientID: cfg.ClientID,
		tokenURL: cfg.TokenURL,
		scope:    cfg.Scope,
		skew:     skew,
		headers:  cloneStrings(cfg.Headers),
		http:     &http.Client{Timeout: timeout},
	}
}

// Token returns a valid access token, refreshing if it is at or near expiry.
func (o *OAuthSource) Token(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.cached != nil && !o.cached.expiring(o.skew) {
		return o.cached.AccessToken, nil
	}

	// Cross-process lock. Held across the whole read-refresh-write cycle so a
	// concurrent odin can't redeem the same refresh token.
	unlock, err := lockFile(o.path + ".lock")
	if err != nil {
		return "", fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()

	// Re-read under the lock: another process may have refreshed while we
	// waited, in which case there is nothing to do.
	tok, err := readToken(o.path)
	if err != nil {
		return "", err
	}
	if !tok.expiring(o.skew) {
		o.cached = tok
		return tok.AccessToken, nil
	}

	if tok.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token in %s: re-run `odin auth`", o.path)
	}

	refreshed, err := o.refresh(ctx, tok.RefreshToken)
	if err != nil {
		return "", err
	}
	// Some providers rotate the refresh token, some return only an access
	// token. Carry the old one forward when absent, or the next refresh has
	// nothing to present.
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tok.RefreshToken
	}
	refreshed.LastRefresh = time.Now().UTC()

	if err := writeToken(o.path, refreshed); err != nil {
		return "", fmt.Errorf("persist refreshed token: %w", err)
	}
	o.cached = refreshed
	return refreshed.AccessToken, nil
}

// Recover force-refreshes a token rejected with 401. It re-reads under the
// cross-process lock first so concurrent Odin processes do not both redeem a
// rotating refresh token.
func (o *OAuthSource) Recover(ctx context.Context, status int, rejectedToken string) (string, bool, error) {
	if status != http.StatusUnauthorized {
		return "", false, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	unlock, err := lockFile(o.path + ".lock")
	if err != nil {
		return "", false, fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()

	tok, err := readToken(o.path)
	if err != nil {
		return "", false, err
	}
	if tok.AccessToken != "" && tok.AccessToken != rejectedToken && !tok.expiring(o.skew) {
		o.cached = tok
		return tok.AccessToken, true, nil
	}
	if tok.RefreshToken == "" {
		return "", false, fmt.Errorf("no refresh token in %s: re-run `odin auth`", o.path)
	}

	refreshed, err := o.refresh(ctx, tok.RefreshToken)
	if err != nil {
		return "", false, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tok.RefreshToken
	}
	refreshed.LastRefresh = time.Now().UTC()
	if err := writeToken(o.path, refreshed); err != nil {
		return "", false, fmt.Errorf("persist refreshed token: %w", err)
	}
	o.cached = refreshed
	return refreshed.AccessToken, true, nil
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	ExpiredIn        int64  `json:"expired_in"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (o *OAuthSource) refresh(ctx context.Context, refreshToken string) (*OAuthToken, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {o.clientID},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	o.applyHeaders(req)

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	var parsed tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		// Never include the token in the message — errors get logged.
		detail := parsed.ErrorDescription
		if detail == "" {
			detail = parsed.Error
		}
		if detail == "" {
			detail = "no access_token in response"
		}
		return nil, fmt.Errorf("refresh failed (http %d): %s: re-run `odin auth`", resp.StatusCode, detail)
	}

	tok := &OAuthToken{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		TokenType:    parsed.TokenType,
	}
	tok.ExpiresAt = tokenExpiry(parsed.ExpiresIn, parsed.ExpiredIn)
	return tok, nil
}

// DeviceLogin runs the OAuth 2.0 device authorization grant and stores the
// resulting token.
//
// Device flow rather than a loopback redirect: the server is headless, has no
// browser, and should not open an inbound port. It prints a short code, the
// user approves on a phone, and the server polls until granted. The prompt
// callback receives the user code and verification URL for display.
func (o *OAuthSource) DeviceLogin(ctx context.Context, deviceURL string, prompt func(userCode, verifyURL string)) error {
	if deviceURL == "" {
		return fmt.Errorf("no device_url configured for this provider")
	}

	start, err := o.requestDeviceCode(ctx, deviceURL)
	if err != nil {
		return err
	}
	if prompt != nil {
		url := start.VerificationURIComplete
		if url == "" {
			url = start.VerificationURI
		}
		prompt(start.UserCode, url)
	}

	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiry := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	if start.ExpiresIn <= 0 {
		expiry = time.Now().Add(10 * time.Minute)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(expiry) {
			return fmt.Errorf("device code expired before approval; run `odin auth` again")
		}

		tok, err := o.pollDeviceToken(ctx, start.DeviceCode)
		switch {
		case err == nil:
			// Serialize the write: a concurrent refresh must not interleave.
			unlock, lockErr := lockFile(o.path + ".lock")
			if lockErr != nil {
				return fmt.Errorf("lock credentials: %w", lockErr)
			}
			tok.LastRefresh = time.Now().UTC()
			writeErr := writeToken(o.path, tok)
			unlock()
			if writeErr != nil {
				return fmt.Errorf("persist token: %w", writeErr)
			}
			o.mu.Lock()
			o.cached = tok
			o.mu.Unlock()
			return nil
		case errors.Is(err, errAuthPending):
			continue
		case errors.Is(err, errSlowDown):
			// The server is asking us to back off; honor it or risk a ban.
			interval += 5 * time.Second
			continue
		default:
			return err
		}
	}
}

var (
	errAuthPending = errors.New("authorization_pending")
	errSlowDown    = errors.New("slow_down")
)

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

func (o *OAuthSource) requestDeviceCode(ctx context.Context, deviceURL string) (*deviceCodeResponse, error) {
	form := url.Values{
		"client_id": {o.clientID},
		"scope":     {o.scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	o.applyHeaders(req)

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request: %w", err)
	}
	defer resp.Body.Close()

	var parsed deviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode device code response: %w", err)
	}
	if parsed.Error != "" || parsed.DeviceCode == "" {
		detail := parsed.ErrorDescription
		if detail == "" {
			detail = parsed.Error
		}
		if detail == "" {
			detail = "no device_code in response"
		}
		return nil, fmt.Errorf("device code request failed (http %d): %s", resp.StatusCode, detail)
	}
	return &parsed, nil
}

func (o *OAuthSource) pollDeviceToken(ctx context.Context, deviceCode string) (*OAuthToken, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {o.clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	o.applyHeaders(req)

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token poll: %w", err)
	}
	defer resp.Body.Close()

	var parsed tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	switch parsed.Error {
	case "authorization_pending":
		return nil, errAuthPending
	case "slow_down":
		return nil, errSlowDown
	case "":
		// fall through
	default:
		detail := parsed.ErrorDescription
		if detail == "" {
			detail = parsed.Error
		}
		return nil, fmt.Errorf("device login failed: %s", detail)
	}

	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("token response had no access_token")
	}
	tok := &OAuthToken{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		TokenType:    parsed.TokenType,
	}
	tok.ExpiresAt = tokenExpiry(parsed.ExpiresIn, parsed.ExpiredIn)
	return tok, nil
}

func (o *OAuthSource) applyHeaders(req *http.Request) {
	for key, value := range o.headers {
		req.Header.Set(key, value)
	}
}

func cloneStrings(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func tokenExpiry(expiresIn int, expiredIn int64) time.Time {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	if expiredIn <= 0 {
		return time.Time{}
	}
	// MiniMax has returned both a duration in seconds and an absolute Unix
	// timestamp in milliseconds under the same expired_in field.
	if expiredIn > 10_000_000_000 {
		return time.UnixMilli(expiredIn)
	}
	return time.Now().Add(time.Duration(expiredIn) * time.Second)
}

// Status reports token health without exposing the token itself.
func (o *OAuthSource) Status() (expiresIn time.Duration, lastRefresh time.Time, err error) {
	tok, err := readToken(o.path)
	if err != nil {
		return 0, time.Time{}, err
	}
	if tok.AccessToken == "" {
		return 0, tok.LastRefresh, fmt.Errorf("no access token: run `odin auth`")
	}
	return time.Until(tok.ExpiresAt), tok.LastRefresh, nil
}

func readToken(path string) (*OAuthToken, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no credentials at %s: run `odin auth`", path)
		}
		return nil, err
	}
	var tok OAuthToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &tok, nil
}

// writeToken persists atomically: write a 0600 temp file in the same directory,
// then rename. A crash mid-write leaves the previous credentials intact rather
// than a truncated file that reads as "no token".
func writeToken(path string, tok *OAuthToken) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(tok); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// lockFile takes an exclusive advisory lock, blocking until acquired. The
// returned func releases it.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
