package gitwire

import (
	"time"
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// serveUploadPack 服务 fetch/clone 数据阶段（docs/03 §3）。
//
// v1 会话（对照 upload-pack.c 状态机）：
//	客户端 POST body（pkt-line 流）：
//	  <want OID> [caps]           ← 能力在首条 want 尾部
//	  ...
//	  [<have OID>...]             ← 协商：客户端已有 commit（MVP 忽略，回全量闭包）
//	  flush / done
//	服务端应答：
//	  NAK pkt（未协商公共 commit）
//	  packfile 原始字节（未广告 side-band ⇒ 客户端按裸 pack 读）
func (s *Server) serveUploadPack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	if _, err := s.Meta.GetRepo(r.Context(), vr.Repo); err != nil {
		http.Error(w, "repository not found: "+vr.Repo, http.StatusNotFound)
		return
	}

	br := bufio.NewReaderSize(r.Body, 64*1024)
	var wants []model.OID
	done := false
	for !done {
		line, flush, err := ReadPacketText(br)
		if err != nil {
			http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if flush {
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			fields := strings.Fields(strings.TrimPrefix(line, "want "))
			if len(fields) == 0 {
				http.Error(w, "malformed want", http.StatusBadRequest)
				return
			}
			wants = append(wants, model.OID(strings.SplitN(fields[0], "\x00", 2)[0]))
		case strings.HasPrefix(line, "have "):
			// MVP：忽略 have，应答全量闭包（正确但非最优；增量协商 M2）。
		case strings.HasPrefix(line, "filter "):
			// 未广告 filter 能力，正常客户端不会发送；忽略以防万一。
		case line == "done":
			done = true
		}
	}

	out := newBufWriter(w)
	_ = WritePacket(out, []byte("NAK\n"))

	if len(wants) == 0 {
		_ = out.Flush()
		return
	}

	closureStart := time.Now()
	blobs, trees, commits, err := s.closure(r.Context(), vr, wants)
	closureDur := time.Since(closureStart)
	if err != nil {
		log.Printf("upload-pack %s%s: closure: %v", vr.Repo, vr.Sub, err)
		// 头已写出，只能以空应答收尾；客户端将报对象缺失。
		_ = out.Flush()
		return
	}
	encodeStart := time.Now()
	// 热路径整 pack 直传（docs/02 §5）：闭包恰等于某个已存 pack 时，
	// 流式回传 pack 原始字节，跳过重哈希 + 重压缩。
	wanted := make(map[model.OID]bool, len(commits)+len(trees)+len(blobs))
	for oid := range commits {
		wanted[oid] = true
	}
	for oid := range trees {
		wanted[oid] = true
	}
	for oid := range blobs {
		wanted[oid] = true
	}
	served := "encode"
	if packOID, ok := s.Objects.ExactPackHit(vr.Repo, wanted); ok {
		if raw, err := s.Objects.Local.GetPackBytes(vr.Repo, packOID); err == nil {
			if _, werr := out.Write(raw); werr == nil {
				served = "exact-pack " + string(packOID)
			}
		}
	}
	if served == "encode" {
		if err := storage.EncodePack(out, blobs, trees, commits); err != nil {
			log.Printf("upload-pack %s%s: encode: %v", vr.Repo, vr.Sub, err)
		}
	}
	_ = out.Flush()
	log.Printf("upload-pack %s%s: %d wants → %d commits, %d trees, %d blobs (closure %s, serve %s/%s)",
		vr.Repo, vr.Sub, len(wants), len(commits), len(trees), len(blobs),
		closureDur.Round(time.Millisecond), served, time.Since(encodeStart).Round(time.Millisecond))
}

// closure 从 wants 出发收集对象闭包：commit 沿全部 parents 展开、tree
// 沿全部条目展开、blob 收集为止（docs/01 §5——根仓库与子树仓库同一条
// 遍历路径：投影 commit 的 tree 即子树，parents 已在投影时重写）。
func (s *Server) closure(ctx context.Context, vr VirtualRepo, wants []model.OID) (
	blobs map[model.OID][]byte, trees map[model.OID][]byte, commits map[model.OID][]byte, err error) {
	blobs = map[model.OID][]byte{}
	trees = map[model.OID][]byte{}
	commits = map[model.OID][]byte{}
	visited := map[model.OID]bool{}
	referrer := map[model.OID]model.OID{} // 调试：oid ← 谁把它入队

	queue := append([]model.OID(nil), wants...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if visited[oid] {
			continue
		}
		visited[oid] = true

		t, raw, err := s.Objects.Raw(ctx, vr.Repo, oid)
		if err != nil {
			// 调试：转储引用者上下文（类型 + 原始内容前 256 字节）。
			refType, refRaw, rerr := s.Objects.Raw(ctx, vr.Repo, referrer[oid])
			refDump := fmt.Sprintf("type=%s rerr=%v raw=%.256x", refType, rerr, refRaw)
			return nil, nil, nil, fmt.Errorf("closure: %w (object %s, referred by %s [%s])", err, oid, referrer[oid], refDump)
		}
		enq := func(child model.OID) {
			if _, seen := referrer[child]; !seen {
				referrer[child] = oid
			}
			queue = append(queue, child)
		}
		switch t {
		case model.CommitType:
			commits[oid] = raw
			// 走 commit-graph 索引：不再逐对象解码 commit（docs/04）。
			ci, err := s.Objects.CommitInfo(vr.Repo, oid)
			if err != nil {
				return nil, nil, nil, err
			}
			enq(ci.Tree)
			for _, p := range ci.Parents {
				enq(p)
			}
		case model.TreeType:
			trees[oid] = raw
			// 走 tree-entry 索引：路径/子节点展开不再重复解析树字节。
			entries, err := s.Objects.TreeEntries(vr.Repo, oid)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("closure: tree %s: %w", oid, err)
			}
			for _, e := range entries {
				enq(e.Target)
			}
		case model.BlobType:
			blobs[oid] = raw
		default:
			return nil, nil, nil, fmt.Errorf("closure: unsupported object type %q for %s", t, oid)
		}
	}
	return blobs, trees, commits, nil
}
