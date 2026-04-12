package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"

	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

func TestIsTargetBodyLimitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "body exceeded size limit",
			err:  errors.New("body exceeded size limit 1048576"),
			want: true,
		},
		{
			name: "case insensitive body exceeded",
			err:  errors.New("Body Exceeded Size Limit 999"),
			want: true,
		},
		{
			name: "request body too large",
			err:  errors.New("request body is too large"),
			want: true,
		},
		{
			name: "payload too large",
			err:  errors.New("payload is too large for this endpoint"),
			want: true,
		},
		{
			name: "HTTP 413",
			err:  errors.New("server returned HTTP 413"),
			want: true,
		},
		{
			name: "unrelated error",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "partial match body without too large",
			err:  errors.New("request body is fine"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTargetBodyLimitError(tt.err)
			if got != tt.want {
				t.Errorf("isTargetBodyLimitError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestTargetBodyLimit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int64
	}{
		{
			name: "nil error",
			err:  nil,
			want: 0,
		},
		{
			name: "extracts numeric limit from error",
			err:  errors.New("body exceeded size limit 1048576"),
			want: 1048576,
		},
		{
			name: "no limit in error message",
			err:  errors.New("connection refused"),
			want: 0,
		},
		{
			name: "limit with surrounding text",
			err:  errors.New("push target refs: body exceeded size limit 536870912 bytes"),
			want: 536870912,
		},
		{
			name: "case insensitive match",
			err:  errors.New("Body Exceeded Size Limit 2097152"),
			want: 2097152,
		},
		{
			name: "no number after pattern",
			err:  errors.New("body exceeded size limit"),
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetBodyLimit(tt.err)
			if got != tt.want {
				t.Errorf("targetBodyLimit(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestAdaptiveNextProbeSpan(t *testing.T) {
	tests := []struct {
		name         string
		limit        int64
		measured     int
		selectedSpan int
		remaining    int
		want         int
	}{
		{
			name:         "grows span when measured pack is far below limit",
			limit:        1000,
			measured:     250,
			selectedSpan: 4,
			remaining:    20,
			want:         16,
		},
		{
			name:         "caps growth at remaining commits",
			limit:        1000,
			measured:     200,
			selectedSpan: 4,
			remaining:    6,
			want:         6,
		},
		{
			name:         "keeps minimum span of one",
			limit:        1000,
			measured:     4000,
			selectedSpan: 1,
			remaining:    10,
			want:         1,
		},
		{
			name:         "falls back to selected span without measurements",
			limit:        1000,
			measured:     0,
			selectedSpan: 3,
			remaining:    10,
			want:         3,
		},
		{
			name:         "caps fallback span at remaining",
			limit:        0,
			measured:     0,
			selectedSpan: 8,
			remaining:    5,
			want:         5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adaptiveNextProbeSpan(tt.limit, tt.measured, tt.selectedSpan, tt.remaining)
			if got != tt.want {
				t.Fatalf("adaptiveNextProbeSpan(%d, %d, %d, %d) = %d, want %d",
					tt.limit, tt.measured, tt.selectedSpan, tt.remaining, got, tt.want)
			}
		})
	}
}

type fakeBootstrapSource struct {
	fetchPack func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
}

func (f fakeBootstrapSource) FetchPack(
	ctx context.Context,
	conn *gitproto.Conn,
	desired map[plumbing.ReferenceName]gitproto.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	return f.fetchPack(ctx, conn, desired, targetRefs)
}

func (fakeBootstrapSource) FetchCommitGraph(context.Context, storer.Storer, *gitproto.Conn, gitproto.DesiredRef) error {
	return nil
}

func (fakeBootstrapSource) SupportsBootstrapBatch() bool { return true }

type fakeBootstrapPusher struct {
	pushPack     func(context.Context, []gitproto.PushCommand, io.ReadCloser) error
	pushCommands func(context.Context, []gitproto.PushCommand) error
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func (f fakeBootstrapPusher) PushPack(ctx context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
	return f.pushPack(ctx, cmds, pack)
}

func (f fakeBootstrapPusher) PushCommands(ctx context.Context, cmds []gitproto.PushCommand) error {
	if f.pushCommands == nil {
		return nil
	}
	return f.pushCommands(ctx, cmds)
}

func TestExecuteOneShotUsesTargetPusher(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	mainHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	var gotDesired map[plumbing.ReferenceName]gitproto.DesiredRef
	var gotCommands []gitproto.PushCommand

	result, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchPack: func(_ context.Context, _ *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, targetRefs map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				gotDesired = desired
				if targetRefs != nil {
					t.Fatalf("expected nil target refs during one-shot bootstrap fetch, got %v", targetRefs)
				}
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
				defer pack.Close()
				gotCommands = append([]gitproto.PushCommand(nil), cmds...)
				return nil
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef:  mainRef,
				TargetRef:  mainRef,
				SourceHash: mainHash,
				Kind:       planner.RefKindBranch,
			},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{},
	}, "empty target")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Pushed != 1 || !result.Relay || result.RelayMode != "bootstrap" || result.RelayReason != "empty target" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if gotDesired[mainRef].SourceHash != mainHash {
		t.Fatalf("desired source hash = %s, want %s", gotDesired[mainRef].SourceHash, mainHash)
	}
	if len(gotCommands) != 1 || gotCommands[0].Name != mainRef || gotCommands[0].New != mainHash {
		t.Fatalf("unexpected push commands: %+v", gotCommands)
	}
}

func TestExecuteOneShotClosesPackOnPushError(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	mainHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	pack := &trackingReadCloser{Reader: bytes.NewReader([]byte("PACK"))}

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchPack: func(_ context.Context, _ *gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return pack, nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				_ = pack.Close()
				return errors.New("boom")
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef:  mainRef,
				TargetRef:  mainRef,
				SourceHash: mainHash,
				Kind:       planner.RefKindBranch,
			},
		},
	}, "empty target")
	if err == nil || err.Error() != "push target refs: boom" {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed on push error")
	}
}

func TestExecuteRequiresTargetPusherBeforeFetch(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	mainHash := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	tests := []struct {
		name         string
		batchMaxPack int64
	}{
		{name: "one-shot bootstrap", batchMaxPack: 0},
		{name: "batched bootstrap", batchMaxPack: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calledFetch := false
			_, err := Execute(context.Background(), Params{
				SourceService: fakeBootstrapSource{
					fetchPack: func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
						calledFetch = true
						return io.NopCloser(bytes.NewReader(nil)), nil
					},
				},
				DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
					mainRef: {
						SourceRef:  mainRef,
						TargetRef:  mainRef,
						SourceHash: mainHash,
						Kind:       planner.RefKindBranch,
					},
				},
				TargetRefs:   map[plumbing.ReferenceName]plumbing.Hash{},
				BatchMaxPack: tt.batchMaxPack,
			}, "missing pusher")
			if err == nil || err.Error() != "bootstrap strategy requires TargetPusher" {
				t.Fatalf("Execute() error = %v, want missing TargetPusher", err)
			}
			if calledFetch {
				t.Fatal("expected bootstrap to fail before fetching source pack")
			}
		})
	}
}

