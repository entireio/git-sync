package syncer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"entire.io/entire/git-sync/internal/validation"
)

// converged is the one input that may succeed: opted in, unscoped, the caller
// asserted emptiness, and every git-side observation agrees.
func converged() Config {
	return Config{AllowEmptySource: true, AllRefs: true, SourceAssertedEmpty: true, TargetAssertedEmpty: true}
}

func emptySourceSession(cfg Config, sourceRefs, targetRefs map[plumbing.ReferenceName]plumbing.Hash, svc *gitproto.RefService) *syncSession {
	return &syncSession{
		cfg:             cfg,
		stats:           newStats(false),
		measurementDone: startMeasurement(false),
		sourceService:   svc,
		sourceRefMap:    sourceRefs,
		target:          &targetSession{refMap: targetRefs},
	}
}

// withTargetSkipped marks the target as having advertised ref names that
// validation dropped, which leaves its refMap empty while it plainly holds
// refs.
func withTargetSkipped(s *syncSession, count int) *syncSession {
	s.target.skippedRefCount = count
	return s
}

func unbornSource() *gitproto.RefService {
	return &gitproto.RefService{Protocol: "v2", HeadUnborn: true}
}

func oneRef() map[plumbing.ReferenceName]plumbing.Hash {
	return map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.ReferenceName("refs/heads/main"): plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}
}

func TestResolveEmptyDesiredSetConverged(t *testing.T) {
	s := emptySourceSession(converged(), nil, nil, unbornSource())
	result, err := s.resolveEmptyDesiredSet()
	if err != nil {
		t.Fatalf("expected success for an asserted-empty source and empty target, got %v", err)
	}
	if !result.Converged {
		t.Error("Converged = false; the caller cannot tell this from a no-op sync")
	}
	if len(result.Plans) != 0 || result.Pushed != 0 || result.Deleted != 0 {
		t.Errorf("expected nothing applied, got plans=%d pushed=%d deleted=%d", len(result.Plans), result.Pushed, result.Deleted)
	}
	if result.OperationMode != modeReplicate {
		t.Errorf("OperationMode = %q, want %q", result.OperationMode, modeReplicate)
	}
}

// A plan must report itself as one. Client.Plan runs replicate with DryRun set,
// and the result is what the caller renders, so dropping the flag makes a plan
// of two empty repos read as a sync that really ran.
func TestResolveEmptyDesiredSetKeepsDryRun(t *testing.T) {
	cfg := converged()
	cfg.DryRun = true
	s := emptySourceSession(cfg, nil, nil, unbornSource())
	result, err := s.resolveEmptyDesiredSet()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !result.DryRun {
		t.Error("DryRun = false on a dry-run session")
	}
	if !result.Converged {
		t.Error("Converged = false; a plan should still report what it found")
	}
}

// Real divergence: refuse rather than converge, because converging here means
// deleting the target's refs.
func TestResolveEmptyDesiredSetTargetPopulated(t *testing.T) {
	s := emptySourceSession(converged(), nil, oneRef(), unbornSource())
	_, err := s.resolveEmptyDesiredSet()
	if !errors.Is(err, ErrSourceEmptyTargetPopulated) {
		t.Fatalf("expected ErrSourceEmptyTargetPopulated, got %v", err)
	}
}

