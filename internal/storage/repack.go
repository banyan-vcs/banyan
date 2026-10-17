package storage

import (
	"context"
	"fmt"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Repacker 后台碎片合并与 delta 化（docs/02 §5）。
//
// 触发：packs(repo) > 碎片阈值（默认 64）或对象冗余超阈。
// 执行：圈定小 pack 并集 → 按时间/路径局部性 delta 化 → 新 pack+idx
//       → 元数据版本化切换（单事务）→ 旧 pack 引用计数归零后删除。
type Repacker struct {
	Meta   MetaStore
	Local  *LocalShardStore
	Nodes  []Node
	Algo   model.HashAlgo

	FragmentThreshold int // 触发阈值，默认 64
}

func NewRepacker(meta MetaStore, local *LocalShardStore, nodes []Node, algo model.HashAlgo) *Repacker {
	return &Repacker{Meta: meta, Local: local, Nodes: nodes, Algo: algo, FragmentThreshold: 64}
}

// RunOnce 执行一轮 repack。骨架：返回计划描述；M1/M2 填充真实合并。
func (r *Repacker) RunOnce(ctx context.Context, repo string) (string, error) {
	// TODO(M1): 枚举 packs → 圈定并集 → 合并重写。
	// TODO(M2): delta 化（滑动窗口 + 同目录 blob 优先 base）；
	//           热路径整 pack 直传（响应体即存储 pack，零重编码）。
	return "", fmt.Errorf("repack: not implemented (M1)")
}
