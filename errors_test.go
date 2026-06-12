package gitsync

import (
	"errors"
	"fmt"
	"testing"

	"entire.io/entire/git-sync/internal/gitproto"
)

// Compile-time assertion that the exported alias is exactly the internal type,
// so a *RefRejectedError that git-sync constructs internally is reachable via
// errors.As(&gitsync.RefRejectedError{}) by external callers. This does not
// compile if the alias drifts from the internal type.
var _ RefRejectedError = gitproto.RefRejectedError{}

func TestErrTargetRefMovedAliasesInternalSentinel(t *testing.T) {
	// Must be the same error value, or errors.Is across the package boundary
	// (gitsync.ErrTargetRefMoved vs the internally-wrapped sentinel) would fail.
	if !errors.Is(ErrTargetRefMoved, gitproto.ErrTargetRefMoved) {
		t.Fatal("gitsync.ErrTargetRefMoved must alias the internal sentinel")
	}
	wrapped := fmt.Errorf("sync: %w", fmt.Errorf("report-status: %w", ErrTargetRefMoved))
	if !errors.Is(wrapped, ErrTargetRefMoved) {
		t.Fatal("errors.Is must see ErrTargetRefMoved through wrapping")
	}
}
