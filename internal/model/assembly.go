package model

import (
	"fmt"
)

// Assembly algorithm (upstream write path) — docs/02 §4.
//
// Information flow: subdirectory local commits ──ReplaceSubtree──▶ parent's
//        new tree ──synthesis──▶ root synthetic commit into the root timeline.
// All inputs are immutable objects ⇒ acyclic, deterministic, cacheable.

// TreeStore adds tree writes on top of ObjectLookup: new trees rewritten
// during assembly and projection must be persisted (server-side semantics);
// MemoryLookup / the storage layer each implement it.
type TreeStore interface {
	ObjectLookup
	PutTree(t *Tree) OID
}

// ReplaceSubtree returns the new root tree OID with the subtree at path in
// baseRoot replaced by subtree; a root path returns subtree directly. Trees
// along the path are rewritten and persisted via store.PutTree (immutable, content-addressed).
func ReplaceSubtree(store TreeStore, algo HashAlgo, baseRoot OID, path AnchorPath, subtree OID) (OID, error) {
	segs := path.Segments()
	if len(segs) == 0 {
		return subtree, nil
	}
	return replaceAt(store, algo, baseRoot, segs, subtree)
}

func replaceAt(store TreeStore, algo HashAlgo, treeOID OID, segs []string, subtree OID) (OID, error) {
	tree, err := store.Tree(treeOID)
	if err != nil {
		return "", fmt.Errorf("replace subtree %s: %w", treeOID, err)
	}
	name := segs[0]
	replaced := false
	entries := make([]TreeEntry, 0, len(tree.Entries)+1)
	for _, e := range tree.Entries {
		if e.Name == name {
			if !e.IsDir && len(segs) > 1 {
				return "", fmt.Errorf("replace subtree: %q in %s is a file, not a directory", name, treeOID)
			}
			if len(segs) == 1 {
				entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: subtree, IsDir: true})
				replaced = true
			} else {
				child, err := replaceAt(store, algo, e.Target, segs[1:], subtree)
				if err != nil {
					return "", err
				}
				entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: child, IsDir: true})
				replaced = true
			}
		} else {
			entries = append(entries, e)
		}
	}
	if !replaced {
		if len(segs) == 1 {
			entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: subtree, IsDir: true})
		} else {
			// Intermediate layer absent: build the missing directories level by level.
			child, err := replaceAt(store, algo, emptyTreeOID(algo), segs[1:], subtree)
			if err != nil {
				return "", err
			}
			entries = append(entries, TreeEntry{Name: name, Mode: "40000", Target: child, IsDir: true})
		}
	}
	return store.PutTree(&Tree{Entries: entries}), nil
}

func emptyTreeOID(algo HashAlgo) OID {
	return ObjectOID(TreeType, nil, algo)
}

// HeadResolver provides a timeline's current head (implemented by MetaStore).
type HeadResolver interface {
	TimelineHead(tl Timeline) (TimelineState, error)
}

// CurrentTree computes directory D's current tree (docs/02 §4.1):
//
//	CurrentTree(D) = base(D) ⊕ replace-children(D)
//
// base(D) is the tree of D's local timeline head; then each subdirectory's
// CurrentTree is swapped in. The skeleton uses lazy single-level replacement:
// only subdirectories with a local timeline are replaced. Full recursive
// assembly lands in the M1 derived engine (incl. multi-subdir merge order).
func CurrentTree(lookup ObjectLookup, heads HeadResolver, algo HashAlgo, tl Timeline) (OID, error) {
	state, err := heads.TimelineHead(tl)
	if err != nil {
		return "", err
	}
	commit, err := lookup.Commit(state.Head)
	if err != nil {
		return "", err
	}
	root := commit.TreeOID
	// Enumerate active timelines in the same repo anchored at this directory's
	// child layer and replace each.
	// TODO(M1): provided by MetaStore.ListTimelines; single-level skeleton semantics kept here.
	return root, nil
}

// AssembleRoot performs upward assembly after a subdirectory commit (docs/02 §4.2):
//
//  1. root_new = ReplaceSubtree(CurrentTree("/"), path, C.TreeOID)
//  2. synthetic commit into the root timeline (M1 synchronous mode)
//
// Returns the synthetic commit to write; the caller CASes new objects and the root head.
func AssembleRoot(store TreeStore, heads HeadResolver, algo HashAlgo, repo, rootName string, path AnchorPath, childCommit OID, by Identity) (*AnchoredCommit, error) {
	child, err := store.Commit(childCommit)
	if err != nil {
		return nil, fmt.Errorf("assemble: %w", err)
	}
	rootTL := Timeline{Repo: repo, Anchor: RootAnchor, Name: rootName}
	state, err := heads.TimelineHead(rootTL)
	if err != nil {
		return nil, fmt.Errorf("assemble: root timeline: %w", err)
	}
	var baseRoot OID
	var parents []OID
	if !state.Head.IsZero() {
		rootCommit, err := store.Commit(state.Head)
		if err != nil {
			return nil, err
		}
		baseRoot = rootCommit.TreeOID
		parents = []OID{state.Head}
	} else {
		baseRoot = emptyTreeOID(algo)
	}
	newRoot, err := ReplaceSubtree(store, algo, baseRoot, path, child.TreeOID)
	if err != nil {
		return nil, err
	}
	return &AnchoredCommit{
		Anchor:    RootAnchor,
		TreeOID:   newRoot,
		Parents:   parents,
		Author:    by,
		Committer: by,
		Message:   fmt.Sprintf("banyan: assemble %s@%s\n", path, childCommit),
		Synthetic: true,
	}, nil
}

// EmptyTreeOID exposes the content-addressed empty tree (the fold-genesis
// base for view weaving, docs/02 §13).
func EmptyTreeOID(algo HashAlgo) OID { return emptyTreeOID(algo) }
