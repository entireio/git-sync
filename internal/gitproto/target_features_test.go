package gitproto

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
)

func TestTargetFeaturesFromAdvRefs(t *testing.T) {
	adv := packp.NewAdvRefs()
	_ = adv.Capabilities.Set(capability.DeleteRefs)
	_ = adv.Capabilities.Set(capability.Capability("no-thin"))
	_ = adv.Capabilities.Set(capability.OFSDelta)
	_ = adv.Capabilities.Set(capability.ReportStatus)
	_ = adv.Capabilities.Set(capability.Sideband64k)

	got := TargetFeaturesFromAdvRefs(adv)
	if !got.Known || !got.DeleteRefs || !got.NoThin || !got.OFSDelta || !got.ReportStatus || !got.Sideband64k {
		t.Fatalf("unexpected target features: %+v", got)
	}
	if got.Sideband {
		t.Fatalf("unexpected sideband feature: %+v", got)
	}
}

func TestTargetFeaturesFromAdvRefsNil(t *testing.T) {
	if got := TargetFeaturesFromAdvRefs(nil); got != (TargetFeatures{}) {
		t.Fatalf("expected zero features for nil adv, got %+v", got)
	}
}
