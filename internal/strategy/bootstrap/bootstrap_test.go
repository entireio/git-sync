package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"

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

func TestEstimateBatchCount(t *testing.T) {
	tests := []struct {
		name         string
		chainLen     int64
		batchMaxPack int64
		want         int
	}{
		{
			name:         "zero chain length returns 1",
			chainLen:     0,
			batchMaxPack: 1024 * 1024,
			want:         1,
		},
		{
			name:         "negative chain length returns 1",
			chainLen:     -5,
			batchMaxPack: 1024 * 1024,
			want:         1,
		},
		{
			name:         "zero batch max pack returns 1",
			chainLen:     100,
			batchMaxPack: 0,
			want:         1,
		},
		{
			name:         "negative batch max pack returns 1",
			chainLen:     100,
			batchMaxPack: -1,
			want:         1,
		},
		{
			name:         "small chain fitting in one batch",
			chainLen:     10,
			batchMaxPack: 10 * estimatedBytesPerCommit,
			want:         1,
		},
		{
			name:         "large chain needing multiple batches",
			chainLen:     1000,
			batchMaxPack: 100 * estimatedBytesPerCommit,
			want:         10,
		},
		{
			name:         "ceil division when not evenly divisible",
			chainLen:     101,
			batchMaxPack: 100 * estimatedBytesPerCommit,
			want:         2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateBatchCount(tt.chainLen, tt.batchMaxPack)
			if got != tt.want {
				t.Fatalf("estimateBatchCount(%d, %d) = %d, want %d",
					tt.chainLen, tt.batchMaxPack, got, tt.want)
			}
		})
	}
}

func TestEvenCheckpoints(t *testing.T) {
	makeHashes := func(n int) []plumbing.Hash {
		hashes := make([]plumbing.Hash, n)
		for i := range hashes {
			hashes[i] = plumbing.NewHash(fmt.Sprintf("%040d", i))
		}
		return hashes
	}

	t.Run("1 batch returns just tip", func(t *testing.T) {
		chain := makeHashes(10)
		got := evenCheckpoints(chain, 1)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0] != chain[9] {
			t.Fatalf("got %s, want tip %s", got[0], chain[9])
		}
	})

	t.Run("3 batches on 10-element chain", func(t *testing.T) {
		chain := makeHashes(10)
		got := evenCheckpoints(chain, 3)
		// batchSize = 10/3 = 3
		// checkpoints at indices: (1)*3-1=2, (2)*3-1=5, then tip=9
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[0] != chain[2] {
			t.Fatalf("got[0] = %s, want chain[2] = %s", got[0], chain[2])
		}
		if got[1] != chain[5] {
			t.Fatalf("got[1] = %s, want chain[5] = %s", got[1], chain[5])
		}
		if got[2] != chain[9] {
			t.Fatalf("got[2] = %s, want chain[9] (tip) = %s", got[2], chain[9])
		}
	})

	t.Run("more batches than chain with single element returns just tip", func(t *testing.T) {
		chain := makeHashes(1)
		got := evenCheckpoints(chain, 5)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0] != chain[0] {
			t.Fatalf("got %s, want tip %s", got[0], chain[0])
		}
	})

	t.Run("more batches than chain with multi-element chain returns just tip", func(t *testing.T) {
		chain := makeHashes(3)
		got := evenCheckpoints(chain, 10)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0] != chain[2] {
			t.Fatalf("got %s, want tip %s", got[0], chain[2])
		}
	})
}

