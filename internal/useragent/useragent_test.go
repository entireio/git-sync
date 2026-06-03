package useragent

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
)

func TestGoGit(t *testing.T) {
	t.Parallel()

	got := GoGit()
	wantPrefix := "git-sync/" + Version + " "
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("GoGit() = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, capability.DefaultAgent()) {
		t.Errorf("GoGit() = %q, want it to contain %q", got, capability.DefaultAgent())
	}
}

func TestPlain(t *testing.T) {
	t.Parallel()

	got := Plain()
	want := "git-sync/" + Version
	if got != want {
		t.Errorf("Plain() = %q, want %q", got, want)
	}
}

func TestVersionOverride(t *testing.T) {
	// Not parallel — mutates package-level Version.
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "1.2.3"
	if got, want := Plain(), "git-sync/1.2.3"; got != want {
		t.Errorf("Plain() with overridden Version = %q, want %q", got, want)
	}
	if got, want := GoGit(), "git-sync/1.2.3 "+capability.DefaultAgent(); got != want {
		t.Errorf("GoGit() with overridden Version = %q, want %q", got, want)
	}
}