// Every way the evidence can fall short lands on "unknown", never "converged".
// These are the cases where acting on emptiness would advance a watermark over
// a repository that may well hold refs.
func TestResolveEmptyDesiredSetUnverified(t *testing.T) {
	noAssertion := converged()
	noAssertion.SourceAssertedEmpty = false

	cases := map[string]struct {
		cfg Config
		svc *gitproto.RefService
	}{
		// The caller never asserted emptiness, so there is nothing to
		// corroborate — an unborn HEAD on its own proves only that HEAD's
		// target does not exist.
		"no authoritative assertion": {noAssertion, unbornSource()},

		// The assertion disagrees with the wire: HEAD's target exists, so a
		// ref is being withheld. git 2.53 emits no unborn line here.
		"asserted empty but HEAD is born": {converged(), &gitproto.RefService{Protocol: "v2"}},

		// A v1 source, or a v2 source not advertising ls-refs=unborn, cannot
		// report unborn at all — so it can never corroborate, however empty.
		"source cannot report unborn": {converged(), &gitproto.RefService{Protocol: "v1"}},

		// Ref-name validation ate the whole advertisement: the repository
		// plainly has refs, and by ref count alone it looks empty.
		"advertised names dropped as invalid": {converged(), &gitproto.RefService{
			Protocol: "v2", HeadUnborn: true, SkippedRefCount: 1,
		}},

		// No ref service at all (a listing that failed upstream of here).
		"no source service": {converged(), nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Asserted against BOTH target states: an empty target must not
			// rescue missing evidence, and a populated one must not be
			// reported as divergence when emptiness was never established.
			for _, target := range []map[plumbing.ReferenceName]plumbing.Hash{nil, oneRef()} {
				s := emptySourceSession(tc.cfg, nil, target, tc.svc)
				_, err := s.resolveEmptyDesiredSet()
				if !errors.Is(err, ErrSourceEmptyUnverified) {
					t.Fatalf("target=%v: expected ErrSourceEmptyUnverified, got %v", target, err)
				}
			}
		})
	}
}

// The target half needs the same evidence as the source, and for a sharper
// reason: receive.hideRefs omits matching refs from receive-pack's
// advertisement, so a populated target can advertise nothing but the
// capabilities^{} sentinel (verified against git 2.53) — and since that is a
// separate setting from uploadpack.hideRefs, such a ref is still served to the
// mirror's readers. An unverifiable target must therefore never read as
// converged.
func TestResolveEmptyDesiredSetTargetUnverified(t *testing.T) {
	noTargetAssertion := converged()
	noTargetAssertion.TargetAssertedEmpty = false

	t.Run("no authoritative assertion", func(t *testing.T) {
		s := emptySourceSession(noTargetAssertion, nil, nil, unbornSource())
		_, err := s.resolveEmptyDesiredSet()
		if !errors.Is(err, ErrTargetEmptyUnverified) {
			t.Fatalf("expected ErrTargetEmptyUnverified, got %v", err)
		}
		// Must not be mistaken for either neighbouring outcome.
		if errors.Is(err, ErrSourceEmptyUnverified) || errors.Is(err, ErrSourceEmptyTargetPopulated) {
			t.Errorf("target-unverified conflated with another outcome: %v", err)
		}
	})

	t.Run("advertised target names dropped as invalid", func(t *testing.T) {
		s := withTargetSkipped(emptySourceSession(converged(), nil, nil, unbornSource()), 1)
		_, err := s.resolveEmptyDesiredSet()
		if !errors.Is(err, ErrTargetEmptyUnverified) {
			t.Fatalf("expected ErrTargetEmptyUnverified, got %v", err)
		}
	})

	// A VISIBLE target ref is still divergence rather than an unknown: hiding
	// can conceal refs but never invent them, so anything advertised is real.
	t.Run("visible target refs stay divergence", func(t *testing.T) {
		s := emptySourceSession(noTargetAssertion, nil, oneRef(), unbornSource())
		_, err := s.resolveEmptyDesiredSet()
		if !errors.Is(err, ErrSourceEmptyTargetPopulated) {
			t.Fatalf("expected ErrSourceEmptyTargetPopulated, got %v", err)
		}
	})
}

// A source that HAS refs whose scope selected none of them is a different
// condition entirely, and must not be reported as an empty source however the
// policy is set — a caller acting on emptiness here would be acting on a repo
// full of refs.
func TestResolveEmptyDesiredSetSelectionEmpty(t *testing.T) {
	s := emptySourceSession(converged(), oneRef(), nil, unbornSource())
	_, err := s.resolveEmptyDesiredSet()
	if !errors.Is(err, ErrNoRefsSelected) {
		t.Fatalf("expected ErrNoRefsSelected, got %v", err)
	}
	if errors.Is(err, ErrSourceEmptyUnverified) || errors.Is(err, ErrSourceEmptyTargetPopulated) {
		t.Errorf("selection-empty misreported as an empty-source case: %v", err)
	}

	// Without the opt-in it stays the historical error, like every other case
	// in this family — a caller that never asked for the distinction cannot
	// receive a sentinel it does not know how to classify.
	s = emptySourceSession(Config{}, oneRef(), nil, unbornSource())
	// err is checked for nil first: calling Error() straight away panics in
	// exactly the regression this assertion exists to catch (a nil error where
	// the historical one is required), turning a clear failure into a crash.
	_, err = s.resolveEmptyDesiredSet()
	if err == nil {
		t.Fatal("expected the historical error, got nil")
	}
	if err.Error() != "no source refs matched" {
		t.Errorf("un-opted-in error = %q, want the historical message", err)
	}
}

