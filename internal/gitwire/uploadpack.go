package gitwire

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
	out := newBufWriter(w)

	var wants []model.OID
	var haves []model.OID
	var clientShallow []model.OID // 客户端当前浅边界（加深/unshallow 请求会上报）
	depth := 0                     // deepen N；0 = 无 deepen
	done := false
	// 无状态 HTTP 协商（对照 remote-curl.c / upload-pack.c get_common_commits）：
	// 每个 POST = 客户端一次完整写完再读（want [+haves] [+flush] [done]）。
	// 服务器在 POST 结束时只写**一行**响应——中途写会导致 done 轮出现
	// 双 ACK，客户端 do{ACK}while(ACK) 会把 PACK 字节当 ACK 行解析
	// （bad pack header）。done 轮 = 终态 ACK/NAK + pack；协商轮 =
	// ACK/NAK + flush-pkt。
	// shallow（deepen）请求例外（对照 git 2.50 http-backend 实测参考流）：
	// 每个响应都以 shallow 段开头——shallow 行（新边界）+ unshallow 行
	// （客户端上报边界中已补全者）+ flush-pkt；协商轮**只**发 shallow 段
	// （无 NAK），done 轮在其后接 ACK/NAK + 按边界裁剪的 pack。
	for !done {
		line, _, err := ReadPacketText(br)
		if err != nil {
			if len(wants) == 0 && len(haves) == 0 {
				http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
				return
			}
			if depth > 0 {
				s.writeShallowSection(out, vr.Repo, wants, clientShallow, depth)
				_ = out.Flush()
				return
			}
			// 协商轮（请求体结束、无 done）——对照 upload-pack.c
			// get_common_commits 的基础模式：有公共提交时 ACK 即终态，
			// 必须在同一响应内直接送 pack（客户端收到裸 ACK 后立即读包）；
			// 无公共提交 NAK + flush-pkt 结束本轮，客户端下一 POST 继续。
			if common := s.firstKnown(r.Context(), vr.Repo, haves); !common.IsZero() {
				_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
				break // 终态 ACK，直接进入 pack 阶段
			}
			_ = WritePacket(out, []byte("NAK\n"))
			_ = WriteFlush(out)
			_ = out.Flush()
			return
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
			haves = append(haves, model.OID(strings.TrimPrefix(line, "have ")))
		case strings.HasPrefix(line, "shallow "):
			clientShallow = append(clientShallow, model.OID(strings.TrimSpace(strings.TrimPrefix(line, "shallow "))))
		case strings.HasPrefix(line, "deepen "):
			if n, derr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "deepen "))); derr == nil && n > 0 {
				depth = n // 2147483647（--unshallow）等超深值自然覆盖全历史
			}
		case strings.HasPrefix(line, "filter "):
			// 未广告 filter 能力，正常客户端不会发送；忽略以防万一。
		case line == "done":
			done = true
		}
	}

	if !done && len(wants) > 0 && len(haves) > 0 {
		// 终态 ACK 已在协商轮写出（break 路径），此处只补 done 轮的终态行。
	}

	if done {
		if depth > 0 {
			s.writeShallowSection(out, vr.Repo, wants, clientShallow, depth)
		}
		if common := s.firstKnown(r.Context(), vr.Repo, haves); !common.IsZero() {
			_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
		} else {
			_ = WritePacket(out, []byte("NAK\n"))
		}
	}

	if len(wants) == 0 {
		_ = out.Flush()
		return
	}

	closureStart := time.Now()
	blobs, trees, commits, err := s.closure(r.Context(), vr, wants, depth, clientShallow)
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

