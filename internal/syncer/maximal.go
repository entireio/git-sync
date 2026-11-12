package syncer

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"github.com/go-git/go-git/v6/plumbing"
)

// fetchHaves returns the target refs a source fetch offers as haves:
// targetRefs without the refs the target proved redundant (see
// maximalTips), then narrowed by planner.FetchHaves.
//
// The reduction comes first because it needs every marked ref in the map. It
// leaves independent-history refs to FetchHaves: an updated one may be
// unmarked behind an unchanged one, and FetchHaves keeps the updated ref's old
// value while dropping the unchanged one, so a reduction there would lose the
// only have that shortens the update. A marked independent ref FetchHaves
// drops reaches only its own namespace's history, so no other ref the
// reduction drops depended on it.
//
// fetches says whether the caller will fetch a pack with these haves. The
// listing costs round trips, so a run that fetches nothing (converged,
// delete-only, or headed for a path that offers no haves) never asks for it.
func (s *syncSession) fetchHaves(
	ctx context.Context,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	desired map[plumbing.ReferenceName]planner.DesiredRef,
	plans []planner.BranchPlan,
	fetches bool,
) map[plumbing.ReferenceName]plumbing.Hash {
	if fetches && fetchesPack(plans) {
		targetRefs = s.maximalTips(ctx).Reduce(targetRefs, s.keepHave(desired))
	}
	return planner.FetchHaves(targetRefs, plans, s.cfg.IndependentHistoryRefPrefixes)
}

// keepHave reports which target refs the maximal reduction must keep as
// haves even when the target left them unmarked: independent-history refs
// (see fetchHaves) and the default branch.
//
// The reduction drops an unmarked ref because a marked tip above it implies
// it. That holds only if the source still holds the marked tip. A marked tip
// the source lacks (the old tip of a branch force-pushed on the source and
// since garbage-collected, or a commit only the target has) is ignored, and
// the history behind it would go out again. A feature branch cut from the
// default branch's tip leaves that tip unmarked, so without this rule the
// whole default branch could ride on one such tip. Keeping it costs one have
// line and bounds the loss to history off the default branch.
//
// The default branch is the source's HEAD branch, under the target name the
// run maps it to, and under its own name in case the run does not map it.
// Without a HEAD symref from the source, only the independent-history rule
// applies.
func (s *syncSession) keepHave(desired map[plumbing.ReferenceName]planner.DesiredRef) func(plumbing.ReferenceName) bool {
	head := plumbing.ReferenceName("")
	if s.sourceService != nil {
		head = s.sourceService.HeadTarget
	}
	return func(name plumbing.ReferenceName) bool {
		if head != "" && (name == head || desired[name].SourceRef == head) {
			return true
		}
		return s.independentHistoryRef(name)
	}
}

func (s *syncSession) independentHistoryRef(name plumbing.ReferenceName) bool {
	for _, prefix := range s.cfg.IndependentHistoryRefPrefixes {
		if prefix = strings.TrimSpace(prefix); prefix != "" && strings.HasPrefix(name.String(), prefix) {
			return true
		}
	}
	return false
}

// fetchesPack reports whether plans need objects from the source: a delete
// moves no objects.
func fetchesPack(plans []planner.BranchPlan) bool {
	for _, plan := range plans {
		if plan.Action == planner.ActionCreate || plan.Action == planner.ActionUpdate {
			return true
		}
	}
	return false
}

// maximalTips asks the target, once per run, which of its refs no other ref
// reaches (gitproto.MaximalFeature). Every target tip is a have by default,
// and most tips are ancestors of others, so a large target sends far more
// have lines than the fetch needs.
//
// It returns nil, and the caller keeps every tip, when the target does not
// advertise the feature (unless Config.AssumeTargetMaximal is set), when
// protocol v1 is pinned, when the target has too few refs to gain anything,
// when the target is reached over SSH, or when the listing fails or breaks the
// server's own rules. An SSH server whose sshd does not accept GIT_PROTOCOL
// answers the v2 probe with its full v0 ref advertisement, which can be
// megabytes for a ref list this run already holds. HTTP and remote helpers
// answer v2 with a short capability list. The listing is an optimization, so a
// failure is logged and not returned.
//
// The listing runs on its own connection, never the push connection. With
// FollowInfoRefsRedirect, upload-pack discovery may redirect to a read host,
// and on a shared HTTPConn that redirect would carry the push and its
// credentials there too.
//
// The listing covers only the namespaces this run mirrors. A marked target ref
// the source lacks would hide the refs behind it from the fetch. Mirrored refs
// came from the source, while a target-only namespace may not have.
func (s *syncSession) maximalTips(ctx context.Context) *gitproto.MaximalTips {
	t := s.target
	if t == nil || t.conn == nil {
		return nil
	}
	if t.maximalListed {
		return t.maximal
	}
	t.maximalListed = true
	if s.cfg.ProtocolMode == protocolModeV1 || len(t.refMap) < 2 || isSSHEndpoint(t.conn) {
		return nil
	}
	probe, err := newConn(s.cfg.Target, "target", s.stats, s.cfg.HTTPClient)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("target maximal tips unavailable; offering every target tip as a have", slog.Any("error", err))
		}
		return nil
	}
	defer probe.Close()
	probe.SetProgressWriter(t.conn.ProgressWriter())
	tips, err := gitproto.ListMaximalTips(ctx, probe, maximalRefPrefixes(planConfig(s.cfg)), s.cfg.AssumeTargetMaximal)
	if err != nil {
		if s.logger != nil && !errors.Is(err, gitproto.ErrMaximalUnsupported) {
			s.logger.Debug("target maximal tips unavailable; offering every target tip as a have", slog.Any("error", err))
		}
		return nil
	}
	if s.logger != nil {
		s.logger.Debug("target maximal tips", slog.Int("marked", tips.Marked()), slog.Int("unmarked", tips.Unmarked()))
	}
	t.maximal = tips
	return tips
}

// maximalRefPrefixes is the target-side scope of the maximal listing: the
// include prefixes when set, otherwise the namespaces the source listing uses.
// A mapping keeps its kind (branch or tag), so the same prefixes cover its
// target name. A ref outside the scope is unlisted, and Reduce keeps it.
func maximalRefPrefixes(cfg planner.PlanConfig) []string {
	var include []string
	for _, p := range cfg.IncludeRefPrefixes {
		if p = strings.TrimSpace(p); p != "" {
			include = append(include, p)
		}
	}
	if len(include) > 0 {
		return include
	}
	return planner.RefPrefixes(cfg)
}

func isSSHEndpoint(conn gitproto.Conn) bool {
	ep := conn.Endpoint()
	return ep != nil && (ep.Scheme == "ssh" || ep.Scheme == "git+ssh")
}