// Without the opt-in — or under a narrowed scope, where an empty desired set
// says nothing about the repository as a whole — behavior is byte-identical to
// before this logic existed, including the message callers match on. Note the
// assertion is set in every case: opting out must win over it.
func TestResolveEmptyDesiredSetFallsBackToHistoricalError(t *testing.T) {
	cases := map[string]Config{
		"no opt-in":      {AllRefs: true, SourceAssertedEmpty: true},
		"scoped request": {AllowEmptySource: true, SourceAssertedEmpty: true},
		"neither":        {SourceAssertedEmpty: true},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			s := emptySourceSession(cfg, nil, nil, unbornSource())
			_, err := s.resolveEmptyDesiredSet()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if err.Error() != "no source refs matched" {
				t.Errorf("error = %q, want the historical %q", err, "no source refs matched")
			}
			for _, sentinel := range []error{ErrNoRefsSelected, ErrSourceEmptyUnverified, ErrTargetEmptyUnverified, ErrSourceEmptyTargetPopulated} {
				if errors.Is(err, sentinel) {
					t.Errorf("fallback error satisfies errors.Is(%v)", sentinel)
				}
			}
		})
	}
}

// The new sentinels must not carry the historical message, or a caller still
// substring-matching it (mirror-pipeline does, across a vendor bump) would
// classify a divergence or an unknown state as the old benign no-op.
func TestEmptySourceSentinelsDoNotCarryHistoricalMessage(t *testing.T) {
	for _, sentinel := range []error{ErrNoRefsSelected, ErrSourceEmptyUnverified, ErrTargetEmptyUnverified, ErrSourceEmptyTargetPopulated} {
		// Substring, not equality: a sentinel that merely CONTAINS the phrase
		// is matched by such a caller just as surely as one that equals it.
		if strings.Contains(sentinel.Error(), "no source refs matched") {
			t.Errorf("%v contains the historical error message %q", sentinel, "no source refs matched")
		}
	}
}

// A target whose only refs are ones this request excludes is converged over
// everything it manages. Counting them made a mirror that trims refs/pull/*
// permanently unconvergeable over refs it would never push or prune — and
// refs/pull/* is the namespace ErrNoRefsSelected's own doc cites as the benign
// case. Every other consumer of the target ref map filters the same way.
func TestResolveEmptySourceIgnoresOutOfScopeTargetRefs(t *testing.T) {
	hash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	cases := map[string]struct {
		cfg    Config
		target map[plumbing.ReferenceName]plumbing.Hash
	}{
		"excluded by prefix": {
			func() Config { c := converged(); c.ExcludeRefPrefixes = []string{"refs/pull/"}; return c }(),
			map[plumbing.ReferenceName]plumbing.Hash{"refs/pull/1/head": hash},
		},
		"excluded by exact name": {
			func() Config { c := converged(); c.ExcludeRefs = []string{"refs/heads/entire"}; return c }(),
			map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/entire": hash},
		},
		// A zero hash is a deletion sentinel, not a ref that exists.
		"zero hash": {
			converged(),
			map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/main": plumbing.ZeroHash},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := emptySourceSession(tc.cfg, nil, tc.target, unbornSource())
			result, err := s.resolveEmptySource()
			if err != nil {
				t.Fatalf("expected convergence over out-of-scope target refs, got %v", err)
			}
			if !result.Converged {
				t.Error("Converged = false")
			}
		})
	}
}

