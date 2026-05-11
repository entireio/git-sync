package gitproto

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/stretchr/testify/require"
)

func TestTargetFeaturesFromAdvRefs(t *testing.T) {
	adv := packp.NewAdvRefs()
	require.NoError(t, adv.Capabilities.Set(capability.DeleteRefs))
	require.NoError(t, adv.Capabilities.Set(capability.Capability("no-thin")))
	require.NoError(t, adv.Capabilities.Set(capability.OFSDelta))
	require.NoError(t, adv.Capabilities.Set(capability.ReportStatus))
	require.NoError(t, adv.Capabilities.Set(capability.Sideband64k))

	got := TargetFeaturesFromAdvRefs(adv)
	if !got.Known || !got.DeleteRefs || !got.NoThin || !got.OFSDelta || !got.ReportStatus || !got.Sideband64k {
		t.Fatalf("unexpected target features: %+v", got)
	}
	if got.Sideband {
		t.Fatalf("unexpected sideband feature: %+v", got)
	}
}

func TestTargetFeaturesFromAdvRefsHeadTarget(t *testing.T) {
	adv := packp.NewAdvRefs()
	require.NoError(t, adv.Capabilities.Set(capability.SymRef, "HEAD:refs/heads/main"))

	got := TargetFeaturesFromAdvRefs(adv)
	if got.HeadTarget != plumbing.ReferenceName("refs/heads/main") {
		t.Fatalf("HeadTarget = %q, want refs/heads/main", got.HeadTarget)
	}
}

func TestTargetFeaturesFromAdvRefsNil(t *testing.T) {
	if got := TargetFeaturesFromAdvRefs(nil); got != (TargetFeatures{}) {
		t.Fatalf("expected zero features for nil adv, got %+v", got)
	}
}
