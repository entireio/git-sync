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

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

func TestHoistSourceHeadPlan(t *testing.T) {
	main := plumbing.NewBranchReferenceName("main")
	master := plumbing.NewBranchReferenceName("master")
	alpha := plumbing.NewBranchReferenceName("alpha")
	stable := plumbing.NewBranchReferenceName("stable")

	plan := func(source, target plumbing.ReferenceName) planner.BranchPlan {
		return planner.BranchPlan{SourceRef: source, TargetRef: target}
	}

	tests := []struct {
		name           string
		plans          []planner.BranchPlan
		head           plumbing.ReferenceName
		wantTargetRefs []plumbing.ReferenceName
	}{
		{
			name:           "hoists matching plan to front",
			plans:          []planner.BranchPlan{plan(alpha, alpha), plan(main, main), plan(master, master)},
			head:           main,
			wantTargetRefs: []plumbing.ReferenceName{main, alpha, master},
		},
		{
			name:           "matches on SourceRef, hoists mapped TargetRef",
			plans:          []planner.BranchPlan{plan(alpha, alpha), plan(master, stable)},
			head:           master,
			wantTargetRefs: []plumbing.ReferenceName{stable, alpha},
		},
		{
			name:           "already first stays put",
			plans:          []planner.BranchPlan{plan(main, main), plan(alpha, alpha)},
			head:           main,
			wantTargetRefs: []plumbing.ReferenceName{main, alpha},
		},
		{
			name:           "empty source HEAD is a no-op",
			plans:          []planner.BranchPlan{plan(alpha, alpha), plan(main, main)},
			head:           "",
			wantTargetRefs: []plumbing.ReferenceName{alpha, main},
		},
		{
			name:           "no matching plan is a no-op",
			plans:          []planner.BranchPlan{plan(alpha, alpha), plan(master, master)},
			head:           main,
			wantTargetRefs: []plumbing.ReferenceName{alpha, master},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hoistSourceHeadPlan(tt.plans, tt.head)
			if len(got) != len(tt.wantTargetRefs) {
				t.Fatalf("length mismatch: got %d, want %d", len(got), len(tt.wantTargetRefs))
			}
			for i, want := range tt.wantTargetRefs {
				if got[i].TargetRef != want {
					t.Errorf("position %d: got %q, want %q", i, got[i].TargetRef, want)
				}
			}
		})
	}
}

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

