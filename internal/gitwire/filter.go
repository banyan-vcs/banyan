package gitwire

import (
	"fmt"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Server-side filter (docs/03 §4): aligned with git partial clone semantics
// (git sources Documentation/technical/partial-clone.adoc and
// list-objects-filter spec), plus the banyan extension subtree:<path>.
//
//	Advertisement: upload-pack capability "filter" (mirrors uploadpack.allowFilter)
//	Session: the client sends "filter <spec>" in the want section

// FilterSpec is a parsed filter specification.
type FilterSpec struct {
	Kind FilterKind
	Path string // valid for subtree:/sparse:
}

type FilterKind int

const (
	FilterNone FilterKind = iota
	FilterBlobNone                   // blob:none  no blobs
	FilterTreeZero                   // tree:0     commits only
	FilterSparseOID                  // sparse:oid=<oid> by sparse pattern
	FilterSubtree                    // subtree:<path> banyan extension
)

// ParseFilterSpec parses and validates a filter spec.
func ParseFilterSpec(spec string) (*FilterSpec, error) {
	switch {
	case spec == "blob:none":
		return &FilterSpec{Kind: FilterBlobNone}, nil
	case spec == "tree:0":
		return &FilterSpec{Kind: FilterTreeZero}, nil
	case strings.HasPrefix(spec, "sparse:oid="):
		oid := model.OID(strings.TrimPrefix(spec, "sparse:oid="))
		if oid.IsZero() {
			return nil, fmt.Errorf("filter: sparse:oid with empty oid")
		}
		return &FilterSpec{Kind: FilterSparseOID}, nil
	case strings.HasPrefix(spec, "subtree:"):
		path := strings.TrimPrefix(spec, "subtree:")
		anchor, err := model.ParseAnchorPath(path)
		if err != nil {
			return nil, fmt.Errorf("filter: %w", err)
		}
		return &FilterSpec{Kind: FilterSubtree, Path: string(anchor)}, nil
	default:
		return nil, fmt.Errorf("filter: unsupported spec %q", spec)
	}
}

// Spec renders back to the canonical string.
func (f *FilterSpec) Spec() string {
	switch f.Kind {
	case FilterBlobNone:
		return "blob:none"
	case FilterTreeZero:
		return "tree:0"
	case FilterSparseOID:
		return "sparse:oid=…"
	case FilterSubtree:
		return "subtree:" + f.Path
	default:
		return ""
	}
}

// ApplyFilter applies filtering to the object closure of a (virtual) repo.
// TODO(M1): wire in at the upload-pack closure computation:
//   - blob:none / tree:0 / sparse:oid per git list-objects-filter semantics;
//   - subtree:<path>: prune tree traversal by path prefix (only blobs/trees inside the subtree
//     + all commits), paired with banyan fetch-objects fetching missing ones by OID afterwards.
func ApplyFilter(objects []ObjectEntry, f *FilterSpec) []ObjectEntry {
	if f == nil || f.Kind == FilterNone {
		return objects
	}
	out := make([]ObjectEntry, 0, len(objects))
	for _, o := range objects {
		switch f.Kind {
		case FilterBlobNone:
			if o.Type != model.BlobType {
				out = append(out, o)
			}
		case FilterTreeZero:
			if o.Type == model.CommitType {
				out = append(out, o)
			}
		default:
			// TODO(M1): path-level filtering for sparse/subtree.
			out = append(out, o)
		}
	}
	return out
}

// ObjectEntry is one object entry of closure computation (M1: produced by projection + traversal).
type ObjectEntry struct {
	OID  model.OID
	Type model.ObjectType
	Path string
}
