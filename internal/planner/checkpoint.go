package planner

import (
	"fmt"
	"sort"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
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
	return FirstParentChainStoppingAt(store, tip, nil)
}

// FirstParentChainStoppingAt walks the first-parent chain from tip back to
// root, stopping early when a commit's hash is in stopAt. Returns the chain
// in root-to-tip order, excluding any stopAt commits. When tip itself is in
// stopAt, the returned chain is empty. A nil stopAt behaves like the plain
// FirstParentChain walk.
//
// This supports trunk-aware planning: once trunk's ancestry is known, other
// branches only need their divergence chain.
func FirstParentChainStoppingAt(store storer.EncodedObjectStorer, tip plumbing.Hash, stopAt map[plumbing.Hash]struct{}) ([]plumbing.Hash, error) {
	if _, stop := stopAt[tip]; stop {
		return nil, nil
	}
	commit, err := object.GetCommit(store, tip)
	if err != nil {
		return nil, fmt.Errorf("load tip commit %s: %w", tip, err)
	}
	chain := make([]plumbing.Hash, 0, 128)
	for {
		chain = append(chain, commit.Hash)
		if len(commit.ParentHashes) == 0 {
			break
		}
		parent := commit.ParentHashes[0]
		if _, stop := stopAt[parent]; stop {
			break
		}
		commit, err = object.GetCommit(store, parent)
		if err != nil {
			return nil, fmt.Errorf("load parent commit %s: %w", parent, err)
		}
	}
	// Reverse in-place to get root-to-tip order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// TopoChainStoppingAt returns every commit reachable from tip
// (excluding any in stopAt and their ancestors) in a deterministic
// topological order: every commit's parents appear before the commit
// itself in the result. Ties are broken by hash so the order is stable
// across runs of the same source graph — important for resume, which
// looks up a temp-ref commit's position in the rebuilt chain.
//
// Where FirstParentChainStoppingAt walks only the first-parent
// backbone, this includes every merge-pulled side-branch commit too.
// For repos where individual first-parent steps drag in unboundedly
// large second-parent ancestries (the merge-heavy "checkpoint" pattern),
// topo order lets the bootstrap place sub-pack boundaries inside those
// side branches instead of being limited to first-parent granularity.
func TopoChainStoppingAt(store storer.EncodedObjectStorer, tip plumbing.Hash, stopAt map[plumbing.Hash]struct{}) ([]plumbing.Hash, error) {
	if _, stop := stopAt[tip]; stop {
		return nil, nil
	}

	// BFS reachability collection. We need every commit and its parent
	// list to compute in-degrees for the topological sort below.
	type entry struct {
		commit  *object.Commit
		parents []plumbing.Hash
	}
	reachable := map[plumbing.Hash]entry{}
	queue := []plumbing.Hash{tip}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if _, ok := reachable[h]; ok {
			continue
		}
		if _, stop := stopAt[h]; stop {
			continue
		}
		commit, err := object.GetCommit(store, h)
		if err != nil {
			return nil, fmt.Errorf("load commit %s: %w", h, err)
		}
		reachable[h] = entry{commit: commit, parents: commit.ParentHashes}
		for _, p := range commit.ParentHashes {
			if _, stop := stopAt[p]; stop {
				continue
			}
			if _, ok := reachable[p]; ok {
				continue
			}
			queue = append(queue, p)
		}
	}

	// Kahn's algorithm: emit commits whose every reachable parent has
	// already been emitted. children[p] is the list of reachable
	// commits that consider p a parent — so when we emit p, we can
	// decrement each child's in-degree.
	inDeg := make(map[plumbing.Hash]int, len(reachable))
	children := make(map[plumbing.Hash][]plumbing.Hash, len(reachable))
	for h, e := range reachable {
		for _, p := range e.parents {
			if _, ok := reachable[p]; !ok {
				continue
			}
			inDeg[h]++
			children[p] = append(children[p], h)
		}
	}

	ready := make([]plumbing.Hash, 0)
	for h := range reachable {
		if inDeg[h] == 0 {
			ready = append(ready, h)
		}
	}
	sortHashes(ready)

	chain := make([]plumbing.Hash, 0, len(reachable))
	for len(ready) > 0 {
		h := ready[0]
		ready = ready[1:]
		chain = append(chain, h)
		for _, child := range children[h] {
			inDeg[child]--
			if inDeg[child] == 0 {
				ready = appendSortedHash(ready, child)
			}
		}
	}
	if len(chain) != len(reachable) {
		return nil, fmt.Errorf("cycle detected in commit graph (emitted %d of %d)",
			len(chain), len(reachable))
	}
	return chain, nil
}

