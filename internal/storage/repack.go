package storage

import (
	"context"
	"fmt"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Repacker does background fragment merging and deltification (docs/02 §5).
//
// Trigger: packs(repo) > fragment threshold (default 64) or object redundancy over threshold.
// Execution: select the union of small packs → delta-encode by time/path locality
//       → new pack+idx → versioned metadata switch (single transaction) → delete old packs once refcounts hit zero.
type Repacker struct {
	Meta   MetaStore
	Local  *LocalShardStore
	Nodes  []Node
	Algo   model.HashAlgo

	FragmentThreshold int // trigger threshold, default 64
}

func NewRepacker(meta MetaStore, local *LocalShardStore, nodes []Node, algo model.HashAlgo) *Repacker {
	return &Repacker{Meta: meta, Local: local, Nodes: nodes, Algo: algo, FragmentThreshold: 64}
}

// RunOnce executes one repack round. Skeleton: returns a plan description; M1/M2 fill in the real merge.
func (r *Repacker) RunOnce(ctx context.Context, repo string) (string, error) {
	// TODO(M1): enumerate packs → select the union → merge-rewrite.
	// TODO(M2): deltification (sliding window + same-directory blobs as preferred bases);
	//           hot-path whole-pack direct serve (response body is the stored pack, zero re-encoding).
	return "", fmt.Errorf("repack: not implemented (M1)")
}
