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
			fetchCommitGraph: func(_ context.Context, store storer.Storer, _ *gitproto.Conn, ref gitproto.DesiredRef, _ []plumbing.Hash) error {
				graphFetches++
				if ref.SourceRef != mainRef {
					t.Errorf("unexpected commit-graph fetch for %s; subsumed branch should have been skipped", ref.SourceRef)
				}
				writeLinearCommitChain(t, store, 3)
				return nil
			},
			fetchPack: func(_ context.Context, _ *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
	fetchPack        func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
	fetchCommitGraph func(context.Context, storer.Storer, *gitproto.Conn, gitproto.DesiredRef, []plumbing.Hash) error
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
	haves []plumbing.Hash,
) error {
	if f.fetchCommitGraph != nil {
		return f.fetchCommitGraph(ctx, store, conn, ref, haves)
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
			fetchCommitGraph: func(_ context.Context, store storer.Storer, _ *gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) error {
				writeLinearCommitChain(t, store, 1)
				return nil
			},
			fetchPack: func(_ context.Context, _ *gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
			fetchCommitGraph: func(_ context.Context, store storer.Storer, _ *gitproto.Conn, _ gitproto.DesiredRef, _ []plumbing.Hash) error {
				writeLinearCommitChain(t, store, 1)
				return nil
			},
			fetchPack: func(_ context.Context, _ *gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
		SourceConn: &gitproto.Conn{
			Endpoint: ep,
			HTTP:     server.Client(),
		},
		SourceService: fakeBootstrapSource{
			fetchPack: func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
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
