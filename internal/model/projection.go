package model

import (
	"fmt"
)

// git projection (download read path) — docs/design/data-model.md §5.
//
//   - Root projection: an anchor="/" commit is a git commit as-is (zero translation);
//   - Subtree projection: DAG-version subtree split — full-parent traversal, preserving branch commits and
//     merge structure, semantics aligned with `git log -- <path>` / `git subtree split`;
//     pure function ⇒ stable SHA ⇒ cacheable, incremental.

// ProjectionAlgoVer is the projection algorithm version. Changing it = opening a new cache namespace
// without breaking existing clients (old projected objects stay valid, just no longer extended).
const ProjectionAlgoVer = 1

// SubtreeAt returns the OID of the subtree at path in the root tree (zero if absent).
func SubtreeAt(lookup ObjectLookup, root OID, path AnchorPath) OID {
	cur := root
	for _, seg := range path.Segments() {
		tree, err := lookup.Tree(cur)
		if err != nil {
			return ""
		}
		found := false
		for _, e := range tree.Entries {
			if e.Name == seg {
				if !e.IsDir {
					return ""
				}
				cur = e.Target
				found = true
				break
			}
		}
		if !found {
			return ""
		}
	}
	return cur
}

// SubtreeSplit projects a root-timeline commit into a projected commit
// "rooted at the path subtree" (docs/design/data-model.md §5.2). Deterministic: same input, same output.
func SubtreeSplit(lookup ObjectLookup, algo HashAlgo, rootCommitOID OID, path AnchorPath) (*AnchoredCommit, error) {
	c, err := lookup.Commit(rootCommitOID)
	if err != nil {
		return nil, err
	}
	sub, err := RequireSubtreeAt(lookup, c.TreeOID, path)
	if err != nil {
		return nil, fmt.Errorf("split %s: %w", rootCommitOID, err)
	}
	return &AnchoredCommit{
		Anchor:        RootAnchor, // the projection is a root anchor in the virtual repo rooted at P
		TreeOID:       sub,
		Parents:       nil, // backfilled uniformly by SplitHistory (OID finalization requires parents first)
		Author:        c.Author,
		Committer:     c.Committer,
		Message:       c.Message,
		ProjectedFrom: rootCommitOID,
		// Weave provenance rides the projection (docs/design/data-model.md §13): a root-local
		// spine commit that is itself a fold of a subtree source stays
		// recognizable as such inside scoped views, so their folds can skip
		// the already-embedded source instead of duplicating its subject.
		WovenFrom: c.WovenFrom,
	}, nil
}

// RequireSubtreeAt is like SubtreeAt but errors when the path is absent.
func RequireSubtreeAt(lookup ObjectLookup, root OID, path AnchorPath) (OID, error) {
	sub := SubtreeAt(lookup, root, path)
	if sub.IsZero() {
		return "", fmt.Errorf("%w: subtree %s", ErrObjectNotFound, path)
	}
	return sub, nil
}

// SplitResult is the subtree projection output.
type SplitResult struct {
	// Commits are the projected commits (topological order, new→old), containing only
	// commits that changed the path; branch commits become standalone nodes, merges keep multiple parent edges.
	Commits []*AnchoredCommit
	// Head is the virtual repo's head: the head itself or its nearest projected ancestor.
	Head OID
}