// An in-scope target ref is still divergence, so the scope filter must not have
// turned the check off altogether.
func TestResolveEmptySourceStillSeesInScopeTargetRefs(t *testing.T) {
	cfg := converged()
	cfg.ExcludeRefPrefixes = []string{"refs/pull/"}
	target := oneRef()
	target["refs/pull/1/head"] = plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	s := emptySourceSession(cfg, nil, target, unbornSource())
	_, err := s.resolveEmptySource()
	if !errors.Is(err, ErrSourceEmptyTargetPopulated) {
		t.Fatalf("expected ErrSourceEmptyTargetPopulated, got %v", err)
	}
	// The count must report only what is in scope, or an operator reading it
	// goes looking for refs the run had disclaimed.
	if !strings.Contains(err.Error(), "(1 in scope)") {
		t.Errorf("error = %q, want the in-scope count", err)
	}
}

// This function is billed as the one place that may conclude convergence, and
// Fetch and a target-less Probe both build sessions with no target at all. A
// reuse from either must be refused, not panic on a nil dereference — and only
// on this branch, so no test that syncs a non-empty source would catch it.
func TestResolveEmptySourceWithoutTargetSession(t *testing.T) {
	s := emptySourceSession(converged(), nil, nil, unbornSource())
	s.target = nil
	_, err := s.resolveEmptySource()
	if !errors.Is(err, ErrTargetEmptyUnverified) {
		t.Fatalf("expected ErrTargetEmptyUnverified, got %v", err)
	}
}

// Replicate refuses a non-relay target outright, so every successful replicate
// reports a relay. A converged run that left these zero would be read as a
// materialized fallback or as a malformed result.
func TestResolveEmptySourceReportsRelayFields(t *testing.T) {
	s := emptySourceSession(converged(), nil, nil, unbornSource())
	result, err := s.resolveEmptySource()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !result.Relay || result.RelayMode != modeReplicate || result.RelayReason == "" {
		t.Errorf("relay fields unset on a converged replicate: relay=%t mode=%q reason=%q",
			result.Relay, result.RelayMode, result.RelayReason)
	}
	// SourceHEAD stays empty on purpose: an unborn HEAD has no target branch
	// that exists, and consumers read a non-empty SourceHEAD as one that does.
	if result.SourceHEAD != "" {
		t.Errorf("SourceHEAD = %q, want empty for an unborn HEAD", result.SourceHEAD)
	}
}

// The text output is what cmd/git-sync and every non-JSON consumer render, so a
// converged run that prints the same summary as a no-op sync drops the one
// distinction the field exists to draw.
func TestConvergedResultIsVisibleInTextOutput(t *testing.T) {
	s := emptySourceSession(converged(), nil, nil, unbornSource())
	result, err := s.resolveEmptySource()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	converged := strings.Join(result.Lines(), "\n")
	if !strings.Contains(converged, "converged:") {
		t.Errorf("converged replicate renders no converged line:\n%s", converged)
	}
	if plain := strings.Join(Result{OperationMode: modeReplicate}.Lines(), "\n"); strings.Contains(plain, "converged:") {
		t.Errorf("an ordinary no-op run claims convergence:\n%s", plain)
	}
}

// The wiring the whole policy rests on, exercised end to end: the ls-refs
// "unborn" argument goes on the wire, the response's unborn line becomes
// RefService.HeadUnborn, and Run turns that into a converged zero-plan result.
//
// Every other test in this file hand-builds a syncSession, so none of that
// chain was covered: deleting the "unborn" argument from listSourceRefsV2 left
// the entire suite green while every real empty-source run degraded to
// ErrSourceEmptyUnverified forever.
func TestRun_EmptySourceConvergesEndToEnd(t *testing.T) {
	sourceRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init source repo: %v", err)
	}
	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	cfg := Config{
		Source:              Endpoint{URL: sourceServer.RepoURL()},
		Target:              Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode:        protocolModeV2,
		Mode:                modeReplicate,
		AllRefs:             true,
		AllowEmptySource:    true,
		SourceAssertedEmpty: true,
		TargetAssertedEmpty: true,
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected two empty repos to converge, got %v", err)
	}
	if !result.Converged {
		t.Error("Converged = false on two verified-empty repositories")
	}
	if len(result.Plans) != 0 || result.Pushed != 0 || result.Deleted != 0 {
		t.Errorf("expected nothing applied, got plans=%d pushed=%d deleted=%d",
			len(result.Plans), result.Pushed, result.Deleted)
	}

	// Without the opt-in the same pair of repositories must still fail, and
	// with the historical message: the policy is what changes the outcome, not
	// the wire.
	optedOut := cfg
	optedOut.AllowEmptySource = false
	optedOut.SourceAssertedEmpty = false
	optedOut.TargetAssertedEmpty = false
	if _, err := Run(context.Background(), optedOut); err == nil {
		t.Error("an empty source without the opt-in must still be an error")
	} else if !strings.Contains(err.Error(), "no source refs matched") {
		t.Errorf("un-opted-in error = %q, want the historical message", err)
	}
}