func TestIsTargetPushDeadlineError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{
			name: "github receive-pack 408",
			err:  errors.New("push target refs: target receive-pack: post RPC stream body: http 408: https://github.com/o/r.git/git-receive-pack"),
			want: true,
		},
		{
			name: "gateway 504",
			err:  errors.New("target receive-pack: http 504: gateway timeout"),
			want: true,
		},
		{
			name: "body limit is not a deadline",
			err:  errors.New("body exceeded size limit 1048576"),
			want: false,
		},
		{
			name: "413 is not a deadline",
			err:  errors.New("http 413: payload too large"),
			want: false,
		},
		{
			name: "unrelated error",
			err:  errors.New("connection refused"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTargetPushDeadlineError(tt.err); got != tt.want {
				t.Errorf("isTargetPushDeadlineError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsBatchableTargetPushError(t *testing.T) {
	// Per-status edge cases are covered by TestIsTargetBodyLimitError and
	// TestIsTargetPushDeadlineError; this only confirms the OR wires both in.
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "body limit", err: errors.New("body exceeded size limit 1048576"), want: true},
		{name: "deadline", err: errors.New("http 408: request timeout"), want: true},
		{name: "unrelated", err: errors.New("connection refused"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBatchableTargetPushError(tt.err); got != tt.want {
				t.Errorf("isBatchableTargetPushError(%v) = %v, want %v", tt.err, got, tt.want)
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
	t.Run("small pack proceeds without subdivide", func(t *testing.T) {
		header := makePackHeader(100) // 100 * 750 = 75000 bytes estimated
		body := make([]byte, 0, len(header)+len("packdata"))
		body = append(body, header...)
		body = append(body, []byte("packdata")...)
		r := io.NopCloser(bytes.NewReader(body))
		subdivided := false
		got, count, err := checkPackSizeAndSubdivide(r, 1_000_000, estimatedBytesPerObject, func(int64) bool {
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
		if count != 100 {
			t.Errorf("expected objectCount=100, got %d", count)
		}
		// Verify the PACK header was prepended back
		out, err2 := io.ReadAll(got)
		if err2 != nil {
			t.Fatalf("unexpected ReadAll error: %v", err2)
		}
		if string(out[:4]) != "PACK" {
			t.Fatalf("expected PACK header preserved, got %q", out[:4])
		}
	})

	t.Run("large pack triggers subdivide", func(t *testing.T) {
		header := makePackHeader(5_000_000) // 5M * 750 = 3.75 GiB estimated
		r := io.NopCloser(bytes.NewReader(header))
		subdivided := false
		got, count, err := checkPackSizeAndSubdivide(r, 2_000_000_000, estimatedBytesPerObject, func(estimated int64) bool {
			subdivided = true
			if estimated <= 0 {
				t.Fatalf("subdivide should receive a positive estimate, got %d", estimated)
			}
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
		if count != 5_000_000 {
			t.Errorf("expected objectCount=5_000_000 even on subdivide path, got %d", count)
		}
	})

	t.Run("calibrated bytesPerObject catches blob-heavy pack the default would miss", func(t *testing.T) {
		// 50,000 objects at the static 750-byte estimate is ~36 MB —
		// would slip past a 500 MB limit. With a calibrated 12 KiB/object
		// it's ~600 MB and must trigger subdivide. Mirrors the real
		// Cloudflare-shaped repo where the static heuristic is 10–20×
		// too low.
		header := makePackHeader(50_000)
		r := io.NopCloser(bytes.NewReader(header))
		subdivided := false
		_, _, err := checkPackSizeAndSubdivide(r, 500*1024*1024, 12*1024, func(int64) bool {
			subdivided = true
			return true
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !subdivided {
			t.Fatal("calibrated estimate should have triggered subdivide")
		}
	})

	t.Run("zero or negative bytesPerObject falls back to default", func(t *testing.T) {
		// 5M objects × default 750 bytes = 3.75 GB, exceeds 2 GB → subdivide.
		// Confirms the function rejects an invalid calibrated value
		// instead of multiplying by 0 and skipping subdivision.
		header := makePackHeader(5_000_000)
		r := io.NopCloser(bytes.NewReader(header))
		subdivided := false
		_, _, err := checkPackSizeAndSubdivide(r, 2_000_000_000, 0, func(int64) bool {
			subdivided = true
			return true
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !subdivided {
			t.Fatal("invalid bytesPerObject must fall back to the default and still subdivide")
		}
	})

	t.Run("non-PACK data proceeds without subdivide", func(t *testing.T) {
		r := io.NopCloser(bytes.NewReader([]byte("not a pack file at all")))
		got, count, err := checkPackSizeAndSubdivide(r, 100, estimatedBytesPerObject, func(int64) bool {
			t.Fatal("should not subdivide non-pack data")
			return true
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil reader for non-pack data")
		}
		if count != 0 {
			t.Errorf("non-PACK data should report 0 objectCount, got %d", count)
		}
	})
}

func TestCalibrateBytesPerObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		sentBytes   int64
		objectCount int64
		current     int64
		want        int64 // 0 means "no improvement"
	}{
		{
			name: "no signal returns 0",
		},
		{
			// Cloudflare scenario: 528 MiB sent across 64,696 objects.
			// 2 × 528*1024*1024 / 64696 = 17,115 bytes/object — well
			// above the 750 default.
			name:        "cloudflare-like calibration ratchets up the default",
			sentBytes:   528 * 1024 * 1024,
			objectCount: 64_696,
			current:     750,
			want:        17_115,
		},
		{
			// Calibration must not regress: a smaller sub-pack giving a
			// lower observed lower-bound shouldn't lower the cumulative
			// estimate — the heaviest observation wins.
			name:        "smaller observation does not lower the estimate",
			sentBytes:   100 * 1024 * 1024,
			objectCount: 100_000,
			current:     17_115,
			want:        0, // observed (~2097) < current
		},
		{
			name:        "negative sent bytes returns 0",
			sentBytes:   -1,
			objectCount: 100,
			want:        0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := calibrateBytesPerObject(c.sentBytes, c.objectCount, c.current)
			if got != c.want {
				t.Errorf("calibrateBytesPerObject(%d, %d, %d) = %d, want %d",
					c.sentBytes, c.objectCount, c.current, got, c.want)
			}
		})
	}
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

func TestShouldAbortPush(t *testing.T) {
	t.Parallel()
	const cap500 = 500 * 1024 * 1024
	cases := []struct {
		name         string
		bytesSent    int64
		objectsSent  int64
		totalObjects int64
		budget       int64
		want         bool
	}{
		{
			name:   "no budget never aborts",
			budget: 0, bytesSent: 1 << 30, want: false,
		},
		{
			name:      "tiny upload below floor never aborts even at full budget",
			bytesSent: 1024, budget: cap500, want: false,
		},
		{
			// Header parsed, balanced pack, projection well under cap.
			// 50 MiB sent for 25% of objects projects to 200 MiB total.
			name:        "projection under threshold proceeds",
			bytesSent:   50 * 1024 * 1024,
			objectsSent: 25, totalObjects: 100,
			budget: cap500, want: false,
		},
		{
			// Cloudflare-shaped front-loaded pack: 50 MiB sent and only
			// 5% of objects done means projected ≈ 1 GiB > 95% of cap.
			name:        "front-loaded projection trips abort",
			bytesSent:   50 * 1024 * 1024,
			objectsSent: 5, totalObjects: 100,
			budget: cap500, want: true,
		},
		{
			// No object signal yet (header still in flight or scanner
			// behind) — fall back to bytes ≥ 95% of budget.
			name:      "no objects, simple threshold under budget",
			bytesSent: 400 * 1024 * 1024,
			budget:    cap500, want: false,
		},
		{
			name:      "no objects, simple threshold over budget",
			bytesSent: 480 * 1024 * 1024,
			budget:    cap500, want: true,
		},
		{
			// Late-stage projection: objectsSent has caught up with
			// totalObjects so projection ≈ bytesSent. Must not flap.
			name:        "near-end matched ratio projects to current bytes",
			bytesSent:   450 * 1024 * 1024,
			objectsSent: 98, totalObjects: 100,
			budget: cap500, want: false,
		},
		{
			// Learned proxy budget below the projection-path floor:
			// the simple "we already crossed the threshold" check
			// must still fire, otherwise client-side abort never
			// triggers and every retry pays for full server-side
			// rejection.
			name:      "small budget under floor, sent crosses threshold",
			bytesSent: 5 * 1024 * 1024,
			budget:    5 * 1024 * 1024, want: true,
		},
		{
			// Same small budget but well under threshold (95% of 5
			// MiB ≈ 4.75 MiB). The floor still sensibly suppresses
			// projection-based aborts.
			name:      "small budget under floor, sent under threshold",
			bytesSent: 1 * 1024 * 1024,
			budget:    5 * 1024 * 1024, want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := shouldAbortPush(c.bytesSent, c.objectsSent, c.totalObjects, c.budget)
			if got != c.want {
				t.Errorf("shouldAbortPush(%d, %d, %d, %d) = %v, want %v",
					c.bytesSent, c.objectsSent, c.totalObjects, c.budget, got, c.want)
			}
		})
	}
}

func TestEffectiveObjectsSent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		objectsSent  int64
		totalObjects int64
		abortedEarly bool
		want         int64
	}{
		{
			// Header parsed, the abort floor (8 MiB) was exhausted by
			// a single front-loaded large blob before it finished
			// scanning. The bug: without effective=1, calibration
			// divides sentBytes by the full pack count and projection
			// stays at sentBytes — the factor calculation collapses
			// back to ~2 every retry, recreating the slow 1→2→4→…
			// convergence streaming-pack-parse is meant to remove.
			name:        "front-loaded blob abort treats first object as observed",
			objectsSent: 0, totalObjects: 100,
			abortedEarly: true, want: 1,
		},
		{
			name:        "no header parsed leaves zero",
			objectsSent: 0, totalObjects: 0,
			abortedEarly: true, want: 0,
		},
		{
			// Server-side rejection (not self-aborted): we don't
			// invent an observation, since sentBytes here is the
			// server's actual cutoff rather than our floor. Falling
			// back to packObjectCount is the right divisor.
			name:        "header parsed but server-rejected, no synthetic observation",
			objectsSent: 0, totalObjects: 100,
			abortedEarly: false, want: 0,
		},
		{
			name:        "actual observation passes through",
			objectsSent: 12, totalObjects: 100,
			abortedEarly: true, want: 12,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := effectiveObjectsSent(c.objectsSent, c.totalObjects, c.abortedEarly)
			if got != c.want {
				t.Errorf("effectiveObjectsSent(%d, %d, %v) = %d, want %d",
					c.objectsSent, c.totalObjects, c.abortedEarly, got, c.want)
			}
		})
	}
}

func TestNextSelfImposedBudget(t *testing.T) {
	t.Parallel()
	const oneHundredMiB = 100 * 1024 * 1024
	const fiveMiB = 5 * 1024 * 1024
	cases := []struct {
		name         string
		current      int64
		parsedLimit  int64
		sentBytes    int64
		abortedEarly bool
		want         int64
	}{
		{
			// Self-aborted means we triggered the cut, not the server,
			// so sentBytes is just the abort floor — never useful for
			// ratcheting the budget.
			name:    "self-aborted leaves budget unchanged",
			current: oneHundredMiB, parsedLimit: 0, sentBytes: minBytesBeforeAbort,
			abortedEarly: true, want: oneHundredMiB,
		},
		{
			// The bug being fixed: a proxy rejected after 5 MiB but
			// announced "body exceeded size limit 104857600". The
			// authoritative number is the announced one — without
			// this, the budget would ratchet to 5 MiB and over-
			// subdivide forever after.
			name:    "parsed limit beats sent bytes when both present",
			current: 0, parsedLimit: oneHundredMiB, sentBytes: fiveMiB,
			abortedEarly: false, want: oneHundredMiB,
		},
		{
			// Cloudflare HTML 413: no parseable number, sentBytes is
			// our only signal that the server hit its body cap.
			name:    "sent bytes used when no parsed limit",
			current: 0, parsedLimit: 0, sentBytes: fiveMiB,
			abortedEarly: false, want: fiveMiB,
		},
		{
			// Ratchet only goes down: a parsed limit larger than the
			// current ceiling must be ignored — we already know a
			// tighter bound from a previous run.
			name:    "larger parsed limit ignored when current is tighter",
			current: fiveMiB, parsedLimit: oneHundredMiB, sentBytes: 0,
			abortedEarly: false, want: fiveMiB,
		},
		{
			// Same invariant applied to the sent-bytes fallback.
			name:    "larger sent bytes ignored when current is tighter",
			current: fiveMiB, parsedLimit: 0, sentBytes: oneHundredMiB,
			abortedEarly: false, want: fiveMiB,
		},
		{
			// No signal at all: keep the current value.
			name:    "no signal leaves budget unchanged",
			current: oneHundredMiB, parsedLimit: 0, sentBytes: 0,
			abortedEarly: false, want: oneHundredMiB,
		},
		{
			// Initial budget zero accepts whichever signal is present.
			name:    "zero current accepts parsed limit",
			current: 0, parsedLimit: oneHundredMiB, sentBytes: 0,
			abortedEarly: false, want: oneHundredMiB,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := nextSelfImposedBudget(c.current, c.parsedLimit, c.sentBytes, c.abortedEarly)
			if got != c.want {
				t.Errorf("nextSelfImposedBudget(%d, %d, %d, %v) = %d, want %d",
					c.current, c.parsedLimit, c.sentBytes, c.abortedEarly, got, c.want)
			}
		})
	}
}

func TestObservedSubdivisionFactor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		sentBytes int64
		limit     int64
		want      int
	}{
		{
			name: "no signal falls back to halving",
			want: 2,
		},
		{
			// Sent comfortably under the limit (server announced limit
			// without cutting mid-stream) — 2× safety is enough.
			name:      "sent well below limit uses conservative 2x multiplier",
			sentBytes: 100, limit: 1000, want: 2,
		},
		{
			// At/over the limit (server cut mid-stream) — switch to 4×.
			// 1000×4/1000 = 4.
			name:      "sent at limit assumed capped, uses 4x multiplier",
			sentBytes: 1000, limit: 1000, want: 4,
		},
		{
			// Cloudflare-shaped scenario: ~524 MiB sent before 413 against
			// a 500 MiB cap. Treat as capped → 4× multiplier:
			// ceil(524*4/500) = 5. One round jumps 1 → 8 instead of 1 → 4.
			name:      "cloudflare-like 524 MiB rejected at 500 MiB → 5 packs",
			sentBytes: 524 * 1024 * 1024,
			limit:     500 * 1024 * 1024,
			want:      5,
		},
		{
			// 8 GiB pack against a 256 MiB cap → factor 128 (4×32 due to
			// the at-cap multiplier). Ensures one informed jump covers
			// even pathologically oversized packs.
			name:      "much larger pack triggers correspondingly large factor",
			sentBytes: 8 * 1024 * 1024 * 1024,
			limit:     256 * 1024 * 1024,
			want:      128,
		},
		{
			// Just under the 90% threshold — keeps the conservative 2×
			// multiplier. 800/1000 = 0.8, threshold 0.9.
			name:      "sent at 80% of limit stays on 2x multiplier",
			sentBytes: 800, limit: 1000, want: 2,
		},
		{
			// Right at 90% — switches to the aggressive multiplier.
			// 900*10 == 1000*9, so condition is met.
			name:      "sent at exactly 90% switches to 4x",
			sentBytes: 900, limit: 1000, want: 4,
		},
		{
			name:      "negative sent bytes falls back",
			sentBytes: -1, limit: 100, want: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := observedSubdivisionFactor(c.sentBytes, c.limit)
			if got != c.want {
				t.Errorf("observedSubdivisionFactor(%d, %d) = %d, want %d",
					c.sentBytes, c.limit, got, c.want)
			}
		})
	}
}

func TestSubdivideToFactorReachesTarget(t *testing.T) {
	t.Parallel()
	makeHashes := func(n int) []plumbing.Hash {
		hashes := make([]plumbing.Hash, n)
		for i := range hashes {
			hashes[i] = plumbing.NewHash(fmt.Sprintf("%040d", i))
		}
		return hashes
	}
	chain := makeHashes(64)

	// Starting from one checkpoint, asking for a factor of 4 should split
	// twice (1 → 2 → 4) so the inner loop processes 4 sub-packs in a row
	// instead of dancing through 1 → 2 → 4 across three rejections.
	got := subdivideToFactor(chain, plumbing.ZeroHash, []plumbing.Hash{chain[63]}, 4)
	if len(got) < 4 {
		t.Errorf("expected at least 4 checkpoints for factor 4, got %d: %v", len(got), got)
	}
}

// TestSubdivideToFactorAlwaysProgresses guards the regression where a
// repeated 413 with sent_bytes ≈ limit produces factor=2 every round —
// the second rejection sees 2 remaining ≥ factor 2 and would skip
// subdivision entirely if the function bailed out on len(remaining) ≥
// targetCount, turning a recoverable retry into a hard failure.
func TestSubdivideToFactorAlwaysProgresses(t *testing.T) {
	t.Parallel()
	makeHashes := func(n int) []plumbing.Hash {
		hashes := make([]plumbing.Hash, n)
		for i := range hashes {
			hashes[i] = plumbing.NewHash(fmt.Sprintf("%040d", i))
		}
		return hashes
	}
	chain := makeHashes(64)

	// Mirrors the live scenario: after a 1 → 2 split, the second 413
	// arrives with factor=2 again. The function must still subdivide
	// (2 → 4) so the inner loop has new checkpoints to retry against.
	already := []plumbing.Hash{chain[31], chain[63]}
	got := subdivideToFactor(chain, plumbing.ZeroHash, already, 2)
	if len(got) <= len(already) {
		t.Errorf("must subdivide at least once even when factor ≤ remaining; got %d, want > %d",
			len(got), len(already))
	}
}

// TestSubdivideToFactorReturnsInputWhenChainExhausted verifies that
// subdivideToFactor stops when every remaining gap is already 1 commit
// — the only legitimate case for returning the input unchanged.
func TestSubdivideToFactorReturnsInputWhenChainExhausted(t *testing.T) {
	t.Parallel()
	makeHashes := func(n int) []plumbing.Hash {
		hashes := make([]plumbing.Hash, n)
		for i := range hashes {
			hashes[i] = plumbing.NewHash(fmt.Sprintf("%040d", i))
		}
		return hashes
	}
	chain := makeHashes(3)
	// Each consecutive commit is its own checkpoint — no further split possible.
	already := []plumbing.Hash{chain[0], chain[1], chain[2]}
	got := subdivideToFactor(chain, plumbing.ZeroHash, already, 16)
	if len(got) != len(already) {
		t.Errorf("with all gaps == 1 commit, subdivision must return input unchanged; got %d", len(got))
	}
}

func TestRecombineDropCount(t *testing.T) {
	t.Parallel()
	const limit = 50_000_000
	cases := []struct {
		name      string
		sentBytes int64
		limit     int64
		maxDrop   int
		want      int
	}{
		{
			// Pack used over half the limit: span already in the right
			// ballpark, no recombination.
			name: "above half target, no drop", sentBytes: 30_000_000, limit: limit, maxDrop: 100, want: 0,
		},
		{
			// Just under half: one doubling overshoots, so no drop.
			name: "just under half overshoots on double, no drop", sentBytes: 13_000_000, limit: limit, maxDrop: 100, want: 0,
		},
		{
			// 1 MB pack out of 50 MB limit: aim for 25 MB. log2(25/1) ≈ 4.6 → 4 doublings.
			name: "small pack ramps several doublings", sentBytes: 1_000_000, limit: limit, maxDrop: 100, want: 4,
		},
		{
			// 6.2 KB pack (the case from the trace): aim for 25 MB.
			// log2(25_000_000/6200) ≈ 11.97 → capped at hardCap=8.
			name: "tiny pack hits hard cap", sentBytes: 6_200, limit: limit, maxDrop: 100, want: 8,
		},
		{
			// Same tiny pack but only 3 checkpoints to drop: respect maxDrop.
			name: "maxDrop limits drop count", sentBytes: 6_200, limit: limit, maxDrop: 3, want: 3,
		},
		{
			name: "no headroom returns zero", sentBytes: 0, limit: limit, maxDrop: 100, want: 0,
		},
		{
			name: "no limit returns zero", sentBytes: 1024, limit: 0, maxDrop: 100, want: 0,
		},
		{
			name: "no slack returns zero", sentBytes: 1024, limit: limit, maxDrop: 0, want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := recombineDropCount(c.sentBytes, c.limit, c.maxDrop)
			if got != c.want {
				t.Errorf("recombineDropCount(%d, %d, %d) = %d, want %d",
					c.sentBytes, c.limit, c.maxDrop, got, c.want)
			}
		})
	}
}

func TestPackStreamObserverTracksBytes(t *testing.T) {
	t.Parallel()
	body := []byte("a packfile worth of bytes")
	o := newPackStreamObserver(io.NopCloser(bytes.NewReader(body)))
	out, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("observer must not alter content: got %q", out)
	}
	if o.Bytes() != int64(len(body)) {
		t.Errorf("observer.Bytes() = %d, want %d", o.Bytes(), len(body))
	}
	// Cleanly drains the Scanner goroutine. Closing twice should be
	// a no-op (the source is the closed io.NopCloser wrapping a
	// bytes.Reader).
	if err := o.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestOrderTrunkFirstPutsHEADBranchFirst(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	featureRef := plumbing.NewBranchReferenceName("feature")
	hotfixRef := plumbing.NewBranchReferenceName("hotfix")

	desired := []planner.DesiredRef{
		{SourceRef: featureRef, TargetRef: featureRef, Label: "feature"},
		{SourceRef: hotfixRef, TargetRef: hotfixRef, Label: "hotfix"},
		{SourceRef: mainRef, TargetRef: mainRef, Label: "main"},
	}

	ordered, trunkIdx := orderTrunkFirst(desired, mainRef)
	if trunkIdx != 0 {
		t.Fatalf("trunkIdx = %d, want 0", trunkIdx)
	}
	if ordered[0].SourceRef != mainRef {
		t.Fatalf("ordered[0] = %s, want main", ordered[0].SourceRef)
	}
	// Relative order of non-trunk refs preserved.
	if ordered[1].SourceRef != featureRef || ordered[2].SourceRef != hotfixRef {
		t.Fatalf("non-trunk relative order lost: %v", ordered)
	}
	// Original slice untouched.
	if desired[0].SourceRef != featureRef {
		t.Fatalf("orderTrunkFirst mutated input slice")
	}
}

func TestOrderTrunkFirstNoHEADLeavesOrder(t *testing.T) {
	a := planner.DesiredRef{SourceRef: plumbing.NewBranchReferenceName("a"), Label: "a"}
	b := planner.DesiredRef{SourceRef: plumbing.NewBranchReferenceName("b"), Label: "b"}
	desired := []planner.DesiredRef{a, b}

	ordered, trunkIdx := orderTrunkFirst(desired, "")
	if trunkIdx != -1 {
		t.Fatalf("trunkIdx = %d, want -1", trunkIdx)
	}
	if ordered[0].Label != "a" || ordered[1].Label != "b" {
		t.Fatalf("order changed without HEAD hint: %v", ordered)
	}
}

func TestOrderTrunkFirstHEADNotInDesired(t *testing.T) {
	a := planner.DesiredRef{SourceRef: plumbing.NewBranchReferenceName("a"), Label: "a"}
	desired := []planner.DesiredRef{a}

	ordered, trunkIdx := orderTrunkFirst(desired, plumbing.NewBranchReferenceName("main"))
	if trunkIdx != -1 {
		t.Fatalf("trunkIdx = %d, want -1 when HEAD filtered out", trunkIdx)
	}
	if len(ordered) != 1 || ordered[0].Label != "a" {
		t.Fatalf("unexpected order: %v", ordered)
	}
}

func TestBuildCheckpointHaves(t *testing.T) {
	t.Parallel()
	tempRef := plumbing.ReferenceName("refs/gitsync/bootstrap/heads/trunk")
	completedTrunk := plumbing.NewHash("1111111111111111111111111111111111111111")
	completedBranch := plumbing.ReferenceName("refs/heads/done")

	t.Run("empty pushed list copies completed refs only", func(t *testing.T) {
		t.Parallel()
		completed := map[plumbing.ReferenceName]plumbing.Hash{completedBranch: completedTrunk}
		got := buildCheckpointHaves(tempRef, nil, completed)
		if len(got) != 1 || got[completedBranch] != completedTrunk {
			t.Fatalf("expected only completed ref, got %#v", got)
		}
	})

	t.Run("each pushed checkpoint contributes a have hash", func(t *testing.T) {
		t.Parallel()
		// Topo ordering scenario: a side-branch commit (sideTip) was
		// pushed as its own checkpoint earlier. It is *not* an
		// ancestor of trunkTip, so declaring only trunkTip would let
		// the source resend sideTip's ancestry on the next merge fetch.
		// Verify both are in the resulting haves so the source can
		// prune.
		sideTip := plumbing.NewHash("2222222222222222222222222222222222222222")
		trunkTip := plumbing.NewHash("3333333333333333333333333333333333333333")
		got := buildCheckpointHaves(tempRef, []plumbing.Hash{sideTip, trunkTip}, nil)

		hashes := make(map[plumbing.Hash]bool, len(got))
		for _, h := range got {
			hashes[h] = true
		}
		if !hashes[sideTip] {
			t.Errorf("side-branch checkpoint %s missing from haves: %#v", sideTip, got)
		}
		if !hashes[trunkTip] {
			t.Errorf("trunk-tip checkpoint %s missing from haves: %#v", trunkTip, got)
		}
	})

	t.Run("zero hashes are skipped", func(t *testing.T) {
		t.Parallel()
		realHash := plumbing.NewHash("4444444444444444444444444444444444444444")
		got := buildCheckpointHaves(tempRef, []plumbing.Hash{plumbing.ZeroHash, realHash}, nil)
		if len(got) != 1 {
			t.Fatalf("zero hash should be skipped; got %#v", got)
		}
		for _, h := range got {
			if h != realHash {
				t.Fatalf("expected only %s, got %#v", realHash, got)
			}
		}
	})

	t.Run("synthetic ref names disambiguate duplicate hashes", func(t *testing.T) {
		t.Parallel()
		// The same hash can legitimately appear at multiple positions
		// (no constraint enforces uniqueness in pushedCheckpoints).
		// Synthetic per-position keys must not collapse them down to
		// one entry — though for the wire it doesn't matter because
		// the protocol layer dedupes hashes anyway.
		dup := plumbing.NewHash("5555555555555555555555555555555555555555")
		got := buildCheckpointHaves(tempRef, []plumbing.Hash{dup, dup}, nil)
		if len(got) != 2 {
			t.Fatalf("expected two distinct map entries for duplicate hash; got %#v", got)
		}
	})

	t.Run("completed refs are not overwritten by checkpoints", func(t *testing.T) {
		t.Parallel()
		// Synthetic checkpoint ref names follow tempRef-have-N. They
		// must not collide with caller-provided completedRefs keys,
		// or a topo run would silently lose a completed branch tip.
		completed := map[plumbing.ReferenceName]plumbing.Hash{
			completedBranch: completedTrunk,
		}
		other := plumbing.NewHash("6666666666666666666666666666666666666666")
		got := buildCheckpointHaves(tempRef, []plumbing.Hash{other}, completed)
		if got[completedBranch] != completedTrunk {
			t.Fatalf("completed ref %s lost: %#v", completedBranch, got)
		}
	})
}

func TestExecuteBatchedSubsumedBranchSkipsPack(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	featureRef := plumbing.NewBranchReferenceName("feature")
	// Linear chain: hashes[0] -> hashes[1] -> hashes[2]. main tip = hashes[2],
	// feature tip = hashes[0]. feature is entirely within main's ancestry, so
	// trunk-first planning should mark it subsumed and emit zero pack pushes
	// for it.
	hashes := makeLinearCommitChain(t, 3)
	mainHash := hashes[2]
	featureHash := hashes[0]

	var (
		graphFetches        int
		packFetches         int
		pushPackCalls       int
		pushCommandsBatches [][]gitproto.PushCommand
	)

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, ref gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				graphFetches++
				if ref.SourceRef != mainRef {
					t.Errorf("unexpected commit-graph fetch for %s; subsumed branch should have been skipped", ref.SourceRef)
				}
				store := memory.NewStorage()
				writeLinearCommitChain(t, store, 3)
				return parentsFromCommitChainStore(t, store), nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				packFetches++
				if _, ok := desired[featureRef]; ok {
					t.Errorf("unexpected pack fetch including feature ref: %+v", desired)
				}
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushPackCalls++
				_ = pack.Close()
				return nil
			},
			pushCommands: func(_ context.Context, cmds []gitproto.PushCommand) error {
				pushCommandsBatches = append(pushCommandsBatches, append([]gitproto.PushCommand(nil), cmds...))
				return nil
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef:    {SourceRef: mainRef, TargetRef: mainRef, SourceHash: mainHash, Kind: planner.RefKindBranch, Label: "main"},
			featureRef: {SourceRef: featureRef, TargetRef: featureRef, SourceHash: featureHash, Kind: planner.RefKindBranch, Label: "feature"},
		},
		TargetRefs:       map[plumbing.ReferenceName]plumbing.Hash{},
		SourceHeadTarget: mainRef,
		TargetMaxPack:    1024 * 1024,
	}, "empty target")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if graphFetches != 1 {
		t.Errorf("fetchCommitGraph called %d times, want 1 (trunk only)", graphFetches)
	}
	if packFetches != 1 {
		t.Errorf("fetchPack called %d times, want 1 (trunk only)", packFetches)
	}
	if pushPackCalls != 1 {
		t.Errorf("PushPack called %d times, want 1 (trunk only)", pushPackCalls)
	}

	var foundFeatureCreate bool
	for _, cmds := range pushCommandsBatches {
		for _, cmd := range cmds {
			if cmd.Name == featureRef && cmd.New == featureHash && cmd.Old == plumbing.ZeroHash && !cmd.Delete {
				foundFeatureCreate = true
			}
		}
	}
	if !foundFeatureCreate {
		t.Fatalf("expected ref-create command for feature at %s; got %v", featureHash, pushCommandsBatches)
	}
}

