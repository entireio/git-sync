package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

func isNotFound(err error) bool {
	return errors.Is(err, keyring.ErrNotFound)
}

const entireCLIClientID = "entire-cli"

type entireAuthHostInfo struct {
	ActiveUser string   `json:"activeUser"`
	Users      []string `json:"users"`
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// LookupEntireDBCredential looks up credentials from the Entire token store.
// Returns (username, password, true, nil) on success, ("", "", false, nil) when
// no credential is configured, or ("", "", false, err) when a credential exists
// but refresh failed (issue #7).
func LookupEntireDBCredential(raw Endpoint, ep *url.URL) (string, string, bool, error) {
	if ep == nil || ep.Host == "" {
		return "", "", false, nil
	}
	credHost := endpointCredentialHost(ep)
	token, err := lookupEntireDBToken(credHost, endpointBaseURL(ep), raw.SkipTLSVerify)
	if err != nil {
		return "", "", false, err
	}
	if token == "" {
		return "", "", false, nil
	}
	username := raw.Username
	if username == "" {
		username = defaultGitUsername
	}
	return username, token, true, nil
}

func endpointBaseURL(ep *url.URL) string {
	if ep == nil || ep.Hostname() == "" {
		return ""
	}
	scheme := ep.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := ep.Host // includes port if present in url.URL
	return scheme + "://" + host
}

func endpointCredentialHost(ep *url.URL) string {
	if ep == nil {
		return ""
	}
	return ep.Host // includes port if present in url.URL
}

func lookupEntireDBToken(host, baseURL string, skipTLS bool) (string, error) {
	configDir := os.Getenv("ENTIRE_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil //nolint:nilerr // missing config dir means no stored credentials, not an error
		}
		configDir = filepath.Join(home, ".config", "entire")
	}

	username, ok := loadEntireDBActiveUser(host, configDir)
	if !ok || username == "" {
		return "", nil
	}
	return getTokenWithRefresh(context.Background(), host, username, baseURL, skipTLS)
}

func loadEntireDBActiveUser(host, configDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(configDir, "hosts.json"))
	if err != nil {
		return "", false
	}
	var hosts map[string]*entireAuthHostInfo
	if err := json.Unmarshal(data, &hosts); err != nil {
		return "", false
	}
	info := hosts[host]
	if info == nil || info.ActiveUser == "" {
		return "", false
	}
	return info.ActiveUser, true
}

// getTokenWithRefresh retrieves a token, refreshing it if expired.
// On refresh failure, returns the stale token with a nil error rather than
// propagating the refresh error silently (issue #7).
func getTokenWithRefresh(ctx context.Context, host, username, baseURL string, skipTLS bool) (string, error) {
	encoded, err := ReadStoredToken(credentialService(host), username)
	if err != nil {
		// "Not found" means no credential is configured — not an error.
		// Only propagate actual storage failures.
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	token, expiresAt := decodeTokenWithExpiration(encoded)
	if token == "" {
		return "", nil
	}
	if !tokenExpiredOrExpiring(expiresAt) {
		return token, nil
	}
	refreshed, err := refreshAccessToken(ctx, host, username, baseURL, skipTLS)
	if err != nil {
		// Issue #7: surface refresh failure explicitly instead of silently reusing stale token.
		return "", fmt.Errorf("token expired and refresh failed for %s@%s: %w", username, host, err)
	}
	return refreshed, nil
}

func decodeTokenWithExpiration(encoded string) (string, time.Time) {
	idx := strings.LastIndex(encoded, "|")
	if idx == -1 {
		return encoded, time.Time{}
	}
	token := encoded[:idx]
	ts, err := strconv.ParseInt(encoded[idx+1:], 10, 64)
	if err != nil {
		return encoded, time.Time{}
	}
	return token, time.Unix(ts, 0)
}

func tokenExpiredOrExpiring(expiresAt time.Time) bool {
	if expiresAt.IsZero() {
		return true
	}
	return time.Now().Add(5 * time.Minute).After(expiresAt)
}

func refreshAccessToken(ctx context.Context, host, username, baseURL string, skipTLS bool) (string, error) {
	refreshToken, err := ReadStoredToken(credentialService(host)+":refresh", username)
	if err != nil {
		return "", err
	}
	if refreshToken == "" || baseURL == "" {
		return "", errors.New("missing refresh token or base url")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", entireCLIClientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLS}, //nolint:gosec // InsecureSkipVerify is controlled by user flag
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute token refresh request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh failed with status %d", resp.StatusCode)
	}

	var tokenResp oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token refresh response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", errors.New("empty access token in refresh response")
	}

	if err := WriteStoredToken(
		credentialService(host),
		username,
		encodeTokenWithExpiration(tokenResp.AccessToken, tokenResp.ExpiresIn),
	); err != nil {
		return "", err
	}
	if tokenResp.RefreshToken != "" {
		//nolint:errcheck // best-effort refresh token storage; access token already saved successfully
		WriteStoredToken(credentialService(host)+":refresh", username, tokenResp.RefreshToken)
	}
	return tokenResp.AccessToken, nil
}

func encodeTokenWithExpiration(token string, expiresIn int64) string {
	return fmt.Sprintf("%s|%d", token, time.Now().Unix()+expiresIn)
}

func credentialService(host string) string {
	return "entire:" + host
}
