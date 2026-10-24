package gitwire

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Thin-pack base selection (docs/12 P2-6, B8): a have-subtracted fetch's
// new objects relate to objects the client already holds — the previous
// version at the same path. For every served commit whose parent the client
// holds, a path-aware tree diff between the commit and that parent yields
// exactly those (new, old) pairs: changed blobs and every rewritten tree
// along their paths. Bases are restricted to the exclude set — objects
// provably present on the client (its have closure, shallow-aware) — so a
// thin pack never references anything the client would have to fetch.
//
// Served commits also delta against their held parent commit (small but
// free). This mirrors what git's pack-objects approximates with its delta
// window over name-sorted objects, but with the base chosen by structure
// instead of similarity heuristics.
func (s *Server) thinBases(ctx context.Context, vr VirtualRepo, commits map[model.OID][]byte, exclude map[model.OID]bool) map[model.OID]model.OID {
	bases := map[model.OID]model.OID{}
	lookup := s.Objects.Lookup(vr.Repo)
	for oid := range commits {
		ci, err := s.Objects.CommitInfo(vr.Repo, oid)
		if err != nil {
			continue
		}
		for _, p := range ci.Parents {
			if !exclude[p] {
				continue // the client does not hold this parent's side
			}
			pci, perr := s.Objects.CommitInfo(vr.Repo, p)
			if perr != nil {
				continue
			}
			// The commit object itself deltas against its held parent.
			if !exclude[oid] {
				bases[oid] = p
			}
			s.diffTreeBases(vr, lookup, ci.Tree, pci.Tree, exclude, bases)
			break // first held parent suffices
		}
	}
	return bases
}

// diffTreeBases walks two trees recording (served, held) pairs at every
// level: identical subtrees prune; a changed subtree both records the tree
// pair and recurses; changed leaves record the blob pair.
func (s *Server) diffTreeBases(vr VirtualRepo, lookup model.ObjectLookup, newTree, oldTree model.OID, exclude map[model.OID]bool, bases map[model.OID]model.OID) {
	if newTree == oldTree || newTree.IsZero() || oldTree.IsZero() {
		return
	}
	if exclude[newTree] {
		return // identical content the client already holds: not served
	}
	if exclude[oldTree] {
		bases[newTree] = oldTree
	}
	newT, err := lookup.Tree(newTree)
	if err != nil {
		return
	}
	oldT, err := lookup.Tree(oldTree)
	if err != nil {
		return
	}
	oldEntries := map[string]model.TreeEntry{}
	for _, e := range oldT.Entries {
		oldEntries[e.Name] = e
	}
	for _, e := range newT.Entries {
		old, had := oldEntries[e.Name]
		if !had || old.Target == e.Target || old.IsDir != e.IsDir {
			continue // added / unchanged / type swap: no same-path base
		}
		if e.IsDir {
			s.diffTreeBases(vr, lookup, e.Target, old.Target, exclude, bases)
			continue
		}
		if !exclude[e.Target] && exclude[old.Target] {
			bases[e.Target] = old.Target // changed blob: previous version held
		}
	}
}

// thinPackPlan carries what serveClosurePack needs to emit a thin pack.
type thinPackPlan struct {
	bases    map[model.OID]model.OID
	loadBase func(model.OID) (model.ObjectType, []byte, error)
}
