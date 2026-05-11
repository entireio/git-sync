package gitproto

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

func TestTargetFeaturesFromAdvRefs(t *testing.T) {
	adv := &packp.AdvRefs{}
	adv.Capabilities.Set(capability.DeleteRefs)
	adv.Capabilities.Set(capability.Capability("no-thin"))
	adv.Capabilities.Set(capability.OFSDelta)
	adv.Capabilities.Set(capability.ReportStatus)
	adv.Capabilities.Set(capability.Sideband64k)

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