type fakeBootstrapSource struct {
	fetchPack          func(context.Context, gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
	fetchCommitParents func(context.Context, gitproto.Conn, gitproto.DesiredRef, []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error)
}

func (f fakeBootstrapSource) FetchPack(
	ctx context.Context,
	conn gitproto.Conn,
	desired map[plumbing.ReferenceName]gitproto.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	return f.fetchPack(ctx, conn, desired, targetRefs)
}

func (f fakeBootstrapSource) FetchCommitParents(
	ctx context.Context,
	conn gitproto.Conn,
	ref gitproto.DesiredRef,
	haves []plumbing.Hash,
) (map[plumbing.Hash][]plumbing.Hash, error) {
	if f.fetchCommitParents != nil {
		return f.fetchCommitParents(ctx, conn, ref, haves)
	}
	return map[plumbing.Hash][]plumbing.Hash{}, nil
}

func (fakeBootstrapSource) SupportsBootstrapBatch() bool { return true }

// parentsFromCommitChainStore walks a store and returns the
// (commit -> parents) map equivalent of what FetchCommitParents would
// have produced for the commits in that store. Used by tests so they
// can build commit graphs via the existing writeLinearCommitChain
// helper and then expose them through the parents-map mock.
func parentsFromCommitChainStore(tb testing.TB, store storer.Storer) map[plumbing.Hash][]plumbing.Hash {
	tb.Helper()
	iter, err := store.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		tb.Fatalf("iter: %v", err)
	}
	defer iter.Close()
	out := map[plumbing.Hash][]plumbing.Hash{}
	if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
		commit := &object.Commit{}
		if err := commit.Decode(obj); err != nil {
			return fmt.Errorf("decode commit %s: %w", obj.Hash(), err)
		}
		out[obj.Hash()] = append([]plumbing.Hash(nil), commit.ParentHashes...)
		return nil
	}); err != nil {
		tb.Fatalf("iter foreach: %v", err)
	}
	return out
}

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
			fetchPack: func(_ context.Context, _ gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, targetRefs map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				gotDesired = desired
				// Target refs ride along as haves so a resume-marker route
				// landing on the one-shot path only transfers the remainder;
				// on the designed empty-target path the map is empty anyway.
				if len(targetRefs) != 0 {
					t.Fatalf("expected empty target-ref haves during empty-target one-shot fetch, got %v", targetRefs)
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
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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

// noBatchSource is a source that can't serve the protocol-v2 fetch filter
// batched bootstrap needs, so a one-shot push failure has no batched fallback.
type noBatchSource struct{ fakeBootstrapSource }

func (noBatchSource) SupportsBootstrapBatch() bool { return false }

func TestAutoTargetMaxPackBytesTimeoutTriggersBatching(t *testing.T) {
	limit, ok := autoTargetMaxPackBytes(
		Params{SourceService: fakeBootstrapSource{}},
		errors.New("target receive-pack: http 408: request timeout"),
	)
	if !ok {
		t.Fatal("autoTargetMaxPackBytes(408) = not ok, want batched fallback")
	}
	if limit != defaultTargetMaxPackBytes {
		t.Fatalf("limit = %d, want default %d", limit, int64(defaultTargetMaxPackBytes))
	}
}

func TestExecuteOneShotTimeoutWithoutBatchSupportIsActionable(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	mainHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	pushErr := errors.New("target receive-pack: post RPC stream body: http 408: request timeout")

	_, err := Execute(context.Background(), Params{
		SourceService: noBatchSource{fakeBootstrapSource{
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		}},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				_ = pack.Close()
				return pushErr
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {SourceRef: mainRef, TargetRef: mainRef, SourceHash: mainHash, Kind: planner.RefKindBranch},
		},
	}, "empty target")
	if err == nil {
		t.Fatal("Execute() error = nil, want actionable timeout error")
	}
	if !errors.Is(err, pushErr) {
		t.Fatalf("Execute() error does not wrap original push error: %v", err)
	}
	if !strings.Contains(err.Error(), "protocol-v2 fetch filter") {
		t.Fatalf("Execute() error missing batched-bootstrap guidance: %v", err)
	}
}

func TestExecuteBatchedClosesCheckpointPackOnPushError(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	hashes := makeLinearCommitChain(t, 1)
	pack := &trackingReadCloser{Reader: bytes.NewReader([]byte("PACK"))}

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				store := memory.NewStorage()
				writeLinearCommitChain(t, store, 1)
				return parentsFromCommitChainStore(t, store), nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				store := memory.NewStorage()
				writeLinearCommitChain(t, store, 1)
				return parentsFromCommitChainStore(t, store), nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
					fetchPack: func(context.Context, gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests++
		t.Fatalf("unexpected preflight request: %s %s", r.Method, r.URL.String())
	}))
	defer server.Close()

	prevBaseURL := GitHubRepoAPIBaseURL
	GitHubRepoAPIBaseURL = server.URL
	defer func() { GitHubRepoAPIBaseURL = prevBaseURL }()

	ep, err := transport.ParseURL("https://github.com/acme/repo.git")
	if err != nil {
		t.Fatalf("transport.ParseURL: %v", err)
	}

	_, err = Execute(context.Background(), Params{
		SourceConn: &gitproto.HTTPConn{
			EndpointURL: ep,
			HTTP:        server.Client(),
		},
		SourceService: fakeBootstrapSource{
			fetchPack: func(context.Context, gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				t.Fatal("unexpected fetch")
				return nil, nil //nolint:nilnil // test fake returns nil to signal no data
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
	for i := range count {
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

// forkedCommitGraph builds a commit graph with two divergent tips sharing a
// root: a linear trunk, plus one commit hanging off the trunk's second commit.
// A second branch whose tip is IN the trunk's ancestry is planned as subsumed
// and never fetches, so a test about later branches' fetch haves needs a fork.
func forkedCommitGraph(tb testing.TB, trunkLen int) (parents map[plumbing.Hash][]plumbing.Hash, trunkTip, forkTip plumbing.Hash) {
	tb.Helper()
	store := memory.NewStorage()
	trunk := writeLinearCommitChain(tb, store, trunkLen)
	obj := store.NewEncodedObject()
	commit := &object.Commit{
		Author:       object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(9000, 0).UTC()},
		Committer:    object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(9000, 0).UTC()},
		Message:      "fork",
		TreeHash:     plumbing.ZeroHash,
		ParentHashes: []plumbing.Hash{trunk[1]},
	}
	if err := commit.Encode(obj); err != nil {
		tb.Fatalf("encode fork commit: %v", err)
	}
	forkTip, err := store.SetEncodedObject(obj)
	if err != nil {
		tb.Fatalf("store fork commit: %v", err)
	}
	return parentsFromCommitChainStore(tb, store), trunk[trunkLen-1], forkTip
}

// reachableParents narrows a commit-parents map to the commits reachable from
// tip.
func reachableParents(parents map[plumbing.Hash][]plumbing.Hash, tip plumbing.Hash) map[plumbing.Hash][]plumbing.Hash {
	out := map[plumbing.Hash][]plumbing.Hash{}
	queue := []plumbing.Hash{tip}
	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]
		if _, seen := out[hash]; seen {
			continue
		}
		out[hash] = parents[hash]
		queue = append(queue, parents[hash]...)
	}
	return out
}

// batchedRefusalHarness drives a two-branch batched bootstrap whose target
// answers with the given per-ref "ng" statuses, and records what the run
// fetched and pushed.
type batchedRefusalHarness struct {
	params              Params
	trunkRef, forkRef   plumbing.ReferenceName
	trunkTempRef        plumbing.ReferenceName
	trunkTip            plumbing.Hash
	fetchHaves          []map[plumbing.ReferenceName]plumbing.Hash
	pushCommandsBatches [][]gitproto.PushCommand
	// refsNow is what the target answers when the run asks which refs it
	// actually has, and refsNowErr makes that question unanswerable. Together
	// they stand in for the only authority on whether a branch landed.
	refsNow    map[plumbing.ReferenceName]plumbing.Hash
	refsNowErr error
	refsNowN   int
	// silent makes the target report nothing per ref, as one that never
	// negotiated report-status does.
	silent bool
}

func newBatchedRefusalHarness(t *testing.T, refused map[plumbing.ReferenceName]string) *batchedRefusalHarness {
	t.Helper()
	return newBatchedHarness(t, refused, false)
}

// newSubsumedRefusalHarness is the same two branches with the second one's tip
// inside the trunk's ancestry, so it is planned as subsumed: one ref create, no
// pack, no temp ref.
func newSubsumedRefusalHarness(t *testing.T, refused map[plumbing.ReferenceName]string) *batchedRefusalHarness {
	t.Helper()
	return newBatchedHarness(t, refused, true)
}

func newBatchedHarness(t *testing.T, refused map[plumbing.ReferenceName]string, subsumeSecond bool) *batchedRefusalHarness {
	t.Helper()
	parents, trunkTip, forkTip := forkedCommitGraph(t, 4)
	if subsumeSecond {
		// A tip on the trunk's own chain: reachable from it, so trunk's
		// batches deliver every object and the planner subsumes it.
		for hash, ps := range parents {
			if hash == trunkTip {
				forkTip = ps[0]
			}
		}
	}
	h := &batchedRefusalHarness{
		trunkRef: plumbing.NewBranchReferenceName("main"),
		forkRef:  plumbing.NewBranchReferenceName("release"),
		trunkTip: trunkTip,
		refsNow:  map[plumbing.ReferenceName]plumbing.Hash{},
	}
	h.trunkTempRef = planner.BootstrapTempRef(h.trunkRef)
	h.params = Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, ref gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				// Only the requested ref's ancestry, as a real commit-graph
				// fetch answers: handing back the whole graph would put the
				// fork tip in trunk's stop set and plan it as subsumed.
				return reachableParents(parents, ref.SourceHash), nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, haves map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				h.fetchHaves = append(h.fetchHaves, planner.CopyRefHashMap(haves))
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				_ = pack.Close()
				return nil
			},
			pushCommands: func(_ context.Context, cmds []gitproto.PushCommand) error {
				h.pushCommandsBatches = append(h.pushCommandsBatches, append([]gitproto.PushCommand(nil), cmds...))
				return nil
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			h.trunkRef: {SourceRef: h.trunkRef, TargetRef: h.trunkRef, SourceHash: trunkTip, Kind: planner.RefKindBranch, Label: "main"},
			h.forkRef:  {SourceRef: h.forkRef, TargetRef: h.forkRef, SourceHash: forkTip, Kind: planner.RefKindBranch, Label: "release"},
		},
		TargetRefs:       map[plumbing.ReferenceName]plumbing.Hash{},
		SourceHeadTarget: h.trunkRef,
		TargetMaxPack:    1024 * 1024,
		RefOutcome: func(name plumbing.ReferenceName) (gitproto.RefOutcome, string) {
			if reason, ok := refused[name]; ok {
				return gitproto.RefOutcomeRefused, reason
			}
			if h.silent {
				return gitproto.RefOutcomeUnknown, ""
			}
			return gitproto.RefOutcomeApplied, ""
		},
		TargetRefsNow: func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
			h.refsNowN++
			if h.refsNowErr != nil {
				return nil, h.refsNowErr
			}
			return h.refsNow, nil
		},
	}
	return h
}

