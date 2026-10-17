package gitwire

import (
	"fmt"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// 服务端 filter（docs/03 §4）：对齐 git partial clone 语义
// （git 源码 Documentation/technical/partial-clone.adoc 与
// list-objects-filter 规格），外加 banyan 扩展 subtree:<path>。
//
//	广告：upload-pack 能力 "filter"（对照 uploadpack.allowFilter）
//	会话：客户端在 want 区发 "filter <spec>"

// FilterSpec 是解析后的过滤规格。
type FilterSpec struct {
	Kind FilterKind
	Path string // subtree:/sparse: 时有效
}

type FilterKind int

const (
	FilterNone FilterKind = iota
	FilterBlobNone                   // blob:none  不含 blob
	FilterTreeZero                   // tree:0     仅 commit
	FilterSparseOID                  // sparse:oid=<oid> 按稀疏模式
	FilterSubtree                    // subtree:<path> banyan 扩展
)

// ParseFilterSpec 解析并校验 filter 规格。
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

// Spec 渲染回规范字符串。
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

// ApplyFilter 对（虚拟）仓库的对象闭包应用过滤。
// TODO(M1): 在 upload-pack 闭包计算处接入：
//   - blob:none / tree:0 / sparse:oid 按 git list-objects-filter 语义；
//   - subtree:<path>：tree 遍历做路径前缀剪枝（仅子树内 blob/tree
//     + 全部 commit），配合 banyan fetch-objects 事后按 OID 补取。
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
			// TODO(M1): sparse/subtree 的路径级过滤。
			out = append(out, o)
		}
	}
	return out
}

// ObjectEntry 是闭包计算的一个对象条目（M1 由投影+遍历产生）。
type ObjectEntry struct {
	OID  model.OID
	Type model.ObjectType
	Path string
}
