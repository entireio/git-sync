package internalbridge

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestHashStringZeroHashIsEmpty(t *testing.T) {
	got := HashString(plumbing.ZeroHash)
	if got != "" {
		t.Fatalf("HashString(zero) = %q, want empty string", got)
	}
}
