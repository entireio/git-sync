package planner

import (
	"github.com/go-git/go-git/v6/plumbing"
)

// RelayTargetPolicy captures only the target-side facts that affect planner
// relay eligibility decisions.
type RelayTargetPolicy struct {
	CapabilitiesKnown bool
	NoThin            bool
}

// SupportsReplicateRelay checks target-side capabilities required by
// replication mode before looking at planned ref actions.
//
// Replicate tolerates targets that advertise "no-thin" because our
// upload-pack client (gitproto.FetchPack) never requests the "thin-pack"
// capability, so the source never emits a thin pack. The relayed pack is
// always self-contained and safe to push to a no-thin receive-pack.
// If gitproto.FetchPack ever begins requesting thin-pack, this check must
// gain a matching fallback (omit the request, or explicitly advertise
// NoThin on the source request) when target.NoThin is set.
func SupportsReplicateRelay(target RelayTargetPolicy) (bool, string) {
	if !target.CapabilitiesKnown {
		return false, "replicate-missing-target-capabilities"
	}
	if target.NoThin {
		return true, "replicate-target-capable-no-thin"
	}
	return true, "replicate-target-capable"
}

// CanBootstrapRelay checks whether all desired target refs are absent on the target,
// making a bootstrap relay possible.
func CanBootstrapRelay(
	force, prune bool,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (bool, string) {
	if force || prune {
		return false, "bootstrap-disabled-by-force-or-prune"
	}
	if len(desired) == 0 {
		return false, "bootstrap-no-managed-refs"
	}
	for targetRef := range desired {
		if !targetRefs[targetRef].IsZero() {
			return false, "bootstrap-target-ref-exists"
		}
	}
	return true, "empty-target-managed-refs"
}

// reasonIncrementalEligible is returned when all plans satisfy the incremental
// relay eligibility rules — branch FF updates, branch creates, or tag creates.
const reasonIncrementalEligible = "fast-forward-branch-or-tag-create"

// CanIncrementalRelay checks whether all plans are eligible for the incremental
// relay fast-path. Eligible plan shapes:
//   - Branch fast-forward update (TargetHash is parent of SourceHash).
//   - Branch create (target has no such ref yet). The relay still passes
//     all target refs as haves, so the source pack covers only objects
//     target doesn't already have via some existing ref. The receive-pack
//     accepts the create command because any pruned objects are reachable
//     from those existing refs.
//   - New tag create.
//
// Incremental tolerates targets that advertise "no-thin" for the same reason
// as replicate (see SupportsReplicateRelay): gitproto.FetchPack never requests
// the "thin-pack" capability, so the relayed pack is always self-contained.
// If gitproto.FetchPack ever begins requesting thin-pack, this function must
// gain a matching fallback when target.NoThin is set.
func CanIncrementalRelay(force, prune, dryRun bool, plans []BranchPlan, target RelayTargetPolicy) (bool, string) {
	if force || prune || dryRun {
		return false, "incremental-disabled-by-force-prune-or-dry-run"
	}
	if len(plans) == 0 {
		return false, "incremental-no-plans"
	}
	if !target.CapabilitiesKnown {
		return false, "incremental-missing-target-capabilities"
	}

	for _, plan := range plans {
		switch plan.Kind {
		case RefKindBranch:
			if !plan.SourceRef.IsBranch() || !plan.TargetRef.IsBranch() {
				return false, "incremental-non-branch-mapping"
			}
			switch plan.Action {
			case ActionUpdate:
				if plan.TargetHash.IsZero() {
					return false, "incremental-branch-target-missing"
				}
			case ActionCreate:
				if !plan.TargetHash.IsZero() {
					return false, "incremental-branch-create-target-not-empty"
				}
			case ActionDelete, ActionSkip, ActionBlock:
				return false, "incremental-branch-action-not-update-or-create"
			}
		case RefKindTag:
			if !plan.SourceRef.IsTag() || !plan.TargetRef.IsTag() {
				return false, "incremental-non-tag-mapping"
			}
			if plan.Action != ActionCreate {
				return false, "incremental-tag-action-not-create"
			}
		default:
			return false, "incremental-unsupported-ref-kind"
		}
	}
	return true, reasonIncrementalEligible
}

// CanFullTagCreateRelay checks whether all plans are tag creates, eligible for
// a full-pack tag relay.
func CanFullTagCreateRelay(plans []BranchPlan) (bool, string) {
	if len(plans) == 0 {
		return false, "incremental-no-plans"
	}
	for _, plan := range plans {
		if plan.Kind != RefKindTag {
			return false, "incremental-tag-relay-non-tag-plan"
		}
		if !plan.SourceRef.IsTag() || !plan.TargetRef.IsTag() {
			return false, "incremental-tag-relay-non-tag-mapping"
		}
		if plan.Action != ActionCreate {
			return false, "incremental-tag-relay-tag-action-not-create"
		}
	}
	return true, "tag-create-full-pack"
}

// RelayFallbackReason returns the reason why relay was not used.
func RelayFallbackReason(force, prune, dryRun bool, plans []BranchPlan, target RelayTargetPolicy) string {
	if ok, reason := CanIncrementalRelay(force, prune, dryRun, plans, target); ok {
		return reason
	}
	_, reason := CanFullTagCreateRelay(plans)
	return reason
}

// CanReplicateRelay checks whether replication mode can execute as a relay-only
// overwrite sync against the target.
func CanReplicateRelay(plans []BranchPlan) (bool, string) {
	for _, plan := range plans {
		switch plan.Kind {
		case RefKindBranch:
			if !plan.SourceRef.IsBranch() || !plan.TargetRef.IsBranch() {
				return false, "replicate-non-branch-mapping"
			}
			if plan.Action != ActionCreate && plan.Action != ActionUpdate {
				return false, "replicate-branch-action-not-create-or-update"
			}
		case RefKindTag:
			if !plan.SourceRef.IsTag() || !plan.TargetRef.IsTag() {
				return false, "replicate-non-tag-mapping"
			}
			if plan.Action != ActionCreate && plan.Action != ActionUpdate {
				return false, "replicate-tag-action-not-create-or-update"
			}
		default:
			return false, "replicate-unsupported-ref-kind"
		}
	}
	return true, "replicate-overwrite-relay"
}
