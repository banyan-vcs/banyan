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

// serveReceivePack serves push (docs/03 §5, write path docs/02 §4).
//
// v1 session (mirrors receive-pack.c):
//	client POST body:
//	  <old> <new> <refname> [caps]     ← ref command lines, capabilities trail the first one
//	  ...
//	  flush
//	  packfile bytes (delete-only pushes carry no pack section)
//	server:
//	  ① pack validation and ingest (chunked CAS, not disassembled)
//	  ② verify the new head is in the pack and is a commit
//	  ③ head CAS (all-zero old = create, all-zero new = delete; failure reports non-fast-forward)
//	  ④ report-status receipt
func (s *Server) serveReceivePack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	// Demo-friendly: auto-create the repo when pushing to a missing repo (M2 will tighten to ACL policy).
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
		// The capability block trails the first command line as "\x00<caps>"; truncate before parsing.
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

	// Pack section: after the flush until body end (empty for delete-only pushes).
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

	// ① pack decode + chunked ingest (error if no pack yet non-delete updates exist).
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

	// ②③ per-ref validation + head CAS. report-status mandates the order:
	// the first pkt is "unpack ok", followed by per-ref statuses (mirrors receive-pack.c).
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
			// ref deletion: root repo only (subtree views are derived, no separate timeline).
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
		// The new head must be in this pack and be a commit (for the re-push case where
		// the object is already in a pack: also accept already-ingested objects — global-index fallback).
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
			// ④ subtree-view push: adopt into the unified DAG (docs/01 §10) — create the canonical
			// subtree-merge commit (parents=[root head, C as-is]); C's hash
			// stays consistent across subtree and parent views; the root head advances and the view tip follows.
			if aerr := s.adoptSubtreePush(r.Context(), vr, name, u.Old, u.New); aerr != nil {
				_ = WritePacket(out, []byte("ng "+u.Name+" "+aerr.Error()+"\n"))
				continue
			}
			_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
			continue
		}

		// Root repo: regular timeline CAS.
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

// adoptSubtreePush is the v4 write path for subtree pushes (docs/01 §12 timeline-first):
//
//	① content-level check (race-window guard, unchanged): admit only when old's X-view tree == the current merged
//	  head's X-view tree — consecutive pushes admitted / stale cross-view rejected (prevents silent rollback) /
//	  post-pull admitted;
//	② subtree timeline CAS advance: the first push anchors via EnsureTimeline
//	  (head=merged head, base=root divergence point); later pushes use the current local head as expected;
//	③ AdoptSubtreePush adopts into the root (C kept as-is as second parent, root head CAS
//	  advances, retry on contention). Post-adoption the projection maps back to C itself —
//	  the pusher's changes keep their own SHA after flowing back, and pull is always fast-forward.
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

	// ② subtree timeline CAS. First push: EnsureTimeline anchors at the merged head
	// (head=curTip, base=root divergence point); expected is that anchored head.
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

	// ③ adopt into the root.
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
		// Root advanced concurrently: reread the root head and recompute (the merge's C side is unchanged).
		state, terr := s.Meta.Timeline(ctx, rootTL)
		if terr != nil {
			return terr
		}
		rootHead = state.Head
	}
	return fmt.Errorf("root head CAS contention after 3 attempts")
}

// metaHeads adapts MetaStore to the model layer's HeadResolver (unknown timeline
// treated as zero head — adoption uses this to make C the sole parent).
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

// sameXView is the content-level fast-forward check: whether two commits have the same
// tree under the path-X view (X-view normalization: a full-repo tree maps to its X subtree; a tree lacking X's first segment ⇒ the tree itself
// is the X-view root tree — the mixed scope of subtree-side commits / view tips).
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

// xviewTree normalizes a tree to the X view (docs/01 §10 mixed scope).
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
	return tree // tree lacks X's first segment ⇒ it is itself the X-view root tree
}
