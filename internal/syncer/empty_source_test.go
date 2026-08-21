package syncer

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/gitproto"
)

// emptySourceSession builds the session state resolveEmptyDesiredSet reads,
// with the same non-nil stats/measurement fields newSession always installs.
func emptySourceSession(cfg Config, sourceRefs, targetRefs map[plumbing.ReferenceName]plumbing.Hash, unborn bool) *syncSession {
	return &syncSession{
		cfg:             cfg,
		stats:           newStats(false),
		measurementDone: startMeasurement(false),
		sourceService:   &gitproto.RefService{Protocol: "v2", SourceUnborn: unborn},
		sourceRefMap:    sourceRefs,
		target:          &targetSession{refMap: targetRefs},
	}
}

func oneRef() map[plumbing.ReferenceName]plumbing.Hash {
	return map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.ReferenceName("refs/heads/main"): plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}
}

// The converged case: the source confirmed it is empty and the target holds
// nothing either. This is the only input that may succeed, and it must be
// distinguishable from an ordinary no-work sync via SourceEmpty.
func TestResolveEmptyDesiredSetConverged(t *testing.T) {
	s := emptySourceSession(Config{AllowEmptySource: true, AllRefs: true}, nil, nil, true)
	result, err := s.resolveEmptyDesiredSet()
	if err != nil {
		t.Fatalf("expected success for empty source + empty target, got %v", err)
	}
	if !result.SourceEmpty {
		t.Error("SourceEmpty = false; the caller cannot tell this from a no-op sync")
	}
	if len(result.Plans) != 0 {
		t.Errorf("expected 0 plans, got %d", len(result.Plans))
	}
	if result.Pushed != 0 || result.Deleted != 0 {
		t.Errorf("expected nothing applied, got pushed=%d deleted=%d", result.Pushed, result.Deleted)
	}
	if result.OperationMode != modeReplicate {
		t.Errorf("OperationMode = %q, want %q", result.OperationMode, modeReplicate)
	}
}

// Real divergence: refuse rather than converge, because converging here means
// deleting the target's refs.
func TestResolveEmptyDesiredSetTargetPopulated(t *testing.T) {
	s := emptySourceSession(Config{AllowEmptySource: true, AllRefs: true}, nil, oneRef(), true)
	_, err := s.resolveEmptyDesiredSet()
	if !errors.Is(err, ErrSourceEmptyTargetPopulated) {
		t.Fatalf("expected ErrSourceEmptyTargetPopulated, got %v", err)
	}
}

// Silence is not an assertion of emptiness. Without the source's unborn-HEAD
// confirmation the state is unknown, even with an empty target — this is the
// case a blank proxy body or a server-side ref-listing regression lands in,
// and treating it as converged would advance a caller's watermark over a
// source that may well have refs.
func TestResolveEmptyDesiredSetUnverified(t *testing.T) {
	for _, target := range []map[plumbing.ReferenceName]plumbing.Hash{nil, oneRef()} {
		s := emptySourceSession(Config{AllowEmptySource: true, AllRefs: true}, nil, target, false)
		_, err := s.resolveEmptyDesiredSet()
		if !errors.Is(err, ErrSourceEmptyUnverified) {
			t.Fatalf("target=%v: expected ErrSourceEmptyUnverified, got %v", target, err)
		}
	}
}

// A source that HAS refs whose scope selected none of them is a different
// condition entirely, and must not be reported as an empty source however the
// policy is set — a caller acting on emptiness here would be acting on a repo
// full of refs.
func TestResolveEmptyDesiredSetSelectionEmpty(t *testing.T) {
	s := emptySourceSession(Config{AllowEmptySource: true, AllRefs: true}, oneRef(), nil, true)
	_, err := s.resolveEmptyDesiredSet()
	if !errors.Is(err, ErrNoRefsSelected) {
		t.Fatalf("expected ErrNoRefsSelected, got %v", err)
	}
	// An empty target does not soften it: the source has refs, so there is
	// nothing here that could be called converged.
	if errors.Is(err, ErrSourceEmptyUnverified) || errors.Is(err, ErrSourceEmptyTargetPopulated) {
		t.Errorf("selection-empty misreported as an empty-source case: %v", err)
	}

	// Without the opt-in it stays the historical error, like every other case
	// in this family — a caller that never asked for the distinction cannot
	// receive a sentinel it does not know how to classify.
	s = emptySourceSession(Config{}, oneRef(), nil, true)
	if _, err := s.resolveEmptyDesiredSet(); err.Error() != "no source refs matched" {
		t.Errorf("un-opted-in error = %q, want the historical message", err)
	}
}

// Without the opt-in — or under a narrowed scope, where an empty desired set
// says nothing about the repository as a whole — behavior is byte-identical to
// before this logic existed, including the message callers match on.
func TestResolveEmptyDesiredSetFallsBackToHistoricalError(t *testing.T) {
	cases := map[string]Config{
		"no opt-in":      {AllRefs: true},
		"scoped request": {AllowEmptySource: true},
		"neither":        {},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			s := emptySourceSession(cfg, nil, nil, true)
			_, err := s.resolveEmptyDesiredSet()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if err.Error() != "no source refs matched" {
				t.Errorf("error = %q, want the historical %q", err, "no source refs matched")
			}
			// The new sentinels must not leak into the fallback: callers
			// matching the old message must not also match these.
			for _, sentinel := range []error{ErrNoRefsSelected, ErrSourceEmptyUnverified, ErrSourceEmptyTargetPopulated} {
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
	for _, sentinel := range []error{ErrNoRefsSelected, ErrSourceEmptyUnverified, ErrSourceEmptyTargetPopulated} {
		// Substring, not equality: a sentinel that merely CONTAINS the phrase
		// is matched by such a caller just as surely as one that equals it.
		if strings.Contains(sentinel.Error(), "no source refs matched") {
			t.Errorf("%v contains the historical error message %q", sentinel, "no source refs matched")
		}
	}
}