func TestCheckPackSizeAndSubdivide(t *testing.T) {
	// Build a minimal PACK header: "PACK" + version 2 + object count
	makePackHeader := func(objectCount uint32) []byte {
		var h [12]byte
		copy(h[:4], "PACK")
		h[4], h[5], h[6], h[7] = 0, 0, 0, 2 // version 2
		h[8] = byte(objectCount >> 24)
		h[9] = byte(objectCount >> 16)
		h[10] = byte(objectCount >> 8)
		h[11] = byte(objectCount)
		return h[:]
	}

	t.Run("small pack proceeds without subdivide", func(t *testing.T) {
		header := makePackHeader(100) // 100 * 750 = 75000 bytes estimated
		body := append(header, []byte("packdata")...)
		r := io.NopCloser(bytes.NewReader(body))
		subdivided := false
		got, err := checkPackSizeAndSubdivide(r, 1_000_000, func() bool {
			subdivided = true
			return true
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil reader")
		}
		if subdivided {
			t.Fatal("should not subdivide small pack")
		}
		// Verify the PACK header was prepended back
		out, _ := io.ReadAll(got)
		if string(out[:4]) != "PACK" {
			t.Fatalf("expected PACK header preserved, got %q", out[:4])
		}
	})

	t.Run("large pack triggers subdivide", func(t *testing.T) {
		header := makePackHeader(5_000_000) // 5M * 750 = 3.75 GiB estimated
		r := io.NopCloser(bytes.NewReader(header))
		subdivided := false
		got, err := checkPackSizeAndSubdivide(r, 2_000_000_000, func() bool {
			subdivided = true
			return true
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatal("expected nil reader after subdivide")
		}
		if !subdivided {
			t.Fatal("expected subdivide for large pack")
		}
	})

	t.Run("non-PACK data proceeds without subdivide", func(t *testing.T) {
		r := io.NopCloser(bytes.NewReader([]byte("not a pack file at all")))
		got, err := checkPackSizeAndSubdivide(r, 100, func() bool {
			t.Fatal("should not subdivide non-pack data")
			return true
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil reader for non-pack data")
		}
	})
}

func TestSubdivideCheckpoints(t *testing.T) {
	makeHashes := func(n int) []plumbing.Hash {
		hashes := make([]plumbing.Hash, n)
		for i := range hashes {
			hashes[i] = plumbing.NewHash(fmt.Sprintf("%040d", i))
		}
		return hashes
	}

	chain := makeHashes(10) // indices 0..9

	t.Run("splits single checkpoint at midpoint", func(t *testing.T) {
		// current=chain[0], remaining=[chain[9]] → insert chain[4] as midpoint
		got := subdivideCheckpoints(chain, chain[0], []plumbing.Hash{chain[9]})
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2: %v", len(got), got)
		}
		if got[0] != chain[4] {
			t.Fatalf("got[0] = %s, want midpoint chain[4] = %s", got[0], chain[4])
		}
		if got[1] != chain[9] {
			t.Fatalf("got[1] = %s, want tip chain[9] = %s", got[1], chain[9])
		}
	})

	t.Run("zero current starts from beginning", func(t *testing.T) {
		// current=zero, remaining=[chain[9]] → insert chain[4] as midpoint
		got := subdivideCheckpoints(chain, plumbing.ZeroHash, []plumbing.Hash{chain[9]})
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0] != chain[4] {
			t.Fatalf("got[0] = %s, want chain[4]", got[0])
		}
	})

	t.Run("adjacent commits cannot split further", func(t *testing.T) {
		// current=chain[8], remaining=[chain[9]] → gap=1, no midpoint
		got := subdivideCheckpoints(chain, chain[8], []plumbing.Hash{chain[9]})
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
	})

	t.Run("multiple remaining checkpoints each get split", func(t *testing.T) {
		// current=zero, remaining=[chain[4], chain[9]]
		// first: gap(-1→4)=5, mid=chain[1]
		// second: gap(4→9)=5, mid=chain[6]
		got := subdivideCheckpoints(chain, plumbing.ZeroHash, []plumbing.Hash{chain[4], chain[9]})
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4: %v", len(got), got)
		}
		if got[0] != chain[1] {
			t.Fatalf("got[0] = %s, want chain[1]", got[0])
		}
		if got[1] != chain[4] {
			t.Fatalf("got[1] = %s, want chain[4]", got[1])
		}
		if got[2] != chain[6] {
			t.Fatalf("got[2] = %s, want chain[6]", got[2])
		}
		if got[3] != chain[9] {
			t.Fatalf("got[3] = %s, want chain[9]", got[3])
		}
	})
}

type fakeBootstrapSource struct {
	fetchPack             func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
	fetchCommitGraph      func(context.Context, storer.Storer, *gitproto.Conn, gitproto.DesiredRef) error
	fetchCommitGraphDepth func(context.Context, storer.Storer, *gitproto.Conn, gitproto.DesiredRef, int) error
}

func (f fakeBootstrapSource) FetchPack(
	ctx context.Context,
	conn *gitproto.Conn,
	desired map[plumbing.ReferenceName]gitproto.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	return f.fetchPack(ctx, conn, desired, targetRefs)
}

func (f fakeBootstrapSource) FetchCommitGraph(
	ctx context.Context,
	store storer.Storer,
	conn *gitproto.Conn,
	ref gitproto.DesiredRef,
) error {
	if f.fetchCommitGraph != nil {
		return f.fetchCommitGraph(ctx, store, conn, ref)
	}
	return nil
}

