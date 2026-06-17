package main

import (
	"strings"
	"testing"

	gitsync "entire.io/entire/git-sync"
	"github.com/spf13/cobra"
)

// Secret-bearing flags must not register their env value as the pflag
// default: pflag prints non-empty defaults in --help and in the usage block
// dumped on a flag error, which would leak the secret (e.g. into CI logs).
// The env value must instead be applied after parsing.
func TestAuthSecretEnvDoesNotLeakIntoUsage(t *testing.T) {
	t.Setenv("GITSYNC_SOURCE_TOKEN", "SUPERSECRET")
	t.Setenv("GITSYNC_TARGET_TOKEN", "TOPSECRET")
	t.Setenv("GITSYNC_SOURCE_BEARER_TOKEN", "BEARERSECRET")
	t.Setenv("GITSYNC_TARGET_BEARER_TOKEN", "BEARERSECRET2")

	var source, target gitsync.EndpointAuth
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	addSourceAuth(cmd, &source)
	addTargetAuth(cmd, &target)

	usage := cmd.UsageString()
	for _, secret := range []string{"SUPERSECRET", "TOPSECRET", "BEARERSECRET", "BEARERSECRET2"} {
		if strings.Contains(usage, secret) {
			t.Fatalf("secret %q leaked into usage output:\n%s", secret, usage)
		}
	}
}

// The env fallback must still populate auth when the flag is not given
// explicitly, and an explicit flag must win over the environment.
func TestAuthSecretEnvFallbackApplies(t *testing.T) {
	t.Setenv("GITSYNC_SOURCE_TOKEN", "from-env")
	t.Setenv("GITSYNC_TARGET_TOKEN", "target-from-env")

	var source, target gitsync.EndpointAuth
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	addSourceAuth(cmd, &source)
	addTargetAuth(cmd, &target)

	// Source token comes from the environment; target token is given
	// explicitly and must override its environment value.
	cmd.SetArgs([]string{"--target-token", "from-flag"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if source.Token != "from-env" {
		t.Errorf("source token = %q, want env fallback %q", source.Token, "from-env")
	}
	if target.Token != "from-flag" {
		t.Errorf("target token = %q, want explicit flag to win over env", target.Token)
	}
}
