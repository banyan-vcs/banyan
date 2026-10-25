package gitwire

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// serveReceivePack serves push (docs/04 ÃÂ§5, write path docs/03 ÃÂ§4).
//
// v1 session (mirrors receive-pack.c):
//
//	client POST body:
//	  <old> <new> <refname> [caps]     Ã¢ÂÂ ref command lines, capabilities trail the first one
//	  ...
//	  flush
//	  packfile bytes (delete-only pushes carry no pack section)
//	server:
//	  Ã¢ÂÂ  pack validation and ingest (chunked CAS, not disassembled)
//	  Ã¢ÂÂ¡ verify the new head is in the pack and is a commit
//	  Ã¢ÂÂ¢ head CAS (all-zero old = create, all-zero new = delete; failure reports non-fast-forward)
//	  Ã¢ÂÂ£ report-status receipt
func (s *Server) serveReceivePack(w http.ResponseWriter, r *http.Request, vr VirtualRepo) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	id, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	// Demo-friendly: auto-create the repo when pushing to a missing repo —
	// the creation itself requires the push permission under authorization.
	if _, err := s.Meta.GetRepo(r.Context(), vr.Repo); err != nil {
		if s.Auth != nil && !s.Auth.CanPush(id, vr.Repo, string(vr.Sub), "") {
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}
		if err := s.Meta.CreateRepo(r.Context(), vr.Repo, s.Algo); err != nil {
			http.Error(w, "create repo: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("receive-pack: auto-created repo %q (object-format=%d)", vr.Repo, s.Algo)
	}

	br := bufio.NewReaderSize(r.Body, 256*1024)
	out := newBufWriter(w)
	if err := s.finishPush(r.Context(), id, br, out, vr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = out.Flush()
}

// finishPush serves the receive-pack data phase shared by smart HTTP and the
// SSH transport (docs/04 §5, write path docs/03 §4): read the ref commands
// and pack, then per-ref validation + head CAS with a report-status receipt.
// A non-nil error is a pre-report protocol failure.
func (s *Server) finishPush(ctx context.Context, id auth.Identity, br *bufio.Reader, out *bufWriter, vr VirtualRepo) error {
	type refUpdate struct {
		Old, New model.OID
		Name     string
	}
	var updates []refUpdate
	sawFlush := false
	for !sawFlush {
		line, flush, err := ReadPacketText(br)
		if err != nil {
			return fmt.Errorf("read ref commands: %w", err)
		}
		if flush {
			sawFlush = true
			break
		}
		// The capability block trails the first command line as "\x00<caps>"; truncate before parsing.
		cmd, _, _ := strings.Cut(line, "\x00")
		fields := strings.Fields(cmd)
		if len(fields) != 3 {
			return fmt.Errorf("malformed ref update: %s", line)
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
		return fmt.Errorf("read pack: %w", err)
	}

	fail := func(why string) {
		_ = WritePacket(out, []byte("unpack "+why+"\n"))
		for _, u := range updates {
			_ = WritePacket(out, []byte("ng "+u.Name+" "+why+"\n"))
		}
		_ = WriteFlush(out)
	}

	// ① pack decode + chunked ingest (error if no pack yet non-delete updates exist).
	var dp *storage.DecodedPack
	if len(packBytes) > 0 {
		dp, err = storage.DecodePack(packBytes)
		if err != nil {
			fail("pack decode: " + err.Error())
			return nil
		}
		if _, err := s.Objects.IngestPack(ctx, vr.Repo, packBytes); err != nil {
			fail("ingest: " + err.Error())
			return nil
		}
	}

	// ②③ per-ref validation + head CAS. report-status mandates the order:
	// the first pkt is "unpack ok", followed by per-ref statuses (mirrors receive-pack.c).
	_ = WritePacket(out, []byte("unpack ok\n"))
	for _, u := range updates {
		// Ref namespaces: refs/heads/* are branch timelines (weave on read);
		// any other refs/<ns>/<name> (tags, notes, custom) is an auxiliary
		// ref — root repo only, stored verbatim, plain CAS (docs/04 §6).
		if !strings.HasPrefix(u.Name, "refs/") || !strings.Contains(strings.TrimPrefix(u.Name, "refs/"), "/") {
			_ = WritePacket(out, []byte("ng "+u.Name+" unsupported ref namespace\n"))
			continue
		}
		if !strings.HasPrefix(u.Name, "refs/heads/") {
			if s.Auth != nil && !s.Auth.CanPush(id, vr.Repo, string(vr.Sub), "") {
				_ = WritePacket(out, []byte("ng "+u.Name+" permission denied\n"))
				continue
			}
			s.handleAuxRefUpdate(ctx, out, vr, u.Old, u.New, u.Name, dp)
			continue
		}
		name := strings.TrimPrefix(u.Name, "refs/heads/")
		if s.Auth != nil && !s.Auth.CanPush(id, vr.Repo, string(vr.Sub), name) {
			_ = WritePacket(out, []byte("ng "+u.Name+" permission denied\n"))
			continue
		}
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
			expected := u.Old
			if st, serr := s.Meta.Timeline(ctx, tl); serr == nil {
				expected = st.Head // the advertised (derived) tip maps to stored state
			}
			if err := s.Meta.CASHead(ctx, tl, expected, u.New); err != nil {
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
				if _, _, rerr := s.Objects.Raw(ctx, vr.Repo, u.New); rerr != nil {
					_ = WritePacket(out, []byte("ng "+u.Name+" head object missing\n"))
					continue
				}
			}
		} else if _, _, rerr := s.Objects.Raw(ctx, vr.Repo, u.New); rerr != nil {
			_ = WritePacket(out, []byte("ng "+u.Name+" head object missing\n"))
			continue
		}

		if !vr.Sub.IsRoot() {
			// Ⓑ subtree-view push (docs/02 §13): the write path touches ONLY
			// this directory's timeline — no root head CAS, no adoption merge.
			// The root view is derived on read (view.RootTip weave).
			// Under M4 convergence (docs/15) the update queues on the
			// per-timeline FIFO and divergent chains converge server-side;
			// otherwise the pre-M4 adopt path runs inline.
			if s.Convergence {
				res := s.runSubtreeJob(ctx, vr, name, u.Old, u.New, dp)
				if res.err != nil {
					_ = WritePacket(out, []byte("ng "+u.Name+" "+res.err.Error()+"\n"))
					continue
				}
				_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
				s.warmView(vr.Repo, vr.Sub, name)
				continue
			}
			if aerr := s.adoptSubtreePush(ctx, vr, name, u.Old, u.New); aerr != nil {
				_ = WritePacket(out, []byte("ng "+u.Name+" "+aerr.Error()+"\n"))
				continue
			}
			_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
			s.warmView(vr.Repo, vr.Sub, name)
			continue
		}

		// Root repo: the client chain lands VERBATIM as root-local history
		// (docs/02 §13) — its boundary parent is the advertised woven tip,
		// which the weave persists via the /.woven timeline, so the chain is
		// self-consistent root-local state; the incremental weave then
		// identity-reuses those commits (same tree, same parents).
		_ = dp
		tl := model.Timeline{Repo: vr.Repo, Anchor: model.RootAnchor, Name: name}
		expected := u.Old
		if st, serr := s.Meta.Timeline(ctx, tl); serr == nil {
			// Race-window guard: the client's old is the advertised woven tip;
			// it must still equal the tip derivable now, else another push
			// landed in between (fetch first, git-equivalent semantics).
			if curWoven, werr := s.Views.RootTip(ctx, vr.Repo, name); werr == nil &&
				!u.Old.IsZero() && !curWoven.IsZero() && u.Old != curWoven {
				_ = WritePacket(out, []byte("ng "+u.Name+" "+storage.ErrNonFastForward.Error()+"\n"))
				continue
			}
			expected = st.Head
		}
		if err := s.Meta.CASHead(ctx, tl, expected, u.New); err != nil {
			_ = WritePacket(out, []byte("ng "+u.Name+" "+err.Error()+"\n"))
			continue
		}
		_ = WritePacket(out, []byte("ok "+u.Name+"\n"))
		s.warmView(vr.Repo, model.RootAnchor, name)
		log.Printf("receive-pack %s: /%s → %s", vr.Repo, name, u.New)
	}

	_ = WriteFlush(out)
	return nil
}

// warmView pre-materializes the views the next advertisement will resolve
// after a successful push (docs/11 P2-3, B6): a push only CASes timeline
// heads — the woven root/subtree views fold lazily on the FIRST subsequent
// clone's ref discovery. Warming right after the CAS moves that fold off
// the read path. Best-effort and asynchronous: the push response never
// waits, and the view engine is idempotent + CAS-safe against concurrent
// materialization, so a racing push is a benign lost update.
func (s *Server) warmView(repo string, sub model.AnchorPath, branch string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if sub.IsRoot() {
			_, _ = s.Views.RootTip(ctx, repo, branch)
			return
		}
		// A subtree push lands in two views: its own subtree view and the
		// root weave that folds descendant timelines in.
		_, _, _ = s.Views.SubtreeHead(ctx, repo, sub, branch)
		_, _ = s.Views.RootTip(ctx, repo, branch)
	}()
}

// handleAuxRefUpdate stores refs/tags/* and any other non-heads namespace
// (docs/04 §6): a fixed pointer to an existing server-side object —
// annotated tags point at tag objects, lightweight tags at commits. The
// timeline name IS the full ref name; the head is the raw OID with no
// derived-view semantics; deletion mirrors the branch path's expected
// mapping (the advertised value equals the stored head).
func (s *Server) handleAuxRefUpdate(ctx context.Context, out *bufWriter, vr VirtualRepo, old, newHead model.OID, refName string, dp *storage.DecodedPack) {
	if !vr.Sub.IsRoot() {
		_ = WritePacket(out, []byte("ng "+refName+" auxiliary refs only supported in the root repo\n"))
		return
	}
	if !newHead.IsZero() {
		// Any object type is a valid target (unlike branch heads).
		known := dp != nil && (dp.Commits[newHead] != nil || dp.Tags[newHead] != nil ||
			dp.Trees[newHead] != nil || dp.Blobs[newHead] != nil)
		if !known {
			if _, _, rerr := s.Objects.Raw(ctx, vr.Repo, newHead); rerr != nil {
				_ = WritePacket(out, []byte("ng "+refName+" target object missing\n"))
				return
			}
		}
	}
	tl := model.Timeline{Repo: vr.Repo, Anchor: model.RootAnchor, Name: refName}
	expected := old
	if st, serr := s.Meta.Timeline(ctx, tl); serr == nil {
		expected = st.Head
	}
	if err := s.Meta.CASHead(ctx, tl, expected, newHead); err != nil {
		_ = WritePacket(out, []byte("ng "+refName+" "+err.Error()+"\n"))
		return
	}
	_ = WritePacket(out, []byte("ok "+refName+"\n"))
	log.Printf("receive-pack %s: %s → %s", vr.Repo, refName, newHead)
}

// adoptSubtreePush is the v5 write path for subtree pushes (docs/02 ÃÂ§13):
//
//	Ã¢ÂÂ  content-level check (race-window guard, unchanged): admit only when old's X-view tree == the current merged
//	  head's X-view tree Ã¢ÂÂ consecutive pushes admitted / stale cross-view rejected (prevents silent rollback) /
//	  post-pull admitted;
//	Ã¢ÂÂ¡ subtree timeline CAS advance: the first push anchors via EnsureTimeline
//	  (head=merged head, base=root divergence point); later pushes use the current local head as expected.
//
// No root-side write: the root view is derived on read (view.RootTip), so a
// subtree push contends on exactly one CAS Ã¢ÂÂ its own directory's timeline.
func (s *Server) adoptSubtreePush(ctx context.Context, vr VirtualRepo, branch string, old, newHead model.OID) error {
	curTip, rootLocal, err := s.Views.SubtreeHead(ctx, vr.Repo, vr.Sub, branch)
	if err != nil {
		return err
	}
	if curTip.IsZero() {
		return fmt.Errorf("subtree view %s unavailable (root has no commits)", vr.Sub)
	}
	// Content-level staleness guard, both sides (docs/02 §13):
	//   local side — old must match the combined view tip X-content
	//     (consecutive pushes pass; a divergent peer local push rejects —
	//     the v4 race-window rule);
	//   root side — old must CONTAIN the current projection tip: by OID
	//     ancestry, by identical X content, or by the projection's X-tree
	//     already recorded in old's history. The last form matters after a
	//     rebase recovery: the client's chain holds a cross-view REWRITE of
	//     the projection (different OID) plus newer local content, so it is
	//     ahead of, not stale against, the root side.
	if !old.IsZero() {
		log.Printf("GUARD %s%s: old=%s curTip=%s same=%v", vr.Repo, vr.Sub, old, curTip, s.sameXView(vr, old, curTip))
		if !s.sameXView(vr, old, curTip) {
			return storage.ErrNonFastForward
		}
		if projTip, perr := s.Views.ProjectionTip(vr.Repo, vr.Sub, rootLocal); perr == nil &&
			!projTip.IsZero() && !s.Views.Descends(vr.Repo, old, projTip) && !s.sameXView(vr, old, projTip) {
			projTree := model.OID("")
			if pci, ciErr := s.Objects.CommitInfo(vr.Repo, projTip); ciErr == nil {
				projTree = pci.Tree
			}
			if projTree.IsZero() || !s.Views.TreeKnown(vr.Repo, old, projTree) {
				return storage.ErrNonFastForward
			}
		}
	}

	// Ã¢ÂÂ¡ subtree timeline CAS. First push: EnsureTimeline anchors at the merged head
	// (head=curTip, base=root divergence point); expected is that anchored head.
	localTL := model.Timeline{Repo: vr.Repo, Anchor: vr.Sub, Name: branch}
	localTip := curTip
	if st, terr := s.Meta.Timeline(ctx, localTL); terr == nil {
		localTip = st.Head
	} else if terr := s.Meta.EnsureTimeline(ctx, localTL, curTip, s.rootLocalHead(ctx, vr.Repo, branch)); terr != nil {
		return terr
	}
	if err := s.Meta.CASHead(ctx, localTL, localTip, newHead); err != nil {
		return err
	}
	log.Printf("subtree %s%s: /%s Ã¢ÂÂ %s (root-local untouched)", vr.Repo, vr.Sub, branch, newHead)
	return nil
}

func (s *Server) rootLocalHead(ctx context.Context, repo, branch string) model.OID {
	state, err := s.Meta.Timeline(ctx, model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch})
	if err != nil {
		return ""
	}
	return state.Head
}

// sameXView is the content-level fast-forward check: whether two commits have the same
// tree under the path-X view (X-view normalization: a full-repo tree maps to its X subtree; a tree lacking X's first segment Ã¢ÂÂ the tree itself
// is the X-view root tree Ã¢ÂÂ the mixed scope of subtree-side commits / view tips).
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

// xviewTree normalizes a tree to the X view (docs/02 ÃÂ§10 mixed scope).
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
	return tree // tree lacks X's first segment Ã¢ÂÂ it is itself the X-view root tree
}
