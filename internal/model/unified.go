package model

import (
	"fmt"
	"strings"
)

// Unified DAG commit history (design v2, docs/01 §10).
//
// Fundamental constraint: a git commit hash binds (root tree, parents,
// signature, message), so "same change, same hash across views" ⇔ "both views
// share one commit object" ⇔ unified DAG. Three primitives follow:
//
//   - SubtreeTip: view tip — deterministic thin commit (tree=subtree,
//     parent=root commit). Ancestors form the unified DAG; parent-tree history
//     enters the subtree under original hashes; display filtering is native
//     `git log -- .` (exactly "show only commits that touched the subtree").
//   - AdoptSubtreePush: adoption-based assembly — canonical subtree-merge
//     commit (parents=[root head, subtree push C kept verbatim]); C's hash is
//     identical across the subtree and parent views.
//   - All objects are pure canonical git encodings with no banyan-private headers.
//
// The legacy split projection (SplitHistory) stays for derived analysis only.

// SubtreeTip builds the subtree view tip: tree = the root commit's subtree
// at path, parents = [root commit].
//
// Determinism: fixed signing identity, time taken from the root commit's
// committer, message determined by (path, rootOID) ⇒ the same (rootOID, path)
// always yields the same hash (cacheable, restart-stable, negotiation-stable).
func SubtreeTip(lookup ObjectLookup, algo HashAlgo, rootOID OID, path AnchorPath) (*AnchoredCommit, error) {
	rc, err := lookup.Commit(rootOID)
	if err != nil {
		return nil, err
	}
	sub := SubtreeAt(lookup, rc.TreeOID, path)
	if sub.IsZero() {
		return nil, fmt.Errorf("%w: subtree %s at %s", ErrObjectNotFound, path, rootOID)
	}
	viewID := Identity{
		Name:  "banyan-view",
		Email: "view@banyan.local",
		When:  rc.Committer.When,
	}
	return &AnchoredCommit{
		Anchor:    RootAnchor, // the root anchor within an "X-as-root" view
		TreeOID:   sub,
		Parents:   []OID{rootOID},
		Author:    viewID,
		Committer: viewID,
		Message:   fmt.Sprintf("banyan: subtree view %s @ %s\n", path, rootOID),
	}, nil
}

// AdoptSubtreePush adopts the subtree push commit C into the unified DAG: emit
// a git-canonical subtree-merge commit — tree = root tree with path replaced by
// C's tree, parents = [root head, C] (C kept verbatim), signature/message from C.
func AdoptSubtreePush(store TreeStore, algo HashAlgo, repo, rootName string, path AnchorPath, childCommit OID, heads HeadResolver) (*AnchoredCommit, error) {
	c, err := store.Commit(childCommit)
	if err != nil {
		return nil, fmt.Errorf("adopt: %w", err)
	}
	state, err := heads.TimelineHead(Timeline{Repo: repo, Anchor: RootAnchor, Name: rootName})
	if err != nil {
		return nil, fmt.Errorf("adopt: root timeline: %w", err)
	}
	var rootTree OID
	var parents []OID
	if !state.Head.IsZero() {
		rc, err := store.Commit(state.Head)
		if err != nil {
			return nil, err
		}
		rootTree = rc.TreeOID
		parents = []OID{state.Head, childCommit}
	} else {
		rootTree = emptyTreeOID(algo)
		parents = []OID{childCommit} // no root history: C is everything
	}
	newRoot, err := ReplaceSubtree(store, algo, rootTree, path, c.TreeOID)
	if err != nil {
		return nil, err
	}
	subject := strings.SplitN(strings.TrimRight(c.Message, "\n"), "\n", 2)[0]
	return &AnchoredCommit{
		Anchor:    RootAnchor,
		TreeOID:   newRoot,
		Parents:   parents,
		Author:    c.Author,
		Committer: c.Committer,
		Message:   fmt.Sprintf("Merge subtree '%s': %s\n", path, subject),
	}, nil
}