// SplitHistory performs a DAG-version subtree split along the root timeline (docs/design/data-model.md §5.2).
//
// Algorithm (old→new, guaranteeing acyclic OID finalization):
//  1. Full-parent DFS collects reachable commits; post-order is topological order (old→new);
//  2. changed(C) = path subtree exists and (no parent, or any parent's subtree ≠ C's subtree);
//  3. Old→new, build projections for changed commits: Parents[P] = resolve(P) — the nearest
//     projected ancestor along parent edges (first non-zero, parent order first); successor
//     projections are already final by then, so OID references never go stale; resolve memoizes unchanged nodes.
func SplitHistory(lookup ObjectLookup, algo HashAlgo, head OID, path AnchorPath) (*SplitResult, error) {
	const maxCommits = 1000000 // runaway guard; shallowing truncation arrives in M3

	commits := map[OID]*AnchoredCommit{}
	visited := map[OID]bool{}
	var topo []OID // old→new
	steps := 0

	var visit func(oid OID) error
	visit = func(oid OID) error {
		if visited[oid] {
			return nil
		}
		visited[oid] = true
		c, err := lookup.Commit(oid)
		if err != nil {
			return err
		}
		commits[oid] = c
		steps++
		if steps > maxCommits {
			return fmt.Errorf("split history: exceeded %d commits", maxCommits)
		}
		for _, p := range c.Parents {
			if err := visit(p); err != nil {
				return err
			}
		}
		topo = append(topo, oid)
		return nil
	}
	if err := visit(head); err != nil {
		return nil, err
	}

	// Subtree at path for each commit (zero = path absent), memoized.
	subOf := map[OID]OID{}
	for _, oid := range topo {
		subOf[oid] = SubtreeAt(lookup, commits[oid].TreeOID, path)
	}

	// changed set: path present and differs from any parent (root commits included).
	changed := map[OID]bool{}
	for _, oid := range topo {
		s := subOf[oid]
		if s.IsZero() {
			continue
		}
		c := commits[oid]
		for _, p := range c.Parents {
			if subOf[p] != s {
				changed[oid] = true
				break
			}
		}
		if len(c.Parents) == 0 {
			changed[oid] = true
		}
	}

	// Old→new: build projections, backfill parents, finalize OIDs.
	res := &SplitResult{}
	projOID := map[OID]OID{}     // changed commit → final projected OID
	resolveMemo := map[OID]OID{} // unchanged commit → nearest projected ancestor

	var resolve func(oid OID) OID
	resolve = func(oid OID) OID {
		if o, ok := projOID[oid]; ok {
			return o
		}
		if o, ok := resolveMemo[oid]; ok {
			return o
		}
		var out OID
		for _, p := range commits[oid].Parents {
			if r := resolve(p); !r.IsZero() {
				out = r
				break
			}
		}
		resolveMemo[oid] = out
		return out
	}

	for _, oid := range topo { // old→new: successor projections finalize first
		if !changed[oid] {
			continue
		}
		// Identity mapping (docs/design/data-model.md §12): the X projection of this path's adoption merge
		// M=[rootPrev, C] (C scoped to X, tree equivalence) = C itself — zero new commits,
		// zero duplication; the pusher's change keeps its own SHA after flowing back through the root.
		// C's parent chain stays its original timeline anchor, unrewritten.
		c := commits[oid]
		if len(c.Parents) == 2 && !subOf[oid].IsZero() &&
			commits[c.Parents[1]].TreeOID == subOf[oid] {
			// Tree equivalence check: second parent's tree == the root tree's subtree at X
			// ⇒ an adoption merge on this path; the projection = the adopted commit itself.
			projOID[oid] = c.Parents[1]
			continue
		}
		pc, err := SubtreeSplit(lookup, algo, oid, path)
		if err != nil {
			return nil, err
		}
		var parents []OID
		seenParent := map[OID]bool{}
		for _, p := range commits[oid].Parents {
			r := resolve(p)
			if !r.IsZero() && !seenParent[r] {
				seenParent[r] = true
				parents = append(parents, r)
			}
		}
		pc.Parents = parents
		projOID[oid] = pc.OID(algo) // final from here on, never changed
		res.Commits = append(res.Commits, pc)
	}
	if len(res.Commits) == 0 {
		return res, nil
	}
	res.Head = resolve(head)
	if res.Head.IsZero() {
		res.Head = res.Commits[0].OID(algo) // theoretically unreachable: a changed node must exist in head's reachable set
	}
	// Reverse the output to new→old, convenient for callers taking head/iterating.
	for i, j := 0, len(res.Commits)-1; i < j; i, j = i+1, j-1 {
		res.Commits[i], res.Commits[j] = res.Commits[j], res.Commits[i]
	}
	return res, nil
}