// deletedTrunkMarker reports whether the run pushed a delete for the trunk
// branch's resume marker.
func (h *batchedRefusalHarness) deletedTrunkMarker() bool {
	for _, cmds := range h.pushCommandsBatches {
		for _, cmd := range cmds {
			if cmd.Name == h.trunkTempRef && cmd.Delete {
				return true
			}
		}
	}
	return false
}

// A refused branch create must leave the marker in place AND leave its hash
// usable as a fetch have for the branches planned after it — the reason the
// cutover records the temp ref instead of the branch it could not create.
func TestExecuteBatchedRefusedCreateKeepsMarkerAsHave(t *testing.T) {
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{
		plumbing.NewBranchReferenceName("main"): "deny creating a protected branch",
	})

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if h.deletedTrunkMarker() {
		t.Errorf("deleted resume marker %s after a create the target refused: %v",
			h.trunkTempRef, h.pushCommandsBatches)
	}
	// Kept because the target says the branch is absent, not because the ng
	// text was read: the listing has to have been consulted.
	if h.refsNowN == 0 {
		t.Error("marker decided without asking the target which refs it has")
	}
	if len(h.fetchHaves) < 2 {
		t.Fatalf("expected a fetch for the second branch, got %d fetch(es)", len(h.fetchHaves))
	}
	// The second branch's fetch must offer the kept marker's hash, or the
	// objects trunk already delivered are re-sent.
	var offeredTrunkTip bool
	for _, haves := range h.fetchHaves[1:] {
		for _, hash := range haves {
			if hash == h.trunkTip {
				offeredTrunkTip = true
			}
		}
	}
	if !offeredTrunkTip {
		t.Errorf("later fetch did not offer the kept marker at %s as a have: %v",
			planner.ShortHash(h.trunkTip), h.fetchHaves)
	}
}

