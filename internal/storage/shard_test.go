package storage

import (
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
)

func TestPickNodesRendezvousStability(t *testing.T) {
	nodes := []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	pick := func(pack string) []string {
		got, err := PickNodes(nodes, pack, 2)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(got))
		for i, n := range got {
			out[i] = n.ID
		}
		return out
	}
	// 确定性：同 key 同结果。
	if p1, p2 := pick("pack-1"), pick("pack-1"); p1[0] != p2[0] || p1[1] != p2[1] {
		t.Fatalf("rendezvous not deterministic: %v vs %v", p1, p2)
	}
	// 分布性：100 个 key 应覆盖多个节点（不至于全压一个）。
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		key := string(rune('a'+i%26)) + "-" + string(rune('0'+i%10))
		seen[pick(key)[0]]++
	}
	if len(seen) < 3 {
		t.Fatalf("poor distribution: %v", seen)
	}
	// 节点摘除：其余 key 的首选应尽量少漂移（rendezvous 性质，期望 ~1/N）。
	short := []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	same := 0
	for i := 0; i < 100; i++ {
		key := string(rune('a'+i%26)) + "-x"
		before, _ := PickNodes(nodes, key, 1)
		after, err := PickNodes(short, key, 1)
		if err != nil {
			t.Fatal(err)
		}
		if before[0].ID == after[0].ID {
			same++
		}
	}
	if same < 60 { // 摘除 1/5 节点，期望 ~80 个 key 不变
		t.Fatalf("removing a node moved %d/100 primary keys (rendezvous should move ~20)", 100-same)
	}
}

func TestEncodeDecodeIdxRoundTrip(t *testing.T) {
	offsets := map[model.OID]uint32{
		"0000000000000000000000000000000000000001": 12,
		"0000000000000000000000000000000000000002": 480,
		"ffffffffffffffffffffffffffffffffffffffff": 0x90,
	}
	packOID := model.OID("abcdefabcdefabcdefabcdefabcdefabcdefabcd")
	b, err := EncodeIdx(model.SHA1, packOID, offsets)
	if err != nil {
		t.Fatal(err)
	}
	pi, err := DecodeIdx(b, model.SHA1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pi.Entries) != 3 {
		t.Fatalf("entries: %d", len(pi.Entries))
	}
	for k, v := range offsets {
		if pi.Entries[k] != v {
			t.Fatalf("offset mismatch for %s: %d != %d", k, pi.Entries[k], v)
		}
	}
}
