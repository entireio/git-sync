package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

const entireLocalSmokeEnv = "GITSYNC_E2E_ENTIRE"

type entireHosts map[string]entireHostInfo

type entireHostInfo struct {
	ActiveUser string   `json:"activeUser"`
	Users      []string `json:"users"`
}

type entireHostsFile struct {
	ActiveHost string      `json:"activeHost"`
	Hosts      entireHosts `json:"hosts"`
}

func TestRun_EntireLocalPublicRepoSmoke(t *testing.T) {
	if os.Getenv(entireLocalSmokeEnv) == "" {
		t.Skip("set GITSYNC_E2E_ENTIRE=1 to run the Entire local smoke test")
	}

	baseURL := firstNonEmpty(
		os.Getenv("GITSYNC_E2E_ENTIRE_BASE_URL"),
		os.Getenv("ENTIRE_BASE_URL"),
	)
	if baseURL == "" {
		t.Skip("set GITSYNC_E2E_ENTIRE_BASE_URL or ENTIRE_BASE_URL to your local Entire base URL")
	}

	sourceURL := firstNonEmpty(
		os.Getenv("GITSYNC_E2E_ENTIRE_SOURCE_URL"),
		"https://github.com/entireio/cli.git",
	)
	branch := firstNonEmpty(
		os.Getenv("GITSYNC_E2E_ENTIRE_BRANCH"),
		"main",
	)
	maxPackBytes, err := envInt64Default("GITSYNC_E2E_ENTIRE_MAX_PACK_BYTES", 0)
	if err != nil {
		t.Fatalf("parse GITSYNC_E2E_ENTIRE_MAX_PACK_BYTES: %v", err)
	}
	batchMaxPackBytes, err := envInt64Default("GITSYNC_E2E_ENTIRE_BATCH_MAX_PACK_BYTES", 0)
	if err != nil {
		t.Fatalf("parse GITSYNC_E2E_ENTIRE_BATCH_MAX_PACK_BYTES: %v", err)
	}
	protocolMode := firstNonEmpty(
		os.Getenv("GITSYNC_E2E_ENTIRE_PROTOCOL"),
		protocolModeAuto,
	)
	repoName := firstNonEmpty(
		os.Getenv("GITSYNC_E2E_ENTIRE_REPO"),
		"git-sync-smoke",
	)
	username := os.Getenv("GITSYNC_E2E_ENTIRE_USERNAME")
	token := os.Getenv("GITSYNC_E2E_ENTIRE_TOKEN")
	skipTLSVerify := envBoolDefault("GITSYNC_E2E_ENTIRE_SKIP_TLS_VERIFY", true)

	host, err := credentialHost(baseURL)
	if err != nil {
		t.Fatalf("parse entire base URL: %v", err)
	}
	resolvedHost, err := resolveEntireCredentialHost(host)
	if err != nil {
		t.Fatalf("resolve Entire credential host: %v", err)
	}
	host = resolvedHost
	if username == "" {
		username, err = activeEntireUser(host)
		if err != nil {
			t.Fatalf("determine active Entire user: %v", err)
		}
	}
	if token == "" {
		token, err = lookupEntireToken(host, username)
		if err != nil {
			t.Fatalf("lookup Entire token: %v", err)
		}
	}

	if err := ensureEntireRepo(t, baseURL, repoName, skipTLSVerify); err != nil {
		t.Fatalf("ensure Entire repo exists: %v", err)
	}

	targetURL := strings.TrimRight(baseURL, "/") + "/git/" + username + "/" + repoName
	t.Logf(
		"Entire local smoke config: source=%s branch=%s target=%s repo=%s protocol=%s max_pack_bytes=%d target_max_pack_bytes=%d",
		sourceURL,
		branch,
		targetURL,
		repoName,
		protocolMode,
		maxPackBytes,
		batchMaxPackBytes,
	)
	result, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceURL},
		Target: Endpoint{
			URL:           targetURL,
			Username:      "git",
			Token:         token,
			SkipTLSVerify: skipTLSVerify,
		},
		Branches:           []string{branch},
		Verbose:            true,
		MaxPackBytes:       maxPackBytes,
		TargetMaxPackBytes: batchMaxPackBytes,
		ProtocolMode:       protocolMode,
	})
	if err != nil {
		t.Fatalf("sync public source into Entire failed: %v", err)
	}
	if result.Blocked != 0 {
		t.Fatalf("expected no blocked refs, got %+v", result)
	}

	sourceProbe, err := Probe(context.Background(), Config{
		Source: Endpoint{URL: sourceURL},
	})
	if err != nil {
		t.Fatalf("probe source refs: %v", err)
	}
	targetProbe, err := Probe(context.Background(), Config{
		Source: Endpoint{
			URL:           targetURL,
			Username:      "git",
			Token:         token,
			SkipTLSVerify: skipTLSVerify,
		},
	})
	if err != nil {
		t.Fatalf("probe target refs: %v", err)
	}

	wantRef := "refs/heads/" + branch
	sourceHash, ok := refHashFromInfos(sourceProbe.Refs, wantRef)
	if !ok {
		t.Fatalf("source ref %s not found in probe response", wantRef)
	}
	targetHash, ok := refHashFromInfos(targetProbe.Refs, wantRef)
	if !ok {
		t.Fatalf("target ref %s not found in probe response", wantRef)
	}
	if sourceHash != targetHash {
		t.Fatalf("target ref %s mismatch: source=%s target=%s", wantRef, sourceHash, targetHash)
	}
}

