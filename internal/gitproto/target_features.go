package gitproto

import (
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
)

// TargetFeatures summarizes the receive-pack capabilities relevant to strategy
// selection and push behavior.
type TargetFeatures struct {
	Known        bool `json:"known"`
	DeleteRefs   bool `json:"deleteRefs"`
	NoThin       bool `json:"noThin"`
	OFSDelta     bool `json:"ofsDelta"`
	ReportStatus bool `json:"reportStatus"`
	Sideband     bool `json:"sideband"`
	Sideband64k  bool `json:"sideband64k"`
}

// TargetFeaturesFromAdvRefs derives the target-side feature summary from a
// receive-pack advertisement.
func TargetFeaturesFromAdvRefs(adv *packp.AdvRefs) TargetFeatures {
	if adv == nil || adv.Capabilities == nil {
		return TargetFeatures{}
	}
	return TargetFeatures{
		Known:        true,
		DeleteRefs:   adv.Capabilities.Supports(capability.DeleteRefs),
		NoThin:       adv.Capabilities.Supports(capability.Capability("no-thin")),
		OFSDelta:     adv.Capabilities.Supports(capability.OFSDelta),
		ReportStatus: adv.Capabilities.Supports(capability.ReportStatus),
		Sideband:     adv.Capabilities.Supports(capability.Sideband),
		Sideband64k:  adv.Capabilities.Supports(capability.Sideband64k),
	}
}