// The common path pays nothing: a confirmed create deletes its marker straight
// away, without asking the target for a ref listing.
func TestExecuteBatchedConfirmedCreateDeletesMarkerWithoutListing(t *testing.T) {
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{})

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !h.deletedTrunkMarker() {
		t.Errorf("did not delete resume marker %s after a confirmed create: %v",
			h.trunkTempRef, h.pushCommandsBatches)
	}
	if h.refsNowN != 0 {
		t.Errorf("asked the target for %d ref listing(s) on a run with nothing in doubt", h.refsNowN)
	}
}

// A refusal does not always mean the branch is absent — "already exists" says
// the opposite, and a pre-receive message can contain that phrase while the
// branch really is missing. So the decision is made on what the target has,
// not on what it wrote: branch present means the marker is stale scaffolding
// and must go, or it strands forever on the bootstrap route, which refuses
// --prune and so has no cleaner.
func TestExecuteBatchedRefusedButPresentBranchDeletesMarker(t *testing.T) {
	trunkRef := plumbing.NewBranchReferenceName("main")
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{
		trunkRef: "already exists",
	})
	// There, at the hash this run pushed — so the import did land and the
	// scaffolding is genuinely spent.
	h.refsNow[trunkRef] = h.trunkTip

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !h.deletedTrunkMarker() {
		t.Errorf("kept resume marker %s for a branch the target has: %v",
			h.trunkTempRef, h.pushCommandsBatches)
	}
}