func (f fakeBootstrapSource) FetchCommitGraphDepth(
	ctx context.Context,
	store storer.Storer,
	conn *gitproto.Conn,
	ref gitproto.DesiredRef,
	depth int,
) error {
	if f.fetchCommitGraphDepth != nil {
		return f.fetchCommitGraphDepth(ctx, store, conn, ref, depth)
	}
	if f.fetchCommitGraph != nil {
		return f.fetchCommitGraph(ctx, store, conn, ref)
	}
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

type interruptedReadCloser struct {
	first  []byte
	err    error
	stage  int
	closed bool
}

func (r *interruptedReadCloser) Read(p []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage = 1
		return copy(p, r.first), nil
	default:
		return 0, r.err
	}
}

func (r *interruptedReadCloser) Close() error {
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

func TestExecuteOneShotClosesPackWhenPusherDoesNot(t *testing.T) {
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
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, _ io.ReadCloser) error {
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
	}, "empty target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pack.closed {
		t.Fatal("expected strategy to close pack after successful push")
	}
}

func TestExecuteBatchedClosesCheckpointPackOnPushError(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	hashes := makeLinearCommitChain(t, 1)
	pack := &trackingReadCloser{Reader: bytes.NewReader([]byte("PACK"))}

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitGraph: func(_ context.Context, store storer.Storer, _ *gitproto.Conn, _ gitproto.DesiredRef) error {
				writeLinearCommitChain(t, store, 1)
				return nil
			},
			fetchPack: func(_ context.Context, _ *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return pack, nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, _ io.ReadCloser) error {
				return errors.New("boom")
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef:  mainRef,
				TargetRef:  mainRef,
				SourceHash: hashes[len(hashes)-1],
				Kind:       planner.RefKindBranch,
				Label:      "main",
			},
		},
		TargetRefs:    map[plumbing.ReferenceName]plumbing.Hash{},
		TargetMaxPack: 10,
	}, "empty target")
	if err == nil || !strings.Contains(err.Error(), "push bootstrap batch") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pack.closed {
		t.Fatal("expected strategy to close checkpoint pack on push error")
	}
}

func TestExecuteBatchedClosesCheckpointPackOnReadInterruption(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	hashes := makeLinearCommitChain(t, 1)
	pack := &interruptedReadCloser{first: []byte("PACK"), err: io.ErrUnexpectedEOF}

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitGraph: func(_ context.Context, store storer.Storer, _ *gitproto.Conn, _ gitproto.DesiredRef) error {
				writeLinearCommitChain(t, store, 1)
				return nil
			},
			fetchPack: func(_ context.Context, _ *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return pack, nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				_, err := io.Copy(io.Discard, pack)
				return err
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef:  mainRef,
				TargetRef:  mainRef,
				SourceHash: hashes[len(hashes)-1],
				Kind:       planner.RefKindBranch,
				Label:      "main",
			},
		},
		TargetRefs:    map[plumbing.ReferenceName]plumbing.Hash{},
		TargetMaxPack: 10,
	}, "empty target")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected interrupted read error, got %v", err)
	}
	if !pack.closed {
		t.Fatal("expected strategy to close checkpoint pack after read interruption")
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
				TargetRefs:    map[plumbing.ReferenceName]plumbing.Hash{},
				TargetMaxPack: tt.batchMaxPack,
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

func makeLinearCommitChain(tb testing.TB, count int) []plumbing.Hash {
	tb.Helper()
	store := memory.NewStorage()
	return writeLinearCommitChain(tb, store, count)
}

func writeLinearCommitChain(tb testing.TB, store storer.Storer, count int) []plumbing.Hash {
	tb.Helper()
	hashes := make([]plumbing.Hash, 0, count)
	for i := 0; i < count; i++ {
		obj := store.NewEncodedObject()
		var parents []plumbing.Hash
		if len(hashes) > 0 {
			parents = []plumbing.Hash{hashes[len(hashes)-1]}
		}
		when := time.Unix(int64(i+1), 0).UTC()
		commit := &object.Commit{
			Author:       object.Signature{Name: "test", Email: "test@example.com", When: when},
			Committer:    object.Signature{Name: "test", Email: "test@example.com", When: when},
			Message:      fmt.Sprintf("commit-%d", i),
			TreeHash:     plumbing.ZeroHash,
			ParentHashes: parents,
		}
		if err := commit.Encode(obj); err != nil {
			tb.Fatalf("encode commit %d: %v", i, err)
		}
		hash, err := store.SetEncodedObject(obj)
		if err != nil {
			tb.Fatalf("store commit %d: %v", i, err)
		}
		hashes = append(hashes, hash)
	}
	return hashes
}
