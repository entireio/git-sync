package gitproto

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
)

var (
	maxA = plumbing.NewHash(strings.Repeat("a", 40))
	maxB = plumbing.NewHash(strings.Repeat("b", 40))
	maxC = plumbing.NewHash(strings.Repeat("c", 40))
	maxD = plumbing.NewHash(strings.Repeat("d", 40))
)

func pktBody(t *testing.T, lines ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, l := range lines {
		if _, err := pktline.WriteString(&buf, l+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := pktline.WriteFlush(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// maximalConn serves a v2 advertisement and one ls-refs response, and records
// the ls-refs request.
type maximalConn struct {
	stubConn

	adv     []byte
	lsRefs  []byte
	request []byte
}

func (c *maximalConn) RequestInfoRefs(context.Context, string, string) ([]byte, error) {
	return c.adv, nil
}

func (c *maximalConn) PostRPCStreamBody(_ context.Context, _ string, body io.Reader, _ bool, _ string) (io.ReadCloser, error) {
	var err error
	if c.request, err = io.ReadAll(body); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(c.lsRefs)), nil
}

func TestListMaximalTips(t *testing.T) {
	conn := &maximalConn{
		adv: pktBody(t, "version 2", "ls-refs=unborn "+MaximalFeature, "fetch"),
		lsRefs: pktBody(t,
			maxA.String()+" HEAD symref-target:refs/heads/main "+MaximalFeature,
			maxA.String()+" refs/heads/main "+MaximalFeature,
			maxB.String()+" refs/heads/old",
			maxC.String()+" refs/tags/v1 peeled:"+maxB.String(),
		),
	}
	tips, err := ListMaximalTips(t.Context(), conn, []string{"refs/heads/", "refs/tags/"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if tips == nil {
		t.Fatal("tips = nil for a server advertising the feature")
	}
	req := string(conn.request)
	for _, want := range []string{"command=ls-refs", MaximalFeature, "ref-prefix refs/heads/", "ref-prefix refs/tags/"} {
		if !strings.Contains(req, want) {
			t.Errorf("request %q lacks %q", req, want)
		}
	}
	if tips.Marked() != 1 || tips.Unmarked() != 2 {
		t.Fatalf("marked=%d unmarked=%d, want 1 and 2 (HEAD skipped)", tips.Marked(), tips.Unmarked())
	}
}

func TestListMaximalTipsUnadvertised(t *testing.T) {
	for name, adv := range map[string][]string{
		"no feature":  {"version 2", "ls-refs=unborn", "fetch"},
		"no ls-refs":  {"version 2", "fetch"},
		"v1 response": {maxA.String() + " refs/heads/main\x00report-status"},
	} {
		t.Run(name, func(t *testing.T) {
			conn := &maximalConn{adv: pktBody(t, adv...)}
			tips, err := ListMaximalTips(t.Context(), conn, nil, false)
			if !errors.Is(err, ErrMaximalUnsupported) {
				t.Fatalf("err = %v, want ErrMaximalUnsupported", err)
			}
			if tips != nil {
				t.Fatal("tips != nil without the advertisement")
			}
			if conn.request != nil {
				t.Fatalf("sent ls-refs %q to a server that did not advertise %s", conn.request, MaximalFeature)
			}
		})
	}
}

// AssumeTargetMaximal's path: the argument goes to a v2 server that does not
// advertise it, but never to one without v2 ls-refs.
func TestListMaximalTipsAssumed(t *testing.T) {
	conn := &maximalConn{
		adv:    pktBody(t, "version 2", "ls-refs=unborn", "fetch"),
		lsRefs: pktBody(t, maxA.String()+" refs/heads/main "+MaximalFeature),
	}
	tips, err := ListMaximalTips(t.Context(), conn, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if tips == nil || tips.Marked() != 1 {
		t.Fatalf("tips = %+v, want one marked ref", tips)
	}
	if !strings.Contains(string(conn.request), MaximalFeature) {
		t.Fatalf("request %q lacks %s", conn.request, MaximalFeature)
	}

	v1 := &maximalConn{adv: pktBody(t, maxA.String()+" refs/heads/main\x00report-status")}
	if _, err := ListMaximalTips(t.Context(), v1, nil, true); !errors.Is(err, ErrMaximalUnsupported) {
		t.Fatalf("err = %v, want ErrMaximalUnsupported for a v1 server", err)
	}
	if v1.request != nil {
		t.Fatalf("sent ls-refs %q to a v1 server", v1.request)
	}
}

func TestMaximalTipsReduce(t *testing.T) {
	tips := &MaximalTips{
		marked:   map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/main": maxA},
		unmarked: map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/old": maxB, "refs/tags/v1": maxC},
	}
	full := map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/main":  maxA,
		"refs/heads/old":   maxB,
		"refs/tags/v1":     maxC,
		"refs/pull/1/head": maxD, // unlisted: kept
	}
	cases := map[string]struct {
		tips  *MaximalTips
		keep  func(plumbing.ReferenceName) bool
		haves map[plumbing.ReferenceName]plumbing.Hash
		want  map[plumbing.ReferenceName]plumbing.Hash
	}{
		"drops unmarked": {
			tips: tips, haves: full,
			want: map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/main": maxA, "refs/pull/1/head": maxD},
		},
		"nil keeps all": {tips: nil, haves: full, want: full},
		"marked ref moved keeps all": {
			tips: tips,
			haves: map[plumbing.ReferenceName]plumbing.Hash{
				"refs/heads/main": maxD, "refs/heads/old": maxB, "refs/tags/v1": maxC,
			},
			want: map[plumbing.ReferenceName]plumbing.Hash{
				"refs/heads/main": maxD, "refs/heads/old": maxB, "refs/tags/v1": maxC,
			},
		},
		"marked ref missing keeps all": {
			tips:  tips,
			haves: map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/old": maxB},
			want:  map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/old": maxB},
		},
		"kept ref stays": {
			tips: tips, haves: full,
			keep: func(n plumbing.ReferenceName) bool { return n == "refs/tags/v1" },
			want: map[plumbing.ReferenceName]plumbing.Hash{
				"refs/heads/main": maxA, "refs/tags/v1": maxC, "refs/pull/1/head": maxD,
			},
		},
		"no marks keeps all": {
			tips: &MaximalTips{
				marked:   map[plumbing.ReferenceName]plumbing.Hash{},
				unmarked: tips.unmarked,
			},
			haves: full, want: full,
		},
		"unmarked ref moved stays": {
			tips: tips,
			haves: map[plumbing.ReferenceName]plumbing.Hash{
				"refs/heads/main": maxA, "refs/heads/old": maxD, "refs/tags/v1": maxC,
			},
			want: map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/main": maxA, "refs/heads/old": maxD},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.tips.Reduce(tc.haves, tc.keep); !maps.Equal(got, tc.want) {
				t.Fatalf("Reduce = %v, want %v", got, tc.want)
			}
		})
	}
}

// A listing that breaks the server's rules is refused whole, so the caller
// keeps every have instead of dropping ones nothing proved redundant.
func TestListMaximalTipsInconsistent(t *testing.T) {
	adv := []string{"version 2", "ls-refs=unborn " + MaximalFeature, "fetch"}
	for name, lines := range map[string][]string{
		"no marks": {
			maxA.String() + " refs/heads/main",
			maxB.String() + " refs/heads/old",
		},
		"duplicate name": {
			maxA.String() + " refs/heads/main " + MaximalFeature,
			maxB.String() + " refs/heads/main",
		},
		"same commit marked differently": {
			maxA.String() + " refs/heads/main " + MaximalFeature,
			maxA.String() + " refs/heads/copy",
		},
		"tag peeled to a marked commit, unmarked": {
			maxA.String() + " refs/heads/main " + MaximalFeature,
			maxC.String() + " refs/tags/v1 peeled:" + maxA.String(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			conn := &maximalConn{adv: pktBody(t, adv...), lsRefs: pktBody(t, lines...)}
			tips, err := ListMaximalTips(t.Context(), conn, nil, false)
			if !errors.Is(err, ErrMaximalInconsistent) {
				t.Fatalf("err = %v, want ErrMaximalInconsistent", err)
			}
			if tips != nil {
				t.Fatalf("tips = %+v, want nil", tips)
			}
		})
	}
}
