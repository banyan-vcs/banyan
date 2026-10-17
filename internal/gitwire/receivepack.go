package gitwire

import (
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
			_ = WritePacket(out, []byte("ng "+why+" "+u.Name+"\n"))
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
			_ = WritePacket(out, []byte("ng unsupported ref namespace "+u.Name+"\n"))
			continue
		}
		name := strings.TrimPrefix(u.Name, "refs/heads/")
		if name == "" {
			_ = WritePacket(out, []byte("ng empty branch name "+u.Name+"\n"))
			continue
		}
		if u.New.IsZero() {
			// ref 删除。
			tl := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: name}
			if err := s.Meta.CASHead(r.Context(), tl, u.Old, u.New); err != nil {
				_ = WritePacket(out, []byte("ng "+err.Error()+" "+u.Name+"\n"))
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
					_ = WritePacket(out, []byte("ng head object missing "+u.Name+"\n"))
					continue
				}
			}
		} else if _, _, rerr := s.Objects.Raw(r.Context(), vr.Repo, u.New); rerr != nil {
			_ = WritePacket(out, []byte("ng head object missing "+u.Name+"\n"))
			continue
		}
		tl := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: name}
		if err := s.Meta.CASHead(r.Context(), tl, u.Old, u.New); err != nil {
			_ = WritePacket(out, []byte("ng "+err.Error()+" "+u.Name+"\n"))
			continue
		}

		_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
		log.Printf("receive-pack %s: %s%s → %s (head %s)", vr.Repo, tl.Anchor, name, u.New, u.New)
	}

	_ = WriteFlush(out)
	_ = out.Flush()
}
