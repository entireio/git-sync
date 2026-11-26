package gitproto

import (
	"os"
	"testing"

	"entire.io/entire/git-sync/internal/syncertest"
)

// TestMain keeps the host's git config, such as commit.gpgsign, out of the
// git commands these tests run.
func TestMain(m *testing.M) {
	syncertest.IsolateGitConfig()
	os.Exit(m.Run())
}
