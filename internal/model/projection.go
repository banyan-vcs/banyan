package model

import (
	"fmt"
)

// git projection (download read path) — docs/01 §5.
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
// "rooted at the path subtree" (docs/01 §5.2). Deterministic: same input, same output.
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

// SplitHistory performs a DAG-version subtree split along the root timeline (docs/01 §5.2).
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
		// Identity mapping (docs/01 §12): the X projection of this path's adoption merge
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
