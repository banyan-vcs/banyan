// Package storage implements distributed pack storage (docs/02-storage.md):
// the pack is the immutable shard unit, placed by rendezvous hashing, with a
// dual-idx system (pack-local idx + central global object index) and
// background repack.
package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

// Node is one shard storage node (M1: this process; M2: remote HTTP node).
type Node struct {
	ID      string
	Local   bool // directly readable on disk from this process
	BaseURL string
}

// PickNodes uses rendezvous (highest random weight) hashing to pick the
// replica node sequence for a pack (docs/02 §1.2). Takes the top replicas by score:
//
//	score(node) = H64(packOID || 0x00 || nodeID)
//
// Adding/removing a node remaps only 1/N of keys, and no virtual-node layer
// (as in consistent hashing) is needed.
func PickNodes(nodes []Node, packOID string, replicas int) ([]Node, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("shard: no storage nodes")
	}
	if replicas > len(nodes) {
		replicas = len(nodes)
	}
	type scored struct {
		n    Node
		rank uint64
	}
	all := make([]scored, 0, len(nodes))
	for _, n := range nodes {
		h := sha256.New()
		h.Write([]byte(packOID))
		h.Write([]byte{0})
		h.Write([]byte(n.ID))
		all = append(all, scored{n, binary.BigEndian.Uint64(h.Sum(nil)[:8])})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].rank != all[j].rank {
			return all[i].rank > all[j].rank
		}
		return all[i].n.ID < all[j].n.ID // stable order, avoiding tie drift
	})
	out := make([]Node, 0, replicas)
	for _, s := range all[:replicas] {
		out = append(out, s.n)
	}
	return out, nil
}
