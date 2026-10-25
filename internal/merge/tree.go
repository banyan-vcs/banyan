// Tree entry three-way merge (docs/15 §2.2): the merge-ort process_entries
// trivial subset — per-name 3-way decision, recursion into directories,
// content merge for modify/modify blobs, explicit conflicts for
// delete/modify, add/add divergence, mode/type clashes and anything a blob
// merge reports unclean. Renames are NOT detected: a rename is delete+add
// and lands in the conflict set rather than silently moving (docs/15 §5).
package merge

import (
	"fmt"
	"sort"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Store is the object access merge needs: reads through the repo view,
// writes land in the synthetic-object layer (content-addressed, idempotent).
type Store interface {
	model.ObjectLookup
	PutTree(t *model.Tree) model.OID
	PutBlob(data []byte) model.OID
}

// Trees merges directory trees base/ours/theirs (any may be absent — an
// absent base means add/add against an empty tree). Returns the merged tree
// and the conflicted paths (repo-relative under rel); on conflict the
// result keeps the ours side of each conflicted entry (the caller discards
// conflicted results — the content is for determinism only).
func Trees(store Store, algo model.HashAlgo, base, ours, theirs model.OID, rel string, o Options) (model.OID, []string, error) {
	bt, err := treeOrNil(store, base)
	if err != nil {
		return "", nil, err
	}
	ot, err := treeOrNil(store, ours)
	if err != nil {
		return "", nil, err
	}
	tt, err := treeOrNil(store, theirs)
	if err != nil {
		return "", nil, err
	}
	merged, conflicts, err := mergeTrees(store, algo, bt, ot, tt, rel, o)
	if err != nil {
		return "", nil, err
	}
	if merged == nil {
		merged = &model.Tree{}
	}
	return store.PutTree(merged), conflicts, nil
}

func treeOrNil(store Store, oid model.OID) (*model.Tree, error) {
	if oid == "" {
		return nil, nil
	}
	return store.Tree(oid)
}

func mergeTrees(store Store, algo model.HashAlgo, base, ours, theirs *model.Tree, rel string, o Options) (*model.Tree, []string, error) {
	names := map[string]bool{}
	for _, t := range []*model.Tree{base, ours, theirs} {
		if t == nil {
			continue
		}
		for _, e := range t.Entries {
			names[e.Name] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	out := &model.Tree{}
	var conflicts []string
	for _, name := range sorted {
		eb := findEntry(base, name)
		eo := findEntry(ours, name)
		et := findEntry(theirs, name)
		path := joinPath(rel, name)

		switch {
		case sameEntry(eo, eb):
			if et != nil {
				out.Entries = append(out.Entries, *et)
			}
		case sameEntry(et, eb):
			if eo != nil {
				out.Entries = append(out.Entries, *eo)
			}
		case sameEntry(eo, et):
			if eo != nil {
				out.Entries = append(out.Entries, *eo)
			}
		case eo != nil && et != nil:
			// Both present and different: recurse for dirs, content-merge
			// for blobs, conflict for anything mixed.
			if eo.IsDir && et.IsDir && (eb == nil || eb.IsDir) {
				var bt, ot, tt *model.Tree
				var err error
				if bt, err = treeOrNil(store, ebTarget(eb)); err != nil {
					return nil, nil, err
				}
				if ot, err = treeOrNil(store, eo.Target); err != nil {
					return nil, nil, err
				}
				if tt, err = treeOrNil(store, et.Target); err != nil {
					return nil, nil, err
				}
				sub, subs, err := mergeTrees(store, algo, bt, ot, tt, path, o)
				if err != nil {
					return nil, nil, err
				}
				conflicts = append(conflicts, subs...)
				out.Entries = append(out.Entries, model.TreeEntry{Name: name, Mode: "40000", Target: store.PutTree(sub), IsDir: true})
				continue
			}
			if !eo.IsDir && !et.IsDir && eo.Mode == et.Mode {
				merged, clean, err := mergeBlob(store, eb, eo, et, o)
				if err != nil {
					return nil, nil, err
				}
				if !clean {
					conflicts = append(conflicts, path)
				}
				out.Entries = append(out.Entries, model.TreeEntry{Name: name, Mode: eo.Mode, Target: merged})
				continue
			}
			conflicts = append(conflicts, path)
			out.Entries = append(out.Entries, *eo)
		default:
			// One side deleted, the other changed (or base-side shapes
			// disagree): explicit conflict; keep the surviving side.
			conflicts = append(conflicts, path)
			if eo != nil {
				out.Entries = append(out.Entries, *eo)
			} else if et != nil {
				out.Entries = append(out.Entries, *et)
			}
		}
	}
	return out, conflicts, nil
}

// mergeBlob content-merges a modify/modify (or add/add, base nil) blob.
func mergeBlob(store Store, eb, eo, et *model.TreeEntry, o Options) (model.OID, bool, error) {
	var baseB []byte
	if eb != nil && !eb.IsDir {
		b, err := store.Blob(eb.Target)
		if err != nil {
			return "", false, err
		}
		baseB = b
	}
	oursB, err := store.Blob(eo.Target)
	if err != nil {
		return "", false, err
	}
	theirsB, err := store.Blob(et.Target)
	if err != nil {
		return "", false, err
	}
	merged, clean := Blobs(baseB, oursB, theirsB, o)
	return store.PutBlob(merged), clean, nil
}

func findEntry(t *model.Tree, name string) *model.TreeEntry {
	if t == nil {
		return nil
	}
	for i := range t.Entries {
		if t.Entries[i].Name == name {
			return &t.Entries[i]
		}
	}
	return nil
}

func sameEntry(a, b *model.TreeEntry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Mode == b.Mode && a.Target == b.Target && a.IsDir == b.IsDir
}

func ebTarget(eb *model.TreeEntry) model.OID {
	if eb == nil {
		return ""
	}
	return eb.Target
}

func joinPath(rel, name string) string {
	if rel == "" {
		return name
	}
	return fmt.Sprintf("%s/%s", rel, name)
}
