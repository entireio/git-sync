package syncer

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/gitproto"
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

// withTargetSkipped marks the target as having advertised a ref name that
// validation dropped, which leaves its refMap empty while it plainly holds a
// ref.
func withTargetSkipped(s *syncSession, names ...string) *syncSession {
	s.target.skippedRefNames = names
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
	if !result.SourceEmpty {
		t.Error("SourceEmpty = false; the caller cannot tell this from a no-op sync")
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
	if !result.SourceEmpty {
		t.Error("SourceEmpty = false; a plan should still report what it found")
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
			Protocol: "v2", HeadUnborn: true, SkippedRefNames: []string{"refs/heads/bad name"},
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
		s := withTargetSkipped(emptySourceSession(converged(), nil, nil, unbornSource()), "refs/heads/bad name")
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
	if _, err := s.resolveEmptyDesiredSet(); err.Error() != "no source refs matched" {
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
