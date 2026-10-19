package gitproto

import (
	"context"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// reconcileRejections removes only concurrent-ref refusals whose requested hash
// is now present. It never changes the expected-old snapshot or retries writes.
// Each report is checked separately so split pushes still execute later batches
// and cannot hide an unrelated rejection behind a converged ref.
func (p *Pusher) reconcileRejections(ctx context.Context, req *packp.UpdateRequests, report *packp.ReportStatus) *packp.ReportStatus {
	if p.TargetRefsNow == nil || ctx.Err() != nil || (report.UnpackStatus != "" && report.UnpackStatus != "ok") {
		return report
	}
	wanted := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, cmd := range req.Commands {
		// Absence in an advertisement cannot prove deletion: receive.hideRefs
		// can hide a ref that is still present. Only positive matches qualify.
		if !cmd.New.IsZero() {
			wanted[cmd.Name] = cmd.New
		}
	}
	needsRead := false
	for _, status := range report.CommandStatuses {
		if canVerifyRefRejection(status.Status) && !wanted[status.ReferenceName].IsZero() {
			needsRead = true
			break
		}
	}
	if !needsRead {
		return report
	}
	refs, err := p.TargetRefsNow(ctx)
	if err != nil || ctx.Err() != nil {
		return report
	}
	filtered := &packp.ReportStatus{UnpackStatus: report.UnpackStatus}
	for _, status := range report.CommandStatuses {
		name := status.ReferenceName
		hash := wanted[name]
		if canVerifyRefRejection(status.Status) && !hash.IsZero() && refs[name] == hash {
			p.answer(name, refAnswer{current: true})
			if p.OnAlreadyCurrent != nil {
				p.OnAlreadyCurrent(name, hash)
			}
			continue
		}
		filtered.CommandStatuses = append(filtered.CommandStatuses, status)
	}
	return filtered
}

// Stock receive-pack uses "failed to update ref" for a compare-and-swap miss,
// but also for other write failures. It can trigger a read, not by itself a
// success or an ErrTargetRefMoved classification. Exact desired-hash presence
// must still be established; permission/hook refusals keep their own errors.
func canVerifyRefRejection(status string) bool {
	return isConcurrentMove(status) || strings.EqualFold(strings.TrimSpace(status), "failed to update ref")
}
