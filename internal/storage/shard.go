// Package storage 实现分布式 pack 存储（docs/02-storage.md）：
// pack 即不可变分片单元，rendezvous hashing 放置，双 idx 体系
// （pack 局部 idx + 中心全局对象索引），后台 repack。
package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

// Node 是一个分片存储节点（M1：本进程；M2：远端 HTTP 节点）。
type Node struct {
	ID      string
	Local   bool // 本进程可直接读盘
	BaseURL string
}

// PickNodes 用 rendezvous（highest random weight）hashing 为 pack 选出
// 副本节点序列（docs/02 §1.2）。取 score 前 replicas 名：
//
//	score(node) = H64(packOID || 0x00 || nodeID)
//
// 节点增减时仅 1/N 的 key 重映射，且无需一致性哈希的虚拟节点层。
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
		return all[i].n.ID < all[j].n.ID // 稳定序，避免平局漂移
	})
	out := make([]Node, 0, replicas)
	for _, s := range all[:replicas] {
		out = append(out, s.n)
	}
	return out, nil
}
