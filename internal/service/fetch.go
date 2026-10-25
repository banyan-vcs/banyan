package service

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/model"
)

// TreeService.Fetch: the thin client's one-shot materialization primitive
// (docs/06 §4.1, docs/05 §3.3). Unlike git's have/want pack negotiation the
// client requests immutable content-addressed trees directly:
//
//   - no haveTree   → full recursive walk (clone);
//   - haveTree set  → tree-diff walk: only changed/new trees and blobs —
//     the download equals the difference of the two checkouts, independent
//     of history depth (pull);
//   - an unloadable haveTree silently degrades to a full walk (the old tree
//     may have fallen out of a rewritten view).
//
// Blobs ≤ inlineLimit travel inline; larger ones are listed in Pending for
// per-object GETs (resumable, cache-friendly).
const inlineLimit = 256 * 1024

type FetchRequest struct {
	Repo     string    `json:"repo"`
	Sub      string    `json:"sub,omitempty"`      // subtree view (clone URL /tree/<path>)
	Rev      string    `json:"rev,omitempty"`      // branch; "" = repo default
	HaveTree model.OID `json:"haveTree,omitempty"` // client's cached view tree (pull)
	Paths    []string  `json:"paths,omitempty"`    // sparse: top-level names to include
	Format   string    `json:"format,omitempty"`   // "bin": binary framing response (docs/05 §3.3)
}

type FetchResult struct {
	Branch  string            `json:"branch"`
	Head    model.OID         `json:"head"`
	Tree    model.OID         `json:"tree"` // the view's root tree (manifest anchor)
	Full    bool              `json:"full"` // true when the diff degraded to a full walk
	Trees   map[string][]byte `json:"trees"`
	Blobs   map[string][]byte `json:"blobs"`
	Pending []string          `json:"pending"`
}

func (s *Service) FetchTrees(ctx context.Context, req FetchRequest) (*FetchResult, error) {
	sub, err := parseSub(req.Sub)
	if err != nil {
		return nil, err
	}
	branch := req.Rev
	if branch == "" {
		if branch, err = s.defaultBranchOf(ctx, req.Repo); err != nil {
			return nil, err
		}
	}
	head, tree, _, err := s.viewHead(ctx, req.Repo, sub, branch)
	if err != nil {
		return nil, err
	}
	res := &FetchResult{
		Branch: branch,
		Head:   head,
		Tree:   tree,
		Trees:  map[string][]byte{},
		Blobs:  map[string][]byte{},
	}
	if tree.IsZero() {
		return res, nil // empty repo: nothing to materialize
	}

	sparse := map[string]bool{}
	for _, p := range req.Paths {
		sparse[p] = true
	}
	lookup := s.Objects.Lookup(req.Repo)
	needBlob := func(oid model.OID) {
		t, data, err := s.Objects.Raw(ctx, req.Repo, oid)
		if err != nil || t != model.BlobType {
			return // broken reference: skip, the walk stays best-effort
		}
		if len(data) <= inlineLimit {
			res.Blobs[string(oid)] = data
		} else {
			res.Pending = append(res.Pending, string(oid))
		}
	}
	emitTree := func(oid model.OID) bool {
		if _, done := res.Trees[string(oid)]; done {
			return true
		}
		t, raw, err := s.Objects.Raw(ctx, req.Repo, oid)
		if err != nil || t != model.TreeType {
			return false
		}
		res.Trees[string(oid)] = raw
		return true
	}

	// included: sparse filter at the view root's top level (docs/06 clone --sparse).
	included := func(name string, isDir bool) bool {
		if len(sparse) == 0 {
			return true
		}
		if !isDir {
			return true // root-level files always materialize (cone semantics)
		}
		return sparse[name]
	}

	var walk func(oid model.OID)
	walk = func(oid model.OID) {
		if !emitTree(oid) {
			return
		}
		t, err := lookup.Tree(oid)
		if err != nil {
			return
		}
		// Tree expansion is the batching point (docs/11 P0-1): children
		// locate in one Meta round trip.
		children := make([]model.OID, 0, len(t.Entries))
		for _, e := range t.Entries {
			if included(e.Name, e.IsDir) {
				children = append(children, e.Target)
			}
		}
		s.Objects.Prefetch(ctx, req.Repo, children)
		for _, e := range t.Entries {
			if !included(e.Name, e.IsDir) {
				continue
			}
			if e.IsDir {
				walk(e.Target)
			} else {
				needBlob(e.Target)
			}
		}
	}

	var diff func(oldOID, newOID model.OID)
	diff = func(oldOID, newOID model.OID) {
		if oldOID == newOID {
			return
		}
		if oldOID.IsZero() {
			walk(newOID) // newly created subtree: full walk
			return
		}
		if !emitTree(newOID) {
			return
		}
		oldTree, oerr := lookup.Tree(oldOID)
		if oerr != nil {
			res.Full = true
			walk(newOID) // old tree unreadable here: degrade to full
			return
		}
		newTree, nerr := lookup.Tree(newOID)
		if nerr != nil {
			return
		}
		oldEntries := map[string]model.TreeEntry{}
		for _, e := range oldTree.Entries {
			oldEntries[e.Name] = e
		}
		for _, e := range newTree.Entries {
			old, had := oldEntries[e.Name]
			if e.IsDir {
				var oldSub model.OID
				if had && old.IsDir {
					oldSub = old.Target
				}
				diff(oldSub, e.Target)
				continue
			}
			if !had || old.Target != e.Target {
				needBlob(e.Target)
			}
		}
	}

	if req.HaveTree.IsZero() {
		walk(tree)
	} else {
		diff(req.HaveTree, tree)
	}
	return res, nil
}