// The mirror image, and the reason the text is not consulted: a refusal whose
// wording happens to contain "already exists" while the branch is genuinely
// absent must keep the marker. Classifying the prose would delete it and cost
// a full re-transfer.
func TestExecuteBatchedRefusalMentioningExistenceKeepsMarkerWhenAbsent(t *testing.T) {
	trunkRef := plumbing.NewBranchReferenceName("main")
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{
		trunkRef: "refusing to create refs/heads/main: a tag with that name already exists",
	})

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if h.deletedTrunkMarker() {
		t.Errorf("deleted resume marker %s for a branch the target does not have: %v",
			h.trunkTempRef, h.pushCommandsBatches)
	}
}

// Silence is not confirmation on the subsumed path either. Nothing is at stake
// but the count, so no listing is fetched for it — the create simply is not
// reported as delivered. Trunk's own checkpoint pack still counts, which is
// what separates the two numbers below.
func TestExecuteBatchedSubsumedUnconfirmedCreateNotCounted(t *testing.T) {
	confirmed := newSubsumedRefusalHarness(t, map[plumbing.ReferenceName]string{})
	baseline, err := Execute(context.Background(), confirmed.params, "empty target")
	if err != nil {
		t.Fatalf("Execute (confirmed): %v", err)
	}

	silent := newSubsumedRefusalHarness(t, map[plumbing.ReferenceName]string{})
	silent.silent = true
	result, err := Execute(context.Background(), silent.params, "empty target")
	if err != nil {
		t.Fatalf("Execute (silent target): %v", err)
	}
	if result.BatchCount != baseline.BatchCount-1 {
		t.Errorf("BatchCount=%d against a silent target, want %d — one less than the %d a confirmed run reports, "+
			"since the subsumed create is the only difference",
			result.BatchCount, baseline.BatchCount-1, baseline.BatchCount)
	}
}

// A branch present at someone else's commit is not this import's branch. The
// span between their tip and ours is reachable from the marker and nothing
// else, so deleting it would strand exactly the objects this run delivered.
func TestExecuteBatchedBranchAtOtherHashKeepsMarker(t *testing.T) {
	trunkRef := plumbing.NewBranchReferenceName("main")
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{
		trunkRef: "already exists",
	})
	h.refsNow[trunkRef] = plumbing.NewHash("6dcf09a3e2a1b3d1d1c88f1ad5e63e3f3d1a2b3c")

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if h.deletedTrunkMarker() {
		t.Errorf("deleted resume marker %s though %s sits at another commit: %v",
			h.trunkTempRef, trunkRef, h.pushCommandsBatches)
	}
}

// A target that refuses the temp ref itself has applied nothing: the run must
// stop rather than advance its checkpoint state against a hash the target
// never accepted.
func TestExecuteBatchedRefusedTempRefStopsTheRun(t *testing.T) {
	trunkTempRef := planner.BootstrapTempRef(plumbing.NewBranchReferenceName("main"))
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{
		plumbing.NewBranchReferenceName("main"): "deny updating a hidden ref",
		trunkTempRef:                            "deny updating a hidden ref",
	})

	_, err := Execute(context.Background(), h.params, "empty target")
	if err == nil {
		t.Fatal("Execute succeeded against a target that refused the temp ref")
	}
	if !strings.Contains(err.Error(), trunkTempRef.String()) {
		t.Errorf("error does not name the refused temp ref: %v", err)
	}
	if h.deletedTrunkMarker() {
		t.Errorf("deleted a marker the target never accepted: %v", h.pushCommandsBatches)
	}
}

// Silence from a target that never advertised report-status is not
// confirmation — but it is not a failure either. The listing settles it: the
// create landed, so the marker is stale and goes. Without this the marker
// would be permanent on the one route that forbids --prune.
func TestExecuteBatchedUnreportingTargetSettledByListing(t *testing.T) {
	trunkRef := plumbing.NewBranchReferenceName("main")
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{})
	h.silent = true
	h.refsNow[trunkRef] = h.trunkTip

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if h.refsNowN == 0 {
		t.Error("never asked the target which refs it has, so nothing was confirmed")
	}
	if !h.deletedTrunkMarker() {
		t.Errorf("kept resume marker %s though the target has the branch: %v",
			h.trunkTempRef, h.pushCommandsBatches)
	}
}

// When the target cannot be asked either, nothing is settled and everything is
// kept — one run's leftover scaffolding against a full re-transfer — and the
// run still succeeds, because the branches that landed are what the operator
// asked for.
func TestExecuteBatchedUnansweredListingKeepsMarker(t *testing.T) {
	h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{})
	h.silent = true
	h.refsNowErr = errors.New("target listing unavailable")

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute must not fail because a listing failed: %v", err)
	}
	if h.deletedTrunkMarker() {
		t.Errorf("deleted resume marker %s without confirming anything: %v",
			h.trunkTempRef, h.pushCommandsBatches)
	}
}

// A subsumed branch has no temp ref to lose, but its create is a lone ref push
// with the same nil-error-is-not-proof problem: a refused create must not be
// counted as a delivered batch or offered as a have.
func TestExecuteBatchedSubsumedRefusedCreateNotCounted(t *testing.T) {
	forkRef := plumbing.NewBranchReferenceName("release")
	h := newSubsumedRefusalHarness(t, map[plumbing.ReferenceName]string{
		forkRef: "deny creating a protected branch",
	})

	result, err := Execute(context.Background(), h.params, "empty target")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Trunk's checkpoint only. Counting the refused subsumed create would
	// report work the target never accepted.
	if result.BatchCount != 1 {
		t.Errorf("BatchCount=%d, want 1 (trunk only; the subsumed create was refused)", result.BatchCount)
	}
	for _, haves := range h.fetchHaves {
		if _, ok := haves[forkRef]; ok {
			t.Errorf("offered refused branch %s as a fetch have: %v", forkRef, haves)
		}
	}
}

// makePackHeader builds a minimal valid PACK header: "PACK" + version 2 +
// object count. Fixtures must use it rather than arbitrary bytes — a bogus
// version field is read as the object count by checkPackSizeAndSubdivide and
// leaves the observer's TotalObjects at 0, which silently closes the
// projection paths a test may believe it is exercising.
func makePackHeader(objectCount uint32) []byte {
	var h [12]byte
	copy(h[:4], "PACK")
	h[4], h[5], h[6], h[7] = 0, 0, 0, 2 // version 2
	h[8] = byte(objectCount >> 24)
	h[9] = byte(objectCount >> 16)
	h[10] = byte(objectCount >> 8)
	h[11] = byte(objectCount)
	return h[:]
}

// bottomOutParams builds a batched bootstrap over a one-commit chain, so the
// checkpoint is indivisible from the first push and the bottom-out path is
// exercised directly.
//
// The commit chain is written ONCE and both DesiredRefs.SourceHash and the
// parent map derive from it: building two stores and relying on the helper
// being deterministic makes an unrelated change surface here as a confusing
// "checkpoint not in chain".
func bottomOutParams(t *testing.T, budget, announced int64, push func(int, io.ReadCloser) error) Params {
	t.Helper()
	mainRef := plumbing.NewBranchReferenceName("main")
	store := memory.NewStorage()
	hashes := writeLinearCommitChain(t, store, 1)
	parents := parentsFromCommitChainStore(t, store)
	tip := hashes[len(hashes)-1]
	// A real header: the observer parses TotalObjects from it, which is what
	// opens the projection used to size the next subdivision. With a bogus
	// version the count reads as 0 and that path is silently skipped.
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...)
	pushes := 0

	return Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			// Fresh reader per attempt: a retry re-fetches.
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushes++
				return push(pushes, pack)
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef: mainRef, TargetRef: mainRef,
				SourceHash: tip, Kind: planner.RefKindBranch, Label: "main",
			},
		},
		TargetRefs:           map[plumbing.ReferenceName]plumbing.Hash{},
		TargetMaxPack:        budget,
		AnnouncedTargetLimit: announced,
	}
}

// drainAbort drains the observer so its counter advances and the aborter can
// fire; the abort surfaces as the read error.
func drainAbort(_ int, pack io.ReadCloser) error {
	_, err := io.Copy(io.Discard, pack)
	return err
}

func TestExecuteBatchedSelfImposedAbortIsNotPermanent(t *testing.T) {
	// Our own budget stopped the upload and the target never stated a limit,
	// so we have no evidence it would refuse the pack. That must stay
	// retryable — permafailing here would strand a repo a larger budget or a
	// config change could still mirror.
	pushes := 0
	_, err := Execute(context.Background(), bottomOutParams(t, 64, 0, func(n int, p io.ReadCloser) error {
		pushes = n
		return drainAbort(n, p)
	}), "empty target")
	if err == nil {
		t.Fatal("expected the push to fail")
	}
	if errors.Is(err, ErrCheckpointExceedsTargetLimit) {
		t.Fatalf("self-imposed abort must not be classified permanent: %v", err)
	}
	if pushes != 1 {
		t.Fatalf("expected no relaxed retry without an announced limit, got %d pushes", pushes)
	}
}

func TestExecuteBatchedPushesIndivisibleCheckpointAtAnnouncedLimit(t *testing.T) {
	// The point of the change: a one-commit checkpoint cannot be split, so the
	// only meaningful ceiling is the target's own. It is pushed at that ceiling
	// directly and converges — in ONE push, with no doomed attempt against our
	// smaller budget and no second source fetch.
	pushes := 0
	result, err := Execute(context.Background(), bottomOutParams(t, 64, 1<<20, func(n int, p io.ReadCloser) error {
		pushes = n
		return drainAbort(n, p)
	}), "empty target")
	if err != nil {
		t.Fatalf("expected the run to converge at the announced ceiling, got %v", err)
	}
	if pushes != 1 {
		t.Fatalf("expected a single push at the announced ceiling, got %d", pushes)
	}
	if !result.Batching {
		t.Fatalf("expected a batched result, got %+v", result)
	}
}

