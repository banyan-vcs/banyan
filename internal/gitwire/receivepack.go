package gitwire

import (
	"context"
	"errors"
	"fmt"
	"bufio"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// serveReceivePack 服务 push（docs/03 §5，写路径 docs/02 §4）。
//
// v1 会话（对照 receive-pack.c）：
//	客户端 POST body：
//	  <old> <new> <refname> [caps]     ← ref 命令行，能力在首条尾部
//	  ...
//	  flush
//	  packfile bytes（纯删除 push 无 pack 段）
//	服务端：
//	  ① pack 校验入库（分片 CAS，不解散）
//	  ② 校验 new head 在 pack 中且为 commit
//	  ③ head CAS（全零 old=新建，全零 new=删除；失败报 non-fast-forward）
//	  ④ report-status 回执
func (s *Server) serveReceivePack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	// demo 友好：push 到不存在的仓库时自动建仓（M2 收紧为 ACL 策略）。
	if _, err := s.Meta.GetRepo(r.Context(), vr.Repo); err != nil {
		if err := s.Meta.CreateRepo(r.Context(), vr.Repo, s.Algo); err != nil {
			http.Error(w, "create repo: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("receive-pack: auto-created repo %q (object-format=%d)", vr.Repo, s.Algo)
	}

	br := bufio.NewReaderSize(r.Body, 256*1024)
	type refUpdate struct {
		Old, New model.OID
		Name     string
	}
	var updates []refUpdate
	sawFlush := false
	for !sawFlush {
		line, flush, err := ReadPacketText(br)
		if err != nil {
			http.Error(w, "read ref commands: "+err.Error(), http.StatusBadRequest)
			return
		}
		if flush {
			sawFlush = true
			break
		}
		// 能力块挂在首条命令行 "\x00<caps>" 之后，先截断再解析。
		cmd, _, _ := strings.Cut(line, "\x00")
		fields := strings.Fields(cmd)
		if len(fields) != 3 {
			http.Error(w, "malformed ref update: "+line, http.StatusBadRequest)
			return
		}
		updates = append(updates, refUpdate{
			Old:  model.OID(fields[0]).NormalizeZero(),
			New:  model.OID(fields[1]).NormalizeZero(),
			Name: fields[2],
		})
	}

	// pack 段：flush 之后到 body 结束（纯删除 push 为空）。
	packBytes, err := io.ReadAll(br)
	if err != nil {
		http.Error(w, "read pack: "+err.Error(), http.StatusBadRequest)
		return
	}

	out := newBufWriter(w)
	fail := func(why string) {
		_ = WritePacket(out, []byte("unpack "+why+"\n"))
		for _, u := range updates {
			_ = WritePacket(out, []byte("ng "+u.Name+" "+why+"\n"))
		}
		_ = WriteFlush(out)
		_ = out.Flush()
	}

	// ① pack 解码 + 分片入库（无 pack 且存在非删除更新时报错）。
	var dp *storage.DecodedPack
	if len(packBytes) > 0 {
		dp, err = storage.DecodePack(packBytes)
		if err != nil {
			fail("pack decode: " + err.Error())
			return
		}
		if _, err := s.Objects.IngestPack(r.Context(), vr.Repo, packBytes); err != nil {
			fail("ingest: " + err.Error())
			return
		}
	}

	// ②③ 逐 ref 校验 + head CAS。report-status 规定顺序：
	// 第一条 pkt 是 "unpack ok"，其后才是各 ref 状态（对照 receive-pack.c）。
	_ = WritePacket(out, []byte("unpack ok\n"))
	for _, u := range updates {
		if !strings.HasPrefix(u.Name, "refs/heads/") {
			_ = WritePacket(out, []byte("ng "+u.Name+" unsupported ref namespace\n"))
			continue
		}
		name := strings.TrimPrefix(u.Name, "refs/heads/")
		if name == "" {
			_ = WritePacket(out, []byte("ng "+u.Name+" empty branch name\n"))
			continue
		}
		if u.New.IsZero() {
			// ref 删除：仅根仓库支持（子树视图是派生的，无独立 timeline）。
			if !vr.Sub.IsRoot() {
				_ = WritePacket(out, []byte("ng "+u.Name+" deletion in subtree view not supported\n"))
				continue
			}
			tl := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: name}
			if err := s.Meta.CASHead(r.Context(), tl, u.Old, u.New); err != nil {
				_ = WritePacket(out, []byte("ng "+u.Name+" "+err.Error()+"\n"))
				continue
			}
			_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
			continue
		}
		// new head 必须在本次 pack 中且为 commit（对象已在 pack 里的
		// 重复 push 场景：也接受已入库的对象——查全局索引兜底）。
		if dp != nil {
			if _, ok := dp.Commits[u.New]; !ok {
				if _, _, rerr := s.Objects.Raw(r.Context(), vr.Repo, u.New); rerr != nil {
					_ = WritePacket(out, []byte("ng "+u.Name+" head object missing\n"))
					continue
				}
			}
		} else if _, _, rerr := s.Objects.Raw(r.Context(), vr.Repo, u.New); rerr != nil {
			_ = WritePacket(out, []byte("ng "+u.Name+" head object missing\n"))
			continue
		}

		if !vr.Sub.IsRoot() {
			// ④ 子树视图 push：收编进统一 DAG（docs/01 §10）——生成规范
			// subtree-merge commit（parents=[根head, C 原样]），C 的 hash
			// 在子树与父视图中保持一致；根 head 前进，视图 tip 随之推进。
			if aerr := s.adoptSubtreePush(r.Context(), vr, name, u.Old, u.New); aerr != nil {
				_ = WritePacket(out, []byte("ng "+u.Name+" "+aerr.Error()+"\n"))
				continue
			}
			_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
			continue
		}

		// 根仓库：常规 timeline CAS。
		tl := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: name}
		if err := s.Meta.CASHead(r.Context(), tl, u.Old, u.New); err != nil {
			_ = WritePacket(out, []byte("ng "+u.Name+" "+err.Error()+"\n"))
			continue
		}
		_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
		log.Printf("receive-pack %s: /%s → %s", vr.Repo, name, u.New)
	}

	_ = WriteFlush(out)
	_ = out.Flush()
}