// sortHashes sorts a hash slice in lexicographic order. Used as the
// tie-breaker in topological emission so the chain is deterministic
// across runs even when several commits are simultaneously ready.
func sortHashes(hs []plumbing.Hash) {
	sort.Slice(hs, func(i, j int) bool {
		return hashLess(hs[i], hs[j])
	})
}

// appendSortedHash inserts h into an already-sorted slice while
// preserving sort order. Cheaper than re-sorting after each append in
// the topological-emission loop where we add one element at a time.
func appendSortedHash(hs []plumbing.Hash, h plumbing.Hash) []plumbing.Hash {
	idx := sort.Search(len(hs), func(i int) bool {
		return !hashLess(hs[i], h)
	})
	hs = append(hs, plumbing.ZeroHash)
	copy(hs[idx+1:], hs[idx:])
	hs[idx] = h
	return hs
}

func hashLess(a, b plumbing.Hash) bool {
	return a.Compare(b.Bytes()) < 0
}

// FirstParentChainFromParents is the parents-map analogue of
// FirstParentChainStoppingAt. parents is keyed by commit hash; the
// value is the commit's parent list (first entry is the first parent).
// Commits not present in parents are treated as roots (chain ends).
// Returns the chain in root-to-tip order, excluding any commit in
// stopAt. When tip itself is in stopAt, returns an empty chain.
//
// Used by the bootstrap planner so it can walk a parents-only map
// extracted from a tree:0 pack instead of a full in-memory object
// store. See ExtractCommitParents in internal/gitproto.
func FirstParentChainFromParents(
	parents map[plumbing.Hash][]plumbing.Hash,
	tip plumbing.Hash,
	stopAt map[plumbing.Hash]struct{},
) ([]plumbing.Hash, error) {
	if _, stop := stopAt[tip]; stop {
		return nil, nil
	}
	if _, ok := parents[tip]; !ok {
		return nil, fmt.Errorf("tip commit %s not found in parents map", tip)
	}
	chain := make([]plumbing.Hash, 0, 128)
	current := tip
	seen := make(map[plumbing.Hash]struct{}, 128)
	for {
		if _, dup := seen[current]; dup {
			return nil, fmt.Errorf("cycle detected at %s", current)
		}
		seen[current] = struct{}{}
		chain = append(chain, current)
		ps, ok := parents[current]
		if !ok || len(ps) == 0 {
			break
		}
		first := ps[0]
		if _, stop := stopAt[first]; stop {
			break
		}
		current = first
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// TopoChainFromParents is the parents-map analogue of
// TopoChainStoppingAt. Same deterministic topological order
// (parents-before-children, hash tiebreak) on a parents-only map.
func TopoChainFromParents(
	parents map[plumbing.Hash][]plumbing.Hash,
	tip plumbing.Hash,
	stopAt map[plumbing.Hash]struct{},
) ([]plumbing.Hash, error) {
	if _, stop := stopAt[tip]; stop {
		return nil, nil
	}

	reachable := map[plumbing.Hash][]plumbing.Hash{}
	queue := []plumbing.Hash{tip}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if _, ok := reachable[h]; ok {
			continue
		}
		if _, stop := stopAt[h]; stop {
			continue
		}
		ps, ok := parents[h]
		if !ok {
			return nil, fmt.Errorf("commit %s not found in parents map", h)
		}
		reachable[h] = ps
		for _, p := range ps {
			if _, stop := stopAt[p]; stop {
				continue
			}
			if _, ok := reachable[p]; ok {
				continue
			}
			queue = append(queue, p)
		}
	}

	inDeg := make(map[plumbing.Hash]int, len(reachable))
	children := make(map[plumbing.Hash][]plumbing.Hash, len(reachable))
	for h, ps := range reachable {
		for _, p := range ps {
			if _, ok := reachable[p]; !ok {
				continue
			}
			inDeg[h]++
			children[p] = append(children[p], h)
		}
	}

	ready := make([]plumbing.Hash, 0)
	for h := range reachable {
		if inDeg[h] == 0 {
			ready = append(ready, h)
		}
	}
	sortHashes(ready)

	chain := make([]plumbing.Hash, 0, len(reachable))
	for len(ready) > 0 {
		h := ready[0]
		ready = ready[1:]
		chain = append(chain, h)
		for _, child := range children[h] {
			inDeg[child]--
			if inDeg[child] == 0 {
				ready = appendSortedHash(ready, child)
			}
		}
	}
	if len(chain) != len(reachable) {
		return nil, fmt.Errorf("cycle detected in commit graph (emitted %d of %d)",
			len(chain), len(reachable))
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
	for range sampleCount - 1 {
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