func TestExecuteBatchedPermanentOnceTargetsOwnLimitIsExceeded(t *testing.T) {
	// Pushed at the announced limit and still over it: the verdict is the
	// target's, and the checkpoint cannot be split. Terminal — and reached in
	// one push rather than after a doomed smaller attempt.
	pushes := 0
	_, err := Execute(context.Background(), bottomOutParams(t, 64, 256, func(n int, p io.ReadCloser) error {
		pushes = n
		return drainAbort(n, p)
	}), "empty target")
	if err == nil {
		t.Fatal("expected the push to fail")
	}
	if !errors.Is(err, ErrCheckpointExceedsTargetLimit) {
		t.Fatalf("expected ErrCheckpointExceedsTargetLimit, got %v", err)
	}
	if pushes != 1 {
		t.Fatalf("expected a single push at the announced ceiling, got %d", pushes)
	}
}

func TestExecuteBatchedUnrelatedErrorStaysRetryable(t *testing.T) {
	// An indivisible checkpoint that fails for a reason that has nothing to do
	// with size must NOT be permanent: the worker would stop redelivering a
	// repo that needed one retry. This is the direction the whole change turns
	// on, so it is asserted for each shape of ordinary failure.
	for _, msg := range []string{
		"http 401 unauthorized",
		"http 500 internal server error",
		"connection reset by peer",
		"pre-receive hook declined",
	} {
		t.Run(msg, func(t *testing.T) {
			_, err := Execute(context.Background(), bottomOutParams(t, 64, 1<<20, func(int, io.ReadCloser) error {
				return errors.New(msg)
			}), "empty target")
			if err == nil {
				t.Fatal("expected the push to fail")
			}
			if errors.Is(err, ErrCheckpointExceedsTargetLimit) {
				t.Fatalf("unrelated failure must stay retryable, got permanent: %v", err)
			}
		})
	}
}

func TestExecuteBatchedHardRejectionIsPermanentWithoutSelfImposedAbort(t *testing.T) {
	// The target rejected the indivisible pack outright, with no prior
	// self-imposed abort. That is already the target's verdict, so it must be
	// permanent immediately rather than redelivering forever — the failure
	// mode this change exists to end.
	pushes := 0
	_, err := Execute(context.Background(), bottomOutParams(t, 1<<30, 0, func(n int, _ io.ReadCloser) error {
		pushes = n
		return errors.New("http 413: body exceeded size limit 1000")
	}), "empty target")
	if err == nil {
		t.Fatal("expected the push to fail")
	}
	if !errors.Is(err, ErrCheckpointExceedsTargetLimit) {
		t.Fatalf("a hard body-limit rejection on an indivisible checkpoint must be permanent, got %v", err)
	}
	if pushes != 1 {
		t.Fatalf("expected no retry for a server rejection, got %d pushes", pushes)
	}
}

func TestExecuteBatchedDeadlineOnIndivisibleCheckpointStaysRetryable(t *testing.T) {
	// A gateway timeout is availability, not size — with no bytes sent there is
	// no size evidence at all. Classifying it permanent would stop redelivery
	// for a repo that one calm retry would mirror, and a target rolling restart
	// would permafail every large bootstrap in flight.
	for _, msg := range []string{"http 504 gateway timeout", "http 408 request timeout"} {
		t.Run(msg, func(t *testing.T) {
			_, err := Execute(context.Background(), bottomOutParams(t, 64, 1<<20, func(int, io.ReadCloser) error {
				return errors.New(msg)
			}), "empty target")
			if err == nil {
				t.Fatal("expected the push to fail")
			}
			if errors.Is(err, ErrCheckpointExceedsTargetLimit) {
				t.Fatalf("a deadline is not a size verdict; must stay retryable: %v", err)
			}
		})
	}
}

func TestExecuteBatchedNoSafetyMarginAtAnnouncedLimit(t *testing.T) {
	// A pack inside the top 5% of the announced limit is one the target would
	// accept. A push at that ceiling must therefore carry no safety margin and
	// no projection: cutting early would invent a rejection the target never
	// issued and then report it as permanent.
	const announced = 4200
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...) // 4108 bytes: 97.8% of announced
	if len(body) <= announced*95/100 || len(body) > announced {
		t.Fatalf("fixture must sit between 95%% and 100%% of the announced limit, got %d", len(body))
	}

	mainRef := plumbing.NewBranchReferenceName("main")
	store := memory.NewStorage()
	hashes := writeLinearCommitChain(t, store, 1)
	parents := parentsFromCommitChainStore(t, store)
	pushes := 0

	result, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushes++
				_, err := io.Copy(io.Discard, pack)
				return err
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef: mainRef, TargetRef: mainRef,
				SourceHash: hashes[len(hashes)-1], Kind: planner.RefKindBranch, Label: "main",
			},
		},
		TargetRefs:    map[plumbing.ReferenceName]plumbing.Hash{},
		TargetMaxPack: 64, // forces the first attempt to abort on our own budget
		// The target says it accepts more than this pack needs.
		AnnouncedTargetLimit: announced,
	}, "empty target")

	if err != nil {
		t.Fatalf("a pack under the announced limit must converge, got %v", err)
	}
	if pushes != 1 {
		t.Fatalf("expected a single push at the announced ceiling, got %d", pushes)
	}
	if !result.Batching {
		t.Fatalf("expected a batched result, got %+v", result)
	}
}

func TestExecuteOneShotRejectionCapturesAnnouncedLimitForRelaxedRetry(t *testing.T) {
	// Production never sets AnnouncedTargetLimit — it is only ever learned by
	// parsing a rejection, so this capture and the in-loop one below are the
	// feature's ONLY real sources. Without coverage either could be deleted and
	// the whole mechanism would go inert with a green suite.
	const announced = 8400
	mainRef := plumbing.NewBranchReferenceName("main")
	store := memory.NewStorage()
	hashes := writeLinearCommitChain(t, store, 1)
	parents := parentsFromCommitChainStore(t, store)
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...)
	pushes := 0

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushes++
				if pushes == 1 {
					// The one-shot attempt: rejected, announcing the real limit.
					return fmt.Errorf("http 413: body exceeded size limit %d", announced)
				}
				_, err := io.Copy(io.Discard, pack)
				return err
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef: mainRef, TargetRef: mainRef,
				SourceHash: hashes[len(hashes)-1], Kind: planner.RefKindBranch, Label: "main",
			},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{},
		// No TargetMaxPack and no AnnouncedTargetLimit: both must be derived
		// from the rejection, exactly as production does it.
	}, "empty target")

	if err != nil {
		t.Fatalf("expected the run to converge via the captured limit, got %v", err)
	}
	// One-shot rejection, then a single batched push at the captured limit.
	if pushes != 2 {
		t.Fatalf("expected one-shot rejection then one push at the captured limit, got %d", pushes)
	}
}

func TestExecuteBatchedInLoopRejectionCapturesAnnouncedLimit(t *testing.T) {
	// The second capture site: a run that enters batching directly (an explicit
	// --target-max-pack-bytes, or the GitHub large-repo preflight) never makes a
	// one-shot attempt, so the only place it can learn the target's limit is a
	// rejection inside the batch loop.
	const announced = 8400
	mainRef := plumbing.NewBranchReferenceName("main")
	store := memory.NewStorage()
	hashes := writeLinearCommitChain(t, store, 3)
	parents := parentsFromCommitChainStore(t, store)
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...)
	pushes := 0

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushes++
				if pushes == 1 {
					// A divisible span is rejected, announcing the real limit:
					// this subdivides AND must record the limit for later.
					return fmt.Errorf("http 413: body exceeded size limit %d", announced)
				}
				_, err := io.Copy(io.Discard, pack)
				return err
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef: mainRef, TargetRef: mainRef,
				SourceHash: hashes[len(hashes)-1], Kind: planner.RefKindBranch, Label: "main",
			},
		},
		TargetRefs:    map[plumbing.ReferenceName]plumbing.Hash{},
		TargetMaxPack: 2000, // enters batching directly; no one-shot attempt
		// AnnouncedTargetLimit deliberately unset.
	}, "empty target")

	if err != nil {
		t.Fatalf("expected the in-loop captured limit to carry the run, got %v", err)
	}
	if pushes < 2 {
		t.Fatalf("expected pushes after the rejection, got %d", pushes)
	}
}

func TestExecuteBatchedReportsBatchingWhenCheckpointPlanningFails(t *testing.T) {
	// Checkpoint planning fetches the commit graph — the likeliest failure for
	// exactly the large repos that batch. The route facts must already be set,
	// or such a failure reports itself as a one-shot bootstrap and the whole
	// point of carrying them on the error path is lost.
	mainRef := plumbing.NewBranchReferenceName("main")
	hashes := makeLinearCommitChain(t, 1)

	result, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return nil, errors.New("commit graph fetch exploded")
			},
		},
		TargetPusher: fakeBootstrapPusher{},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef: mainRef, TargetRef: mainRef,
				SourceHash: hashes[len(hashes)-1], Kind: planner.RefKindBranch, Label: "main",
			},
		},
		TargetRefs:    map[plumbing.ReferenceName]plumbing.Hash{},
		TargetMaxPack: 1000,
	}, "empty target")

	if err == nil {
		t.Fatal("expected checkpoint planning to fail")
	}
	if !result.Batching || result.RelayMode != "bootstrap-batch" {
		t.Fatalf("a failed batched bootstrap must not report as one-shot: batching=%t relayMode=%q",
			result.Batching, result.RelayMode)
	}
}