// ProjectionHeads derives every top-level directory's projected head in
// ONE shared history walk — the advertisement path (docs/operations/
// performance.md §3b finding ④: a SplitHistory per directory made the
// cold advertisement O(dirs × history) and persisted every directory's
// projection before answering; the shared walk reads each commit and its
// root tree ONCE, advances every directory's chain in the same pass, and
// persists nothing — the /tree/<dir> face still materializes on demand
// and must agree: the per-directory derivation below is SplitHistory's
// to the byte, so heads are OID-identical).
//
// Callers with a local timeline at or below a directory must NOT use its
// shared head: SubtreeHead's append-weave folds locals in (a strictly
// longer chain); fall back per directory there.
func ProjectionHeads(lookup ObjectLookup, algo HashAlgo, head OID) (map[string]*SplitResult, error) {
	const maxCommits = 1000000 // the same runaway guard as SplitHistory

	// 1. Identical DFS/topo collection (old→new).
	commits := map[OID]*AnchoredCommit{}
	visited := map[OID]bool{}
	var topo []OID
	steps := 0
	var visit func(oid OID) error
	visit = func(oid OID) error {
		if visited[oid] {
			return nil
		}
		visited[oid] = true
		c, err := lookup.Commit(oid)
		if err != nil {
			return err
		}
		commits[oid] = c
		steps++
		if steps > maxCommits {
			return fmt.Errorf("projection heads: exceeded %d commits", maxCommits)
		}
		for _, p := range c.Parents {
			if err := visit(p); err != nil {
				return err
			}
		}
		topo = append(topo, oid)
		return nil
	}
	if err := visit(head); err != nil {
		return nil, err
	}

	// Directory set: the head root tree's top-level directories — the same
	// enumeration the advertisement iterates (gitlinks excluded).
	rootTree, terr := lookup.Tree(commits[head].TreeOID)
	if terr != nil {
		return nil, terr
	}
	var dirs []string
	for _, e := range rootTree.Entries {
		if e.IsDir && e.Mode != "160000" {
			dirs = append(dirs, e.Name)
		}
	}
	out := make(map[string]*SplitResult, len(dirs))
	if len(dirs) == 0 {
		return out, nil
	}
	dirIdx := make(map[string]int, len(dirs))
	for i, d := range dirs {
		dirIdx[d] = i
	}

	// 2. Per-commit subtree row — ONE root-tree read per distinct tree,
	// shared by every directory (the sharing is the whole point).
	treeMemo := map[OID][]OID{}
	subOf := make([][]OID, len(topo))
	for i, oid := range topo {
		t := commits[oid].TreeOID
		if row, ok := treeMemo[t]; ok {
			subOf[i] = row
			continue
		}
		row := make([]OID, len(dirs))
		if tree, err := lookup.Tree(t); err == nil {
			for _, e := range tree.Entries {
				if e.IsDir {
					if j, ok := dirIdx[e.Name]; ok {
						row[j] = e.Target
					}
				}
			}
		}
		treeMemo[t] = row
		subOf[i] = row
	}
	rowOf := make(map[OID][]OID, len(topo))
	for i, oid := range topo {
		rowOf[oid] = subOf[i]
	}

	// 3. Old→new, every directory's chain advanced in the same pass —
	// SplitHistory's step 3 per directory, shared reads.
	type dirState struct {
		proj    map[OID]OID // changed commit → projected OID (or adoption identity)
		resolve map[OID]OID // unchanged commit → nearest projected ancestor
		chain   []*AnchoredCommit // synthesized projections, old→new (adoption links carry no object)
	}
	states := make([]dirState, len(dirs))
	for i := range states {
		states[i] = dirState{proj: map[OID]OID{}, resolve: map[OID]OID{}}
	}
	var resolveDir func(st *dirState, oid OID) OID
	resolveDir = func(st *dirState, oid OID) OID {
		if o, ok := st.proj[oid]; ok {
			return o
		}
		if o, ok := st.resolve[oid]; ok {
			return o
		}
		var res OID
		for _, p := range commits[oid].Parents {
			if r := resolveDir(st, p); !r.IsZero() {
				res = r
				break
			}
		}
		st.resolve[oid] = res
		return res
	}
	for i, oid := range topo {
		c := commits[oid]
		row := subOf[i]
		parentRows := make([][]OID, len(c.Parents))
		for pi, p := range c.Parents {
			parentRows[pi] = rowOf[p]
		}
		for di := range states {
			st := &states[di]
			s := row[di]
			if s.IsZero() {
				continue
			}
			changed := false
			if len(c.Parents) == 0 {
				changed = true
			} else {
				for pi := range c.Parents {
					if parentRows[pi] == nil || parentRows[pi][di] != s {
						changed = true
						break
					}
				}
			}
			if !changed {
				continue
			}
			// Adoption-merge identity (docs/design/data-model.md §12), exactly
			// as SplitHistory: the projection of an adoption merge IS the
			// adopted commit.
			if len(c.Parents) == 2 && commits[c.Parents[1]].TreeOID == s {
				st.proj[oid] = c.Parents[1]
				continue
			}
			var parents []OID
			seen := map[OID]bool{}
			for _, p := range c.Parents {
				if r := resolveDir(st, p); !r.IsZero() && !seen[r] {
					seen[r] = true
					parents = append(parents, r)
				}
			}
			pc := &AnchoredCommit{
				Anchor:        RootAnchor,
				TreeOID:       s,
				Parents:       parents,
				Author:        c.Author,
				Committer:     c.Committer,
				Message:       c.Message,
				ProjectedFrom: oid,
				WovenFrom:     c.WovenFrom,
			}
			st.proj[oid] = pc.OID(algo)
			st.chain = append(st.chain, pc)
		}
	}
	for di, d := range dirs {
		h := resolveDir(&states[di], head)
		if h.IsZero() {
			continue
		}
		res := &SplitResult{Head: h, Commits: make([]*AnchoredCommit, len(states[di].chain))}
		copy(res.Commits, states[di].chain)
		for i, j := 0, len(res.Commits)-1; i < j; i, j = i+1, j-1 { // → new→old
			res.Commits[i], res.Commits[j] = res.Commits[j], res.Commits[i]
		}
		out[d] = res
	}
	return out, nil
}