// adoptSubtreePush 子树 push 的 v4 写路径（docs/01 §12 时间线优先）：
//
//	① 内容级校验（竞态防护，不变）：old 的 X 视图树 == 当前合头的
//	  X 视图树才放行——连续 push 放行 / 陈旧跨视图拒绝（防静默回滚）/
//	  pull 后放行；
//	② 子树时间线 CAS 前进：first push 经 EnsureTimeline 锚定
//	  （head=合头、base=根分叉点），后续以当前本地头为 expected；
//	③ AdoptSubtreePush 收编进根（C 原样保留为第二父，根 head CAS
//	  前进，冲突重试）。收编后投影经身份映射还原为 C 本尊——
//	  push 者的变更回流后仍是自己的 SHA，pull 恒 fast-forward。
func (s *Server) adoptSubtreePush(ctx context.Context, vr VirtualRepo, branch string, old, newHead model.OID) error {
	curTip, rootHead, err := s.subtreeHead(ctx, vr)
	if err != nil {
		return err
	}
	if curTip.IsZero() {
		return fmt.Errorf("subtree view %s unavailable (root has no commits)", vr.Sub)
	}
	if !old.IsZero() && old != curTip && !s.sameXView(vr, old, curTip) {
		return storage.ErrNonFastForward
	}

	// ② 子树时间线 CAS。首次 push：EnsureTimeline 以合头锚定
	//（head=curTip、base=根分叉点），expected 即为该锚定头。
	localTL := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: branch}
	localTip := curTip
	if st, terr := s.Meta.Timeline(ctx, localTL); terr == nil {
		localTip = st.Head
	} else if terr := s.Meta.EnsureTimeline(ctx, localTL, curTip, rootHead); terr != nil {
		return terr
	}
	if err := s.Meta.CASHead(ctx, localTL, localTip, newHead); err != nil {
		return err
	}

	// ③ 收编进根。
	info, err := s.Meta.GetRepo(ctx, vr.Repo)
	if err != nil {
		return err
	}
	rootName := info.DefaultBranch
	if rootName == "" {
		rootName = "main"
	}
	rootTL := model.Timeline{Repo: vr.Repo, Anchor: model.RootAnchor, Name: rootName}
	heads := &metaHeads{meta: s.Meta, ctx: ctx}
	store := s.Objects.TreeStoreFor(vr.Repo)

	for attempt := 0; attempt < 3; attempt++ {
		merge, aerr := model.AdoptSubtreePush(store, s.Algo, vr.Repo, rootName, vr.Sub, newHead, heads)
		if aerr != nil {
			return aerr
		}
		oid := merge.OID(s.Algo)
		s.Objects.PutSynthetic(vr.Repo, oid, model.CommitType, merge.Encode())
		if err := s.Meta.CASHead(ctx, rootTL, rootHead, oid); err == nil {
			log.Printf("adopt %s: subtree %s push %s → root merge %s (parents [%s %s])",
				vr.Repo, vr.Sub, newHead, oid, rootHead, newHead)
			return nil
		}
		// 根被并发推进：重读根 head 重算（merge 的 C 侧不变）。
		state, terr := s.Meta.Timeline(ctx, rootTL)
		if terr != nil {
			return terr
		}
		rootHead = state.Head
	}
	return fmt.Errorf("root head CAS contention after 3 attempts")
}

// metaHeads 把 MetaStore 适配为模型层的 HeadResolver（未知 timeline
// 视作零 head——收编时据此把 C 作为唯一父）。
type metaHeads struct {
	meta storage.MetaStore
	ctx  context.Context
}

func (m *metaHeads) TimelineHead(tl model.Timeline) (model.TimelineState, error) {
	st, err := m.meta.Timeline(m.ctx, tl)
	if errors.Is(err, storage.ErrTimelineUnknown) {
		return model.TimelineState{}, nil
	}
	return st, err
}

// sameXView 内容级 fast-forward 判定：两个提交在路径 X 视角下的树
// 是否相同（X 视角归一：全仓树取 X 子树；树不含 X 首段 ⇒ 该树本身
// 即 X 视角根树——子树侧提交/视图 tip 的混合作用域）。
func (s *Server) sameXView(vr VirtualRepo, a, b model.OID) bool {
	ia, err := s.Objects.CommitInfo(vr.Repo, a)
	if err != nil {
		return false
	}
	ib, err := s.Objects.CommitInfo(vr.Repo, b)
	if err != nil {
		return false
	}
	ta := s.xviewTree(vr, ia.Tree)
	tb := s.xviewTree(vr, ib.Tree)
	return !ta.IsZero() && ta == tb
}

// xviewTree 把树归一到 X 视角（docs/01 §10 混合作用域）。
func (s *Server) xviewTree(vr VirtualRepo, tree model.OID) model.OID {
	lookup := s.Objects.Lookup(vr.Repo)
	segs := vr.Sub.Segments()
	if len(segs) == 0 {
		return tree
	}
	t, err := lookup.Tree(tree)
	if err != nil {
		return ""
	}
	for _, e := range t.Entries {
		if e.Name == segs[0] {
			return model.SubtreeAt(lookup, tree, vr.Sub)
		}
	}
	return tree // 树不含 X 首段 ⇒ 本身即 X 视角根树
}
