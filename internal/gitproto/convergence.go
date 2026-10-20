package gitproto

import (
	"context"
	"fmt"
	"strings"

	"entire.io/entire/git-sync/internal/sanitize"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// reconcileRejections removes only concurrent-ref refusals whose requested hash
// is now present. It never changes the expected-old snapshot or retries writes.
// Each report is checked separately so split pushes still execute later batches
// and cannot hide an unrelated rejection behind a converged ref. finish, when
// non-nil, completes the push RPC before the verification read opens another.
// A failed finish or read leaves the report unchanged, with a warning.
func (p *Pusher) reconcileRejections(ctx context.Context, req *packp.UpdateRequests, report *packp.ReportStatus, finish func() error) *packp.ReportStatus {
	if p.TargetRefsNow == nil || ctx.Err() != nil || unpackFailed(report) || report.Error() == nil {
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
	candidates := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, status := range report.CommandStatuses {
		if status.Error() == nil {
			continue
		}
		if hash := wanted[status.ReferenceName]; canVerifyRefRejection(status.Status) && !hash.IsZero() {
			candidates[status.ReferenceName] = hash
		} else if p.OnRejection == nil {
			// Strict pushes still fail on this refusal. Avoid a full target read
			// that cannot make the report successful.
			return report
		}
	}
	if len(candidates) == 0 {
		return report
	}
	if finish != nil {
		if err := finish(); err != nil {
			p.warnUnverified(ctx, err)
			return report
		}
	}
	refs, err := p.TargetRefsNow(ctx)
	if err != nil || ctx.Err() != nil {
		p.warnUnverified(ctx, err)
		return report
	}
	filtered := &packp.ReportStatus{UnpackStatus: report.UnpackStatus}
	for _, status := range report.CommandStatuses {
		name := status.ReferenceName
		if hash, ok := candidates[name]; ok && status.Error() != nil && refs[name] == hash {
			p.answer(name, refAnswer{outcome: RefOutcomeAlreadyCurrent})
			if p.OnAlreadyCurrent != nil {
				p.OnAlreadyCurrent(name, hash)
			}
			continue
		}
		filtered.CommandStatuses = append(filtered.CommandStatuses, status)
	}
	return filtered
}

// warnUnverified says a rejection stands only because it could not be checked.
// Cancellation is not worth a warning: the run is already stopping. The error
// can carry server-authored text, so filter it.
func (p *Pusher) warnUnverified(ctx context.Context, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	fmt.Fprintf(progressOut(p.Conn), "target: warning: could not check whether rejected refs are already current: %s\n", sanitize.Text(err.Error()))
}

// Stock receive-pack uses "failed to update ref" for a compare-and-swap miss,
// but also for other write failures. It can trigger a read, not by itself a
// success or an ErrTargetRefMoved classification. Exact desired-hash presence
// must still be established; permission/hook refusals keep their own errors.
func canVerifyRefRejection(status string) bool {
	return isConcurrentMove(status) || strings.EqualFold(strings.TrimSpace(status), "failed to update ref")
}