func ensureEntireRepo(t *testing.T, baseURL, repoName string, skipTLSVerify bool) error {
	t.Helper()

	bin, err := entireCLIBinary()
	if err != nil {
		return err
	}

	listCmd := exec.CommandContext(t.Context(), bin, "repo", "list")
	listCmd.Env = append(os.Environ(), entireCLIEnv(baseURL, skipTLSVerify)...)
	output, err := listCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("list repos with entiredb: %w\n%s", err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == repoName {
			return nil
		}
	}

	createCmd := exec.CommandContext(t.Context(), bin, "repo", "create", repoName)
	createCmd.Env = append(os.Environ(), entireCLIEnv(baseURL, skipTLSVerify)...)
	output, err = createCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create repo with entiredb: %w\n%s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func entireCLIBinary() (string, error) {
	if path := os.Getenv("GITSYNC_E2E_ENTIREDB_BIN"); path != "" {
		return path, nil
	}
	if path, err := exec.LookPath("entiredb"); err == nil {
		return path, nil
	}
	return "", errors.New("could not find entiredb; set GITSYNC_E2E_ENTIREDB_BIN or put entiredb on PATH")
}

func entireCLIEnv(baseURL string, skipTLSVerify bool) []string {
	env := []string{"ENTIRE_BASE_URL=" + baseURL}
	if configDir := os.Getenv("ENTIRE_CONFIG_DIR"); configDir != "" {
		env = append(env, "ENTIRE_CONFIG_DIR="+configDir)
	}
	if skipTLSVerify {
		env = append(env, "ENTIRE_TLS_SKIP_VERIFY=true")
	}
	return env
}

func credentialHost(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("missing host in %q", baseURL)
	}
	return parsed.Host, nil
}

func activeEntireUser(host string) (string, error) {
	hosts, _, err := loadEntireHosts()
	if err != nil {
		return "", err
	}
	info, ok := hosts[host]
	if !ok || info.ActiveUser == "" {
		return "", fmt.Errorf("no active Entire user recorded for host %s", host)
	}
	return info.ActiveUser, nil
}

func resolveEntireCredentialHost(requestedHost string) (string, error) {
	hosts, activeHost, err := loadEntireHosts()
	if err != nil {
		return "", err
	}
	if requestedHost != "" {
		if _, ok := hosts[requestedHost]; ok {
			return requestedHost, nil
		}
	}
	if activeHost != "" {
		if _, ok := hosts[activeHost]; ok {
			return activeHost, nil
		}
	}
	return requestedHost, nil
}

func lookupEntireToken(host, username string) (string, error) {
	encoded, err := keyring.Get("entire:"+host, username)
	if err != nil {
		return "", err
	}
	if idx := strings.LastIndex(encoded, "|"); idx != -1 {
		return encoded[:idx], nil
	}
	return encoded, nil
}

func entireConfigDir() string {
	if dir := os.Getenv("ENTIRE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "entire")
}

func loadEntireHosts() (entireHosts, string, error) {
	data, err := os.ReadFile(filepath.Join(entireConfigDir(), "hosts.json"))
	if err != nil {
		return nil, "", fmt.Errorf("read hosts.json: %w", err)
	}

	var wrapped entireHostsFile
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Hosts) > 0 {
		return wrapped.Hosts, wrapped.ActiveHost, nil
	}

	var hosts entireHosts
	if err := json.Unmarshal(data, &hosts); err != nil {
		return nil, "", fmt.Errorf("decode hosts.json: %w", err)
	}
	return hosts, "", nil
}

func envBoolDefault(key string, defaultValue bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return defaultValue
	default:
		return defaultValue
	}
}

func envInt64Default(key string, defaultValue int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func refHashFromInfos(refs []RefInfo, name string) (string, bool) {
	for _, ref := range refs {
		if ref.Name == name {
			return ref.Hash.String(), true
		}
	}
	return "", false
}
