package gitsync

import (
	"testing"

	"entire.io/entire/git-sync/internal/useragent"

	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
)

func TestSetIdentity(t *testing.T) {
	// Not parallel — mutates process-wide user-agent state.
	orig := useragent.Identity
	t.Cleanup(func() { useragent.Identity = orig })

	tests := []struct {
		name             string
		service, version string
		wantIdentity     string
	}{
		{"service and version", "mirror-worker", "abc1234", "mirror-worker/abc1234"},
		{"service only", "mirror-worker", "", "mirror-worker"},
		{"empty service clears", "", "abc1234", ""},
		{"whitespace collapsed", "mirror worker", "v1 2", "mirror-worker/v1-2"},
		{"blank service clears", "   ", "abc1234", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetIdentity(tt.service, tt.version)
			if useragent.Identity != tt.wantIdentity {
				t.Errorf("SetIdentity(%q, %q) → Identity = %q, want %q",
					tt.service, tt.version, useragent.Identity, tt.wantIdentity)
			}
		})
	}
}

func TestSetIdentityUserAgentShape(t *testing.T) {
	// Not parallel — mutates process-wide user-agent state.
	origIdentity, origVersion := useragent.Identity, useragent.Version
	t.Cleanup(func() { useragent.Identity, useragent.Version = origIdentity, origVersion })

	useragent.Version = "0.7.1"
	SetIdentity("mirror-worker", "abc1234")

	want := "mirror-worker/abc1234 git-sync/0.7.1 " + capability.DefaultAgent()
	if got := useragent.GoGit(); got != want {
		t.Errorf("GoGit() after SetIdentity = %q, want %q", got, want)
	}
	if got, want := useragent.Plain(), "mirror-worker/abc1234 git-sync/0.7.1"; got != want {
		t.Errorf("Plain() after SetIdentity = %q, want %q", got, want)
	}
}