// writeShallowSection 写响应开头的 shallow 段（deepen 请求专用）：
//   - shallow 行：本次深度计算得到的新边界（已纳入但父未全纳入的提交）；
//   - unshallow 行：客户端上报的边界中、本次已补全父链者（--unshallow /
//     加深场景客户端据此清除 .git/shallow 对应条目）；
//   - flush-pkt 收尾（空边界 = 裸 flush，参考流 d99 实测）。
//
// 输出序固定（字典序）保证可测试性；行文无换行，对照参考流字节。
func (s *Server) writeShallowSection(out io.Writer, repo string, wants, clientShallow []model.OID, depth int) {
	included, boundary := s.shallowCut(repo, wants, depth)
	for _, oid := range sortedOIDs(boundary) {
		_ = WritePacket(out, []byte("shallow "+string(oid)))
	}
	cs := map[model.OID]bool{}
	for _, oid := range clientShallow {
		cs[oid] = true
	}
	for _, oid := range sortedOIDs(cs) {
		if included[oid] && !boundary[oid] {
			_ = WritePacket(out, []byte("unshallow "+string(oid)))
		}
	}
	_ = WriteFlush(out)
}

func sortedOIDs(set map[model.OID]bool) []model.OID {
	out := make([]model.OID, 0, len(set))
	for oid := range set {
		out = append(out, oid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// shallowCut 从 wants 沿全部父边 BFS，纳入深度 0..depth-1 层
// （--unshallow 的 2147483647 或超历史深度自然收敛为全量）。
// 返回 (纳入集, 边界集)；边界 = 已纳入但存在未纳入父的提交——
// 这些提交的父链不入 pack，客户端以 .git/shallow 嫁接。
func (s *Server) shallowCut(repo string, wants []model.OID, depth int) (included, boundary map[model.OID]bool) {
	included = map[model.OID]bool{}
	boundary = map[model.OID]bool{}
	parents := map[model.OID][]model.OID{}
	type node struct {
		oid model.OID
		d   int
	}
	queue := make([]node, 0, len(wants))
	for _, w := range wants {
		queue = append(queue, node{w, 0})
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.d >= depth || included[n.oid] {
			continue
		}
		included[n.oid] = true
		ci, err := s.Objects.CommitInfo(repo, n.oid)
		if err != nil {
			continue // 非 commit 对象（防御）：不影响其余层
		}
		parents[n.oid] = ci.Parents
		for _, p := range ci.Parents {
			queue = append(queue, node{p, n.d + 1})
		}
	}
	for oid := range included {
		for _, p := range parents[oid] {
			if !included[p] {
				boundary[oid] = true
				break
			}
		}
	}
	return included, boundary
}

// closure 从 wants 出发收集对象闭包：commit 沿全部 parents 展开、tree
// 沿全部条目展开、blob 收集为止（docs/01 §5——根仓库与子树仓库同一条
// 遍历路径：投影 commit 的 tree 即子树，parents 已在投影时重写）。
// 裁剪：depth > 0 时按浅化边界裁父链；无 deepen 但客户端上报了 shallow
// 边界（浅仓普通 fetch）时以客户端边界为裁剪点——对照参考服务端的
// have 减除 pack 形状（嫁接层之下对象不入包）。
func (s *Server) closure(ctx context.Context, vr VirtualRepo, wants []model.OID, depth int, clientShallow []model.OID) (
	blobs map[model.OID][]byte, trees map[model.OID][]byte, commits map[model.OID][]byte, err error) {
	blobs = map[model.OID][]byte{}
	trees = map[model.OID][]byte{}
	commits = map[model.OID][]byte{}
	visited := map[model.OID]bool{}
	referrer := map[model.OID]model.OID{} // 调试：oid ← 谁把它入队
	var boundary map[model.OID]bool
	if depth > 0 {
		_, boundary = s.shallowCut(vr.Repo, wants, depth)
	} else if len(clientShallow) > 0 {
		boundary = map[model.OID]bool{}
		for _, oid := range clientShallow {
			boundary[oid] = true
		}
	}

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
			if !boundary[oid] {
				for _, p := range ci.Parents {
					enq(p)
				}
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

// firstKnown 返回 haves 中第一个服务端已存在的对象（v1 协商的公共
// 提交判定；基础 ACK 单个即可——不广告 multi_ack）。批量 have 全未知
// 时返回零值。
func (s *Server) firstKnown(ctx context.Context, repo string, haves []model.OID) model.OID {
	for _, oid := range haves {
		if _, _, err := s.Objects.Raw(ctx, repo, oid); err == nil {
			return oid
		}
	}
	return ""
}