func TestExecuteRequiresTargetPusherBeforeGitHubPreflight(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Fatalf("unexpected preflight request: %s %s", r.Method, r.URL.String())
	}))
	defer server.Close()

	prevBaseURL := GitHubRepoAPIBaseURL
	GitHubRepoAPIBaseURL = server.URL
	defer func() { GitHubRepoAPIBaseURL = prevBaseURL }()

	ep, err := transport.NewEndpoint("https://github.com/acme/repo.git")
	if err != nil {
		t.Fatalf("transport.NewEndpoint: %v", err)
	}

	_, err = Execute(context.Background(), Params{
		SourceConn: &gitproto.Conn{
			Endpoint: ep,
			HTTP:     server.Client(),
		},
		SourceService: fakeBootstrapSource{
			fetchPack: func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				t.Fatal("unexpected fetch")
				return nil, nil
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			plumbing.NewBranchReferenceName("main"): {
				SourceRef:  plumbing.NewBranchReferenceName("main"),
				TargetRef:  plumbing.NewBranchReferenceName("main"),
				SourceHash: plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc"),
				Kind:       planner.RefKindBranch,
			},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{},
	}, "missing pusher")
	if err == nil || err.Error() != "bootstrap strategy requires TargetPusher" {
		t.Fatalf("Execute() error = %v, want missing TargetPusher", err)
	}
	if requests != 0 {
		t.Fatalf("expected no GitHub preflight requests, got %d", requests)
	}
}
