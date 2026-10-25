package gitwire

import (
	"fmt"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Server-side filter (docs/04 §4): aligned with git partial clone semantics
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
	FilterNone      FilterKind = iota
	FilterBlobNone             // blob:none  no blobs
	FilterTreeZero             // tree:0     commits only
	FilterSparseOID            // sparse:oid=<oid> by sparse pattern
	FilterSubtree              // subtree:<path> banyan extension
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

// ServeSupported reports whether the serve path implements the spec
// (docs/04 §4): blob:none and tree:0 follow git list-objects-filter
// semantics; sparse:oid and the banyan subtree: extension are not served on
// the wire and are rejected at request parsing.
func (f *FilterSpec) ServeSupported() bool {
	return f != nil && (f.Kind == FilterBlobNone || f.Kind == FilterTreeZero)
}

// filterDrop reports whether the active partial-clone filter omits one
// object of the fetch closure. Explicitly wanted objects always travel —
// git semantics: a want exempts from filtering, which is exactly what
// lazy-fetch requests rely on (they name the missing blob/tree directly).
func filterDrop(f *FilterSpec, wants map[model.OID]bool, oid model.OID, t model.ObjectType) bool {
	if f == nil || wants[oid] {
		return false
	}
	switch f.Kind {
	case FilterBlobNone:
		return t == model.BlobType
	case FilterTreeZero:
		return t == model.BlobType || t == model.TreeType
	default:
		return false
	}
}