// A caller that pins refs by mapping is the headline use case for a mirror, and
// the policy was entirely inert for it: BuildDesiredRefs errors on the absent
// mapped source ref before the empty-set branch can run, so the caller got a
// hard failure matching none of the sentinels on exactly the state the policy
// exists to make succeed.
func TestRun_EmptySourceConvergesWithMappings(t *testing.T) {
	sourceRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init source repo: %v", err)
	}
	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Run(context.Background(), Config{
		Source:              Endpoint{URL: sourceServer.RepoURL()},
		Target:              Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode:        protocolModeV2,
		Mode:                modeReplicate,
		AllRefs:             true,
		Mappings:            []validation.RefMapping{{Source: "refs/heads/main", Target: "refs/heads/main"}},
		AllowEmptySource:    true,
		SourceAssertedEmpty: true,
		TargetAssertedEmpty: true,
	})
	if err != nil {
		t.Fatalf("expected a mapping-pinned mirror of two empty repos to converge, got %v", err)
	}
	if !result.Converged {
		t.Error("Converged = false on two verified-empty repositories with a mapping")
	}
}

// A policy no path would consult must fail at the edge rather than be threaded
// in and discarded. Both conditions previously produced the historical
// "no source refs matched", which tells the caller nothing about their policy
// having been ignored.
func TestValidateEmptySourcePolicy(t *testing.T) {
	cases := map[string]struct {
		cfg     Config
		wantErr bool
	}{
		"replicate, unscoped": {Config{AllowEmptySource: true, AllRefs: true, Mode: modeReplicate}, false},
		"sync mode":           {Config{AllowEmptySource: true, AllRefs: true, Mode: modeSync}, true},
		"scoped replicate":    {Config{AllowEmptySource: true, Mode: modeReplicate}, true},
		// The assertions are inputs to the policy, inert without it by design.
		"assertions without the opt-in": {Config{SourceAssertedEmpty: true, TargetAssertedEmpty: true, Mode: modeSync}, false},
		"policy unset":                  {Config{Mode: modeSync}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateEmptySourcePolicy(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateEmptySourcePolicy() err = %v, wantErr = %t", err, tc.wantErr)
			}
		})
	}
}

// Reached through a real entry point, not just the predicate: a scoped request
// must be refused before any I/O rather than reaching the planner.
func TestRun_EmptySourcePolicyRejectedAtTheEdge(t *testing.T) {
	cfg := Config{
		Source:              Endpoint{URL: "https://source.invalid/repo.git"},
		Target:              Endpoint{URL: "https://target.invalid/repo.git"},
		Mode:                modeReplicate,
		AllowEmptySource:    true,
		SourceAssertedEmpty: true,
		TargetAssertedEmpty: true,
	}
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected a scoped AllowEmptySource request to be rejected")
	} else if !strings.Contains(err.Error(), "AllRefs") {
		t.Errorf("error = %q, want it to name the AllRefs requirement", err)
	}
}

