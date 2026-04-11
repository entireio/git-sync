package planner

import (
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// BootstrapBatch holds the checkpoint plan for a single branch during batched bootstrap.
type BootstrapBatch struct {
	Plan        BranchPlan
	TempRef     plumbing.ReferenceName
	ResumeHash  plumbing.Hash
	Checkpoints []plumbing.Hash
}

// FirstParentChain walks the first-parent chain from tip back to root,
// returning the chain in root-to-tip order.
func FirstParentChain(store storer.EncodedObjectStorer, tip plumbing.Hash) ([]plumbing.Hash, error) {
	commit, err := object.GetCommit(store, tip)
	if err != nil {
		return nil, err
	}
	chain := make([]plumbing.Hash, 0, 128)
	for {
		chain = append(chain, commit.Hash)
		if len(commit.ParentHashes) == 0 {
			break
		}
		commit, err = object.GetCommit(store, commit.ParentHashes[0])
		if err != nil {
			return nil, err
		}
	}
	// Reverse in-place to get root-to-tip order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// SampledCheckpointCandidates generates a set of candidate indices to probe,
// sorted from largest (preferred) to smallest.
func SampledCheckpointCandidates(lo, hi int, prevSpan int) []int {
	if lo > hi {
		return nil
	}
	set := map[int]struct{}{}
	add := func(idx int) {
		if idx < lo {
			idx = lo
		}
		if idx > hi {
			idx = hi
		}
		set[idx] = struct{}{}
	}

	projected := hi
	if prevSpan > 0 {
		projected = lo + prevSpan - 1
	}
	add(projected)

	const sampleCount = 4
	current := projected
	for i := 0; i < sampleCount-1; i++ {
		if current <= lo {
			add(lo)
			continue
		}
		distance := current - lo
		current = lo + distance/2
		add(current)
	}
	add(lo)

	candidates := make([]int, 0, len(set))
	for idx := range set {
		candidates = append(candidates, idx)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(candidates)))
	return candidates
}

// SampledCheckpointUnderLimit finds the largest checkpoint index that fits within
// the batch limit, using a sampling strategy to reduce probes (issue #14).
func SampledCheckpointUnderLimit(
	chain []plumbing.Hash,
	prevIdx int,
	prevSpan int,
	probe func(idx int) (tooLarge bool, err error),
) (int, error) {
	lo := prevIdx + 1
	hi := len(chain) - 1
	if lo > hi {
		return -1, nil
	}

	samples := SampledCheckpointCandidates(lo, hi, prevSpan)
	best := -1
	for _, idx := range samples {
		tooLarge, err := probe(idx)
		if err != nil {
			return -1, err
		}
		if tooLarge {
			continue
		}
		best = idx
		break
	}
	if best != -1 {
		return best, nil
	}

	if prevSpan > 1 {
		shrunk := prevSpan / 2
		if shrunk < 1 {
			shrunk = 1
		}
		idx := lo + shrunk - 1
		if idx > hi {
			idx = hi
		}
		if idx >= lo {
			tooLarge, err := probe(idx)
			if err != nil {
				return -1, err
			}
			if !tooLarge {
				return idx, nil
			}
		}
	}

	tooLarge, err := probe(lo)
	if err != nil {
		return -1, err
	}
	if tooLarge {
		return -1, nil
	}
	return lo, nil
}

// BootstrapResumeIndex finds the starting index in a checkpoint list given a resume hash.
func BootstrapResumeIndex(checkpoints []plumbing.Hash, resumeHash plumbing.Hash) (int, error) {
	if resumeHash.IsZero() {
		return 0, nil
	}
	for idx, checkpoint := range checkpoints {
		if checkpoint == resumeHash {
			return idx + 1, nil
		}
	}
	return 0, fmt.Errorf("temp ref hash %s does not match any planned checkpoint", resumeHash)
}