// twoCommitFixture builds a chain of n commits plus the desired-ref map for a
// single branch over it, sharing one store so the tip and the parent map cannot
// disagree.
func twoCommitFixture(t *testing.T, n int) (map[plumbing.Hash][]plumbing.Hash, map[plumbing.ReferenceName]planner.DesiredRef) {
	t.Helper()
	mainRef := plumbing.NewBranchReferenceName("main")
	store := memory.NewStorage()
	hashes := writeLinearCommitChain(t, store, n)
	return parentsFromCommitChainStore(t, store), map[plumbing.ReferenceName]planner.DesiredRef{
		mainRef: {
			SourceRef: mainRef, TargetRef: mainRef,
			SourceHash: hashes[len(hashes)-1], Kind: planner.RefKindBranch, Label: "main",
		},
	}
}

func TestExecuteBatchedDivisibleCheckpointKeepsTheSmallBudget(t *testing.T) {
	// The central gate: only a checkpoint that CANNOT be split is pushed at the
	// target's ceiling. A divisible span must still abort at the small budget —
	// that is what bounds the waste of a doomed push and keeps the temp ref
	// advancing, and it is the entire justification for TargetMaxPack existing.
	// Without this, escalating unconditionally would look identical in every
	// other test.
	parents, desired := twoCommitFixture(t, 4)
	// Big enough to trip the small budget's 95% cut, small enough to fit the
	// announced limit — so escalating a divisible span would visibly let it
	// through, and the abort below is the only reason it does not.
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 300_000)...)
	var aborts, ceilings int

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				if _, err := io.Copy(io.Discard, pack); err != nil {
					aborts++
					return err
				}
				return nil
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: desired,
		TargetRefs:  map[plumbing.ReferenceName]plumbing.Hash{},
		// Large enough that planning yields ONE multi-commit (divisible)
		// checkpoint, while the pack still exceeds its 95% cut.
		TargetMaxPack: 262_144,
		// Comfortably above the pack, so escalating would let it through.
		AnnouncedTargetLimit: 1 << 20,
		OnNotice: func(msg string) {
			if strings.Contains(msg, "announced limit") {
				ceilings++
			}
		},
	}, "empty target")
	_ = err

	if aborts == 0 {
		t.Fatal("expected the divisible span to abort at the small budget; it never did")
	}
	if ceilings > 0 && aborts == 0 {
		t.Fatal("escalated before exhausting subdivision")
	}
}

func TestExecuteBatchedEscalatesWhenBudgetEqualsAnnouncedLimit(t *testing.T) {
	// The >= boundary. An in-batching rejection ratchets the budget down to the
	// announced limit, leaving the two EQUAL. Escalating still matters there:
	// it sheds the 95% margin and the projection. With a strict > this pack —
	// sized inside that last 5% — aborts on every delivery forever.
	const announced = 4200
	parents, desired := twoCommitFixture(t, 2)
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...) // 4108 = 97.8% of announced
	if len(body) <= announced*95/100 || len(body) > announced {
		t.Fatalf("fixture must sit in the top 5%% of the announced limit, got %d", len(body))
	}
	pushes := 0

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushes++
				if pushes == 1 {
					// Announces the limit AND ratchets the budget to exactly it.
					return fmt.Errorf("http 413: body exceeded size limit %d", announced)
				}
				_, err := io.Copy(io.Discard, pack)
				return err
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs:   desired,
		TargetRefs:    map[plumbing.ReferenceName]plumbing.Hash{},
		TargetMaxPack: 1 << 30, // enters batching; the 413 ratchets it to announced
	}, "empty target")

	if err != nil {
		t.Fatalf("a pack inside the top 5%% of the announced limit must converge, got %v", err)
	}
}

func TestExecuteBatchedObservedCutoffBlocksAnnouncedEscalation(t *testing.T) {
	// A budget inferred from bytes we actually got to send is evidence of where
	// the server cuts, not a figure we chose, so an indivisible checkpoint must
	// NOT be escalated past it.
	//
	// Reaching this needs >= 2 commits: a divisible span to take the
	// observation, then an indivisible one to consult it. On a one-commit chain
	// the guard is unreachable — any failure that would set it ends the run on
	// that same checkpoint, so there is no later iteration to read the flag.
	parents, desired := twoCommitFixture(t, 2)
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...)
	pushes := 0

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushes++
				switch pushes {
				case 1:
					// One-shot: a parseable 413 announces 1 MiB and derives the
					// batching budget from it.
					return errors.New("http 413: body exceeded size limit 1048576")
				case 2:
					// A divisible span: bytes flow, then an UNPARSEABLE
					// body-limit cut. That is the only route that arms the
					// guard — it ratchets the budget to the bytes observed.
					if _, copyErr := io.Copy(io.Discard, pack); copyErr != nil {
						return copyErr
					}
					return errors.New("request body too large for target")
				default:
					_, copyErr := io.Copy(io.Discard, pack)
					return copyErr
				}
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: desired,
		TargetRefs:  map[plumbing.ReferenceName]plumbing.Hash{},
		// Both the budget and the announced limit are derived from the 413,
		// exactly as production does it.
	}, "empty target")

	if err == nil {
		t.Fatal("guard violated: an indivisible push escalated past a measured server cutoff")
	}
	if errors.Is(err, ErrCheckpointExceedsTargetLimit) {
		t.Fatalf("an abort against a measured cutoff is not the target's size verdict: %v", err)
	}
	if pushes != 3 {
		t.Fatalf("expected one-shot 413, observed cutoff, one capped attempt (3 pushes), got %d", pushes)
	}
}

func TestExecuteBatchedIndivisibleSpanIsNotRePushedWhileLaterGapsSplit(t *testing.T) {
	// subdivideToFactor splits EVERY remaining gap, so a splittable gap later
	// in the branch grows the checkpoint list even when the current span is
	// already one commit. Retrying on that basis re-fetches and re-pushes an
	// identical pack — a one-commit gap has no midpoint to gain — once per
	// later split. On the repos this path serves that is the same multi-GiB
	// upload several times in a single run.
	//
	// Layout: 5 commits planned into 4 batches gives gaps of 1,1,1,2, so the
	// first checkpoint is indivisible while the last gap can still split.
	parents, desired := twoCommitFixture(t, 5)
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...)
	pushes := 0

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			// A deadline: batchable (so the subdivide path is entered) but not a
			// size verdict, so classification is not terminal and the old code
			// fell through to the growth branch.
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, _ io.ReadCloser) error {
				pushes++
				return errors.New("http 504 gateway timeout")
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: desired,
		TargetRefs:  map[plumbing.ReferenceName]plumbing.Hash{},
		// ~5 commits x 64 KiB / 80 KiB => 4 planned batches.
		TargetMaxPack: 81_920,
	}, "empty target")

	if err == nil {
		t.Fatal("expected the push to fail")
	}
	if errors.Is(err, ErrCheckpointExceedsTargetLimit) {
		t.Fatalf("a deadline is not a size verdict: %v", err)
	}
	if pushes != 1 {
		t.Fatalf("an indivisible span must not be re-pushed while later gaps split; got %d pushes", pushes)
	}
}

func TestExecuteBatchedDeadlineDoesNotDisableEscalation(t *testing.T) {
	// A target that drains the body and then times out (GitHub's 408 shape)
	// told us about TIME, not size. Ratcheting the budget to those bytes is
	// right — smaller packs do finish inside the window — but recording it as a
	// measured size limit is not: budgetFromObservation gates escalation and is
	// cleared only by a later parseable 413, so a single deadline would disable
	// the feature for the rest of the run, on exactly the flaky multi-GiB
	// targets this path serves.
	//
	// Classification already treats a deadline as availability rather than
	// size; provenance has to agree.
	const announced = 1 << 20
	parents, desired := twoCommitFixture(t, 3)
	body := append(makePackHeader(1), bytes.Repeat([]byte("x"), 4096)...)
	pushes := 0
	var notices []string

	_, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				return parents, nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				pushes++
				switch pushes {
				case 1:
					// One-shot: announces the real limit and derives the budget.
					return fmt.Errorf("http 413: body exceeded size limit %d", announced)
				case 2:
					// Drain, then time out: bytes flowed, so the budget ratchets
					// to them — but this is not size evidence.
					if _, copyErr := io.Copy(io.Discard, pack); copyErr != nil {
						return copyErr
					}
					return errors.New("http 408 request timeout")
				default:
					_, copyErr := io.Copy(io.Discard, pack)
					return copyErr
				}
			},
			pushCommands: func(_ context.Context, _ []gitproto.PushCommand) error { return nil },
		},
		DesiredRefs: desired,
		TargetRefs:  map[plumbing.ReferenceName]plumbing.Hash{},
		OnNotice:    func(msg string) { notices = append(notices, msg) },
	}, "empty target")

	if err != nil {
		t.Fatalf("a deadline must not disable escalation for the rest of the run, got %v", err)
	}
	var escalated bool
	for _, n := range notices {
		if strings.Contains(n, "announced limit") {
			escalated = true
		}
	}
	if !escalated {
		t.Fatalf("expected an indivisible span to still escalate after a deadline; notices=%v", notices)
	}
}