// Convergence needs an unborn HEAD, which only v2's ls-refs can carry. Pinning
// v1 therefore fails every run — and with a message that reads as the source
// withholding refs, when the real cause is the caller's protocol selection. It
// is refused before any I/O instead.
func TestValidateEmptySourceRejectsProtocolV1(t *testing.T) {
	base := Config{AllowEmptySource: true, AllRefs: true, Mode: modeReplicate}

	v1 := base
	v1.ProtocolMode = protocolModeV1
	if err := validateEmptySourcePolicy(v1); err == nil {
		t.Error("expected AllowEmptySource + protocol v1 to be rejected")
	} else if !strings.Contains(err.Error(), "v2") {
		t.Errorf("error = %q, want it to name the v2 requirement", err)
	}

	// auto is fine: it negotiates v2 wherever the server supports it, and the
	// SSH fallback to v1 is only observable mid-run.
	for _, mode := range []string{protocolModeAuto, protocolModeV2, ""} {
		cfg := base
		cfg.ProtocolMode = mode
		if err := validateEmptySourcePolicy(cfg); err != nil {
			t.Errorf("protocol %q rejected: %v", mode, err)
		}
	}
}

// What validation cannot catch: an "auto" source whose v2 probe failed and fell
// back to v1 mid-run. The error must name the protocol rather than implying the
// server hid refs, because the two call for opposite responses — one is a
// client configuration note, the other a reason to suspect the source.
func TestResolveEmptySourceNamesTheProtocolNotTheServer(t *testing.T) {
	cases := map[string]struct {
		svc  *gitproto.RefService
		want string
	}{
		"fell back to v1": {
			&gitproto.RefService{Protocol: protocolModeV1},
			"protocol v1",
		},
		"v2 without the capability": {
			&gitproto.RefService{Protocol: protocolModeV2, V2Caps: &gitproto.V2Capabilities{}},
			"ls-refs=unborn",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := emptySourceSession(converged(), nil, nil, tc.svc)
			_, err := s.resolveEmptySource()
			if !errors.Is(err, ErrSourceEmptyUnverified) {
				t.Fatalf("expected ErrSourceEmptyUnverified, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// The message that blames the server must not be the one shown.
			if strings.Contains(err.Error(), "did not report an unborn HEAD") {
				t.Errorf("a protocol limitation was reported as a withheld ref: %q", err)
			}
		})
	}
}

// A source that CAN report unborn and does not is the case where false really is
// evidence against the caller's assertion, so that message must survive.
func TestResolveEmptySourceStillBlamesAWithheldRef(t *testing.T) {
	// No V2Caps: nothing establishes that the server lacks the feature, so
	// false is read as evidence rather than as a protocol limitation.
	s := emptySourceSession(converged(), nil, nil, &gitproto.RefService{Protocol: protocolModeV2})
	_, err := s.resolveEmptySource()
	if !errors.Is(err, ErrSourceEmptyUnverified) {
		t.Fatalf("expected ErrSourceEmptyUnverified, got %v", err)
	}
	if !strings.Contains(err.Error(), "did not report an unborn HEAD") {
		t.Errorf("error = %q, want the withheld-ref message", err)
	}
}

// The "unborn" ls-refs argument is deliberately NOT gated on
// SyncPolicy.AllowEmptySource: it is appended to every v2 listing whose server
// advertised support for it, opt-in or not.
//
// That is a decision, not an oversight, so it is pinned here. Gating it would
// mean threading the policy through gitproto.ListSourceRefs for no benefit —
// the argument adds no round trip, a source with commits answers exactly as it
// did before, and the value is that Probe and Plan can report an unborn HEAD
// without the caller having opted into a convergence policy first. What IS
// gated is the reader: only resolveEmptySource acts on HeadUnborn.
//
// The gate that does matter is the advertisement: protocol v2 forbids sending
// an argument the server did not advertise, and a strict server may fail the
// command.
func TestLSRefsUnbornIsSentRegardlessOfPolicy(t *testing.T) {
	repo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	server := newSmartHTTPRepoServerV2(t, repo)
	defer server.Close()

	// A probe: no policy at all, not even a target.
	if _, err := Probe(context.Background(), Config{
		Source:       Endpoint{URL: server.RepoURL()},
		ProtocolMode: protocolModeV2,
	}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	args := server.LastLSRefsArgs()
	if !slices.Contains(args, "unborn") {
		t.Errorf("ls-refs args = %v, want the unborn argument on an un-opted-in probe", args)
	}
}

// The one gate that is load-bearing: protocol v2 forbids sending an argument
// the server did not advertise, and a strict server may fail the command
// outright. A source advertising a bare "ls-refs" must therefore see no unborn
// argument — and must still list refs normally.
func TestLSRefsUnbornWithheldWhenUnadvertised(t *testing.T) {
	repo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	server := newSmartHTTPRepoServerV2(t, repo)
	server.lsRefsNoUnborn = true
	defer server.Close()

	if _, err := Probe(context.Background(), Config{
		Source:       Endpoint{URL: server.RepoURL()},
		ProtocolMode: protocolModeV2,
	}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if args := server.LastLSRefsArgs(); slices.Contains(args, "unborn") {
		t.Errorf("ls-refs args = %v; sent an argument the server did not advertise", args)
	}
}

// Scope is not only exclusions. A mapping-scoped request manages the refs it
// mapped and, for prune, tags — but not unmapped branches and not other
// namespaces. Counting those left a mapping-pinned mirror permanently diverged
// over a ref it would neither push nor prune, which is the same false
// divergence exclusions produced and defeats the very case the pre-planning
// path was added to serve.
func TestResolveEmptySourceRespectsMappingScope(t *testing.T) {
	hash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	mapped := func() Config {
		c := converged()
		c.Mappings = []validation.RefMapping{{Source: "refs/heads/main", Target: "refs/heads/main"}}
		return c
	}

	t.Run("unmapped branch is not this request's business", func(t *testing.T) {
		target := map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/other": hash}
		s := emptySourceSession(mapped(), nil, target, unbornSource())
		result, err := s.resolveEmptySource()
		if err != nil {
			t.Fatalf("expected convergence over an unmapped target branch, got %v", err)
		}
		if !result.Converged {
			t.Error("Converged = false")
		}
	})

	// Other namespaces are NOT like unmapped branches, which is what an
	// earlier revision of this test got wrong. BuildDesiredRefs' tag and
	// other-kind pass sits outside the mapping branch, so an AllRefs request
	// mirrors refs/notes/* even with Mappings set — prune is what skips them,
	// and push scope is the wider of the two.
	t.Run("other namespaces are mirrored under AllRefs, so they diverge", func(t *testing.T) {
		target := map[plumbing.ReferenceName]plumbing.Hash{"refs/notes/commits": hash}
		s := emptySourceSession(mapped(), nil, target, unbornSource())
		if _, err := s.resolveEmptySource(); !errors.Is(err, ErrSourceEmptyTargetPopulated) {
			t.Fatalf("expected ErrSourceEmptyTargetPopulated for a mirrored namespace, got %v", err)
		}
	})

	// Same reasoning, and it is what keeps the exclusion behaviour honest: an
	// EXCLUDED other-kind ref is genuinely out of scope, since auto-discovery
	// applies exclusions.
	t.Run("an excluded namespace stays out of scope", func(t *testing.T) {
		cfg := mapped()
		cfg.ExcludeRefPrefixes = []string{"refs/notes/"}
		target := map[plumbing.ReferenceName]plumbing.Hash{"refs/notes/commits": hash}
		s := emptySourceSession(cfg, nil, target, unbornSource())
		if _, err := s.resolveEmptySource(); err != nil {
			t.Fatalf("expected convergence over an excluded namespace, got %v", err)
		}
	})

	// The case that matters most, and the one an "unmapped refs are out of
	// scope" reading silently drops: the target still holds the very ref the
	// request maps. Converging here would mean deleting it.
	t.Run("the mapped ref itself is divergence", func(t *testing.T) {
		target := map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/main": hash}
		s := emptySourceSession(mapped(), nil, target, unbornSource())
		_, err := s.resolveEmptySource()
		if !errors.Is(err, ErrSourceEmptyTargetPopulated) {
			t.Fatalf("expected ErrSourceEmptyTargetPopulated for a populated mapping target, got %v", err)
		}
	})

	// A mapping target is managed regardless of exclusions, matching the
	// mapping pass in BuildDesiredRefs, which applies exclusions only to
	// auto-discovery.
	t.Run("a mapped ref stays in scope even when excluded", func(t *testing.T) {
		cfg := mapped()
		cfg.ExcludeRefPrefixes = []string{"refs/heads/"}
		target := map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/main": hash}
		s := emptySourceSession(cfg, nil, target, unbornSource())
		if _, err := s.resolveEmptySource(); !errors.Is(err, ErrSourceEmptyTargetPopulated) {
			t.Fatalf("expected an excluded-but-mapped target ref to still be divergence, got %v", err)
		}
	})

	// Mapping targets are compared by resolved name, so a short-form mapping
	// must match the full ref the target advertises.
	t.Run("short-form mapping names resolve", func(t *testing.T) {
		cfg := converged()
		cfg.Mappings = []validation.RefMapping{{Source: "main", Target: "trunk"}}
		target := map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/trunk": hash}
		s := emptySourceSession(cfg, nil, target, unbornSource())
		if _, err := s.resolveEmptySource(); !errors.Is(err, ErrSourceEmptyTargetPopulated) {
			t.Fatalf("expected a short-form mapping target to resolve to refs/heads/trunk, got %v", err)
		}
	})

	// Tags stay in scope under a mapping, because prune still selects them —
	// the check must track what the planner does, not a simpler story.
	t.Run("tags remain divergence", func(t *testing.T) {
		target := map[plumbing.ReferenceName]plumbing.Hash{"refs/tags/v1": hash}
		s := emptySourceSession(mapped(), nil, target, unbornSource())
		if _, err := s.resolveEmptySource(); !errors.Is(err, ErrSourceEmptyTargetPopulated) {
			t.Fatalf("expected ErrSourceEmptyTargetPopulated for a target tag, got %v", err)
		}
	})
}

// planConfig does not normalize, and un-normalized AllRefs configs still carry
// the caller's Branches filter. Reading that raw would report a branch as
// unmanaged when the request would in fact prune it — an undercount, so it
// converges over a populated target rather than refusing.
func TestResolveEmptySourceNormalizesScopeBeforeJudging(t *testing.T) {
	cfg := converged()
	cfg.Branches = []string{"main"} // AllRefs is set, so this is cleared by normalization
	target := map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/other": plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}
	s := emptySourceSession(cfg, nil, target, unbornSource())
	if _, err := s.resolveEmptySource(); !errors.Is(err, ErrSourceEmptyTargetPopulated) {
		t.Fatalf("a populated target must refuse regardless of a stale Branches filter, got %v", err)
	}
}

// The converged result claims Relay: true on the strength of replicate
// refusing non-relay targets outright. That justification only holds if the
// relay check actually runs on this path — and it did not: the empty-source
// intercept returned first, so a target whose receive-pack advertisement
// carries no capabilities got a success claiming a relay that the ordinary
// path would have refused. Nothing moves either way, but the field was
// fabricated, in a change whose whole subject is honest reporting.
func TestRunReplicateChecksRelayBeforeResolvingEmptySource(t *testing.T) {
	s := emptySourceSession(converged(), nil, nil, unbornSource())
	// CapabilitiesKnown false is what an unreadable target advertisement
	// produces; SupportsReplicateRelay rejects it.
	s.target.policy = planner.RelayTargetPolicy{CapabilitiesKnown: false}

	_, err := s.runReplicate(context.Background())
	if err == nil {
		t.Fatal("expected a non-relay-capable target to be refused, got convergence")
	}
	if !strings.Contains(err.Error(), "replicate requires relay-capable target") {
		t.Errorf("error = %q, want the relay-capability refusal", err)
	}
	// It must not be reported as an empty-source outcome: the run never got
	// far enough to judge emptiness.
	for _, sentinel := range []error{ErrNoRefsSelected, ErrSourceEmptyUnverified, ErrTargetEmptyUnverified, ErrSourceEmptyTargetPopulated} {
		if errors.Is(err, sentinel) {
			t.Errorf("relay refusal misreported as %v", sentinel)
		}
	}

	// With a capable target the same session converges, so the new check is
	// a gate on capability rather than a blanket refusal.
	s = emptySourceSession(converged(), nil, nil, unbornSource())
	s.target.policy = planner.RelayTargetPolicy{CapabilitiesKnown: true}
	result, err := s.runReplicate(context.Background())
	if err != nil {
		t.Fatalf("expected convergence against a relay-capable target, got %v", err)
	}
	if !result.Converged || !result.Relay {
		t.Errorf("converged=%t relay=%t; want both true", result.Converged, result.Relay)
	}
}
