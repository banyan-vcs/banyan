package gitwire

import (
	"context"
	"errors"
	"fmt"
	"time"
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
			// ref 删除。
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
		tl := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: name}
		expected := u.Old
		// 首次子树 push：timeline 尚不存在，但客户端报的 old 是我们广告过的
		// 组合投影 head——验证一致后建立 timeline（head=组合头，base=分叉点），
		// 并以组合头为 expected 继续常规 CAS（docs/01 §5.2）。
		if _, terr := s.Meta.Timeline(r.Context(), tl); terr != nil {
			if !expected.IsZero() {
				if combined, rootHead, cerr := s.subtreeHead(r.Context(), vr); cerr == nil && combined == expected {
					_ = s.Meta.EnsureTimeline(r.Context(), tl, combined, rootHead)
					expected = combined
				}
			}
		}
		if err := s.Meta.CASHead(r.Context(), tl, expected, u.New); err != nil {
			_ = WritePacket(out, []byte("ng "+u.Name+" "+err.Error()+"\n"))
			continue
		}
		_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
		log.Printf("receive-pack %s: %s@%s → %s", vr.Repo, tl.Anchor, name, u.New)

		// ④ 目录锚点 push 触发向上组装（docs/01 §4.2，M1 同步模式）：
		// 子树新树挂回根 → synthetic commit 入根时间线 → 根 head 前进。
		// 上层视图（父目录投影、根仓库）由此看到该提交。
		if !vr.Sub.IsRoot() {
			if aerr := s.assembleSubtreePush(r.Context(), vr, u.New); aerr != nil {
				log.Printf("receive-pack %s: assemble %s: %v", vr.Repo, vr.Sub, aerr)
			}
		}
	}

	_ = WriteFlush(out)
	_ = out.Flush()
}

// assembleSubtreePush 把子树 push 向上组装进根时间线（含 CAS 冲突重试
// 与 no-op 跳过）。合成对象（新根树路径上的中间树 + synthetic commit）
// 物化进合成对象层——"对象存储与提交分离"的复合面（docs/01 §4）。
func (s *Server) assembleSubtreePush(ctx context.Context, vr VirtualRepo, childCommit model.OID) error {
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
		state, terr := s.Meta.Timeline(ctx, rootTL)
		if terr != nil && !errors.Is(terr, storage.ErrTimelineUnknown) {
			return terr
		}
		expected := state.Head // 未知根时间线 → 零值（生成无父初始根 commit）

		syn, aerr := model.AssembleRoot(store, heads, s.Algo, vr.Repo, rootName, vr.Sub, childCommit, serverIdentity())
		if aerr != nil {
			return aerr
		}
		// no-op 跳过：子树状态与根现状一致时不产生空提交。
		if !expected.IsZero() {
			if rootCommit, cerr := store.Commit(expected); cerr == nil && rootCommit.TreeOID == syn.TreeOID {
				return nil
			}
		}
		oid := syn.OID(s.Algo)
		s.Objects.PutSynthetic(vr.Repo, oid, model.CommitType, syn.Encode())
		if err := s.Meta.CASHead(ctx, rootTL, expected, oid); err == nil {
			log.Printf("assemble %s: %s@%s → root %s (synthetic %s)", vr.Repo, vr.Sub, childCommit, rootName, oid)
			return nil
		}
		// 根被并发推进：重读根 head 重算（ReplaceSubtree 幂等）。
	}
	return fmt.Errorf("root head CAS contention after 3 attempts")
}

// metaHeads 把 MetaStore 适配为模型层的 HeadResolver（未知 timeline
// 视作零 head——AssembleRoot 据此生成无父的初始根 commit）。
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


// serverIdentity 是组装 synthetic commit 的签名身份。
func serverIdentity() model.Identity {
	return model.Identity{Name: "banyand", Email: "banyan@banyand.local", When: time.Now().UTC()}
}
