package commit

// Path-scoped history RPCs (docs/19 slice 4): FindCommits, CommitStats,
// TreeEntry, CommitsByMessage, LastCommitForPath, ListLastCommitsForTree —
// all built on entryAtPath/pathTouched over the pure-Go history walk.

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitaly/object"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// FindCommits: revision walk filtered by pathspecs (path-following
// history). Pagination via limit/offset; DisableWalk answers just the
// revision's own commit.
func (cs *commitService) FindCommits(req *gitalypb.FindCommitsRequest, stream gitalypb.CommitService_FindCommitsServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	rev := string(req.GetRevision())
	if rev == "" {
		rev = "HEAD"
	}
	head, err := cs.ResolveRevision(ctx, name, rev)
	if err != nil {
		return nil // nothing reachable: empty stream
	}
	if req.GetDisableWalk() {
		commit, err := cs.GitCommit(ctx, name, head)
		if err == nil {
			return stream.Send(&gitalypb.FindCommitsResponse{Commits: []*gitalypb.GitCommit{commit}})
		}
		return nil
	}
	filter := kit.PathFilter(req.GetPaths())
	sent, skipped := int32(0), int32(0)
	limit := req.GetLimit()
	var errOut error
	err = cs.WalkParents(ctx, name, []model.OID{head}, &kit.WalkFilter{}, func(oid model.OID, rec *kit.CommitRecord) bool {
		if len(filter) > 0 {
			touched := false
			for _, p := range filter {
				if cs.PathTouched(ctx, name, rec, string(p)) {
					touched = true
					break
				}
			}
			if !touched {
				return true
			}
		}
		if skipped < req.GetOffset() {
			skipped++
			return true
		}
		if limit > 0 && sent >= limit {
			return false
		}
		commit, cerr := cs.GitCommit(ctx, name, oid)
		if cerr != nil {
			return true
		}
		if serr := stream.Send(&gitalypb.FindCommitsResponse{Commits: []*gitalypb.GitCommit{commit}}); serr != nil {
			errOut = serr
			return false
		}
		sent++
		return true
	})
	return errOut
}

// CommitStats: additions/deletions of one commit against its first parent.
func (cs *commitService) CommitStats(ctx context.Context, req *gitalypb.CommitStatsRequest) (*gitalypb.CommitStatsResponse, error) {
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	head, err := cs.ResolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return nil, err
	}
	rec, err := cs.CommitRecord(ctx, name, head)
	if err != nil {
		return nil, err
	}
	parentTree := model.OID("")
	if len(rec.Parents) > 0 {
		if pr, err := cs.CommitRecord(ctx, name, rec.Parents[0]); err == nil {
			parentTree = pr.Tree
		}
	}
	changes, err := cs.TreeDiff(ctx, name, parentTree, rec.Tree)
	if err != nil {
		return nil, err
	}
	out := &gitalypb.CommitStatsResponse{Oid: head.String()}
	for _, c := range changes {
		added, removed := kit.PatchStats(object.BlobContent(ctx, cs.Base, name, c.OldOID), object.BlobContent(ctx, cs.Base, name, c.NewOID))
		out.Additions += added
		out.Deletions += removed
	}
	out.Files = int32(len(changes))
	return out, nil
}

// TreeEntry: one entry's metadata plus (for blobs) chunked content.
func (cs *commitService) TreeEntry(req *gitalypb.TreeEntryRequest, stream gitalypb.CommitService_TreeEntryServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	oid, err := cs.ResolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return err
	}
	rec, err := cs.CommitRecord(ctx, name, oid)
	if err != nil {
		return err
	}
	entry, err := cs.EntryAtPath(ctx, name, rec.Tree, string(req.GetPath()))
	if err != nil {
		return err
	}
	if entry == nil {
		// Upstream answers NotFound for absent paths; Rails' blob finder
		// rescues GRPC::NotFound into nil. An empty stream instead leaves
		// its entry nil and crashes on `entry.data=` — every project tree
		// page probes .git-blame-ignore-revs, so this 500'd the whole UI.
		return status.Error(codes.NotFound, "tree entry not found")
	}
	switch {
	case entry.IsDir:
		return stream.Send(&gitalypb.TreeEntryResponse{Type: gitalypb.TreeEntryResponse_TREE, Oid: entry.Target.String(), Mode: kit.ModeValue(entry.Mode)})
	case entry.Mode == "160000":
		return stream.Send(&gitalypb.TreeEntryResponse{Type: gitalypb.TreeEntryResponse_COMMIT, Oid: entry.Target.String(), Mode: kit.ModeValue(entry.Mode)})
	default:
		typ, raw, err := cs.Objects.Raw(ctx, name, entry.Target)
		if err != nil || typ != model.BlobType {
			return nil
		}
		if err := stream.Send(&gitalypb.TreeEntryResponse{
			Type: gitalypb.TreeEntryResponse_BLOB, Oid: entry.Target.String(),
			Size: int64(len(raw)), Mode: kit.ModeValue(entry.Mode)}); err != nil {
			return err
		}
		data := raw
		if l := req.GetLimit(); l > 0 && int64(len(data)) > l {
			data = data[:l]
		}
		return kit.StreamBytes(data, func(b []byte) error {
			return stream.Send(&gitalypb.TreeEntryResponse{Data: b})
		})
	}
}

// CommitsByMessage: history walk filtered by message substring.
func (cs *commitService) CommitsByMessage(req *gitalypb.CommitsByMessageRequest, stream gitalypb.CommitService_CommitsByMessageServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	rev := string(req.GetRevision())
	if rev == "" {
		rev = "HEAD"
	}
	head, err := cs.ResolveRevision(ctx, name, rev)
	if err != nil {
		return nil // nothing reachable: empty stream
	}
	raw := cs.rawCommit(ctx, name)
	sent, skipped := int32(0), int32(0)
	limit := req.GetLimit()
	var errOut error
	err = cs.WalkParents(ctx, name, []model.OID{head}, &kit.WalkFilter{}, func(oid model.OID, rec *kit.CommitRecord) bool {
		if c, ok := raw(oid); ok && strings.Contains(c.Message, req.GetQuery()) {
			if skipped < req.GetOffset() {
				skipped++
				return true
			}
			if limit > 0 && sent >= limit {
				return false
			}
			commit, cerr := cs.GitCommit(ctx, name, oid)
			if cerr != nil {
				return true
			}
			if serr := stream.Send(&gitalypb.CommitsByMessageResponse{Commits: []*gitalypb.GitCommit{commit}}); serr != nil {
				errOut = serr
				return false
			}
			sent++
		}
		return true
	})
	return errOut
}

// LastCommitForPath: the newest commit whose tree changed the path.
func (cs *commitService) LastCommitForPath(ctx context.Context, req *gitalypb.LastCommitForPathRequest) (*gitalypb.LastCommitForPathResponse, error) {
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	head, err := cs.ResolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return nil, err
	}
	var found *gitalypb.GitCommit
	_ = cs.WalkParents(ctx, name, []model.OID{head}, &kit.WalkFilter{}, func(oid model.OID, rec *kit.CommitRecord) bool {
		if cs.PathTouched(ctx, name, rec, string(req.GetPath())) {
			if c, err := cs.GitCommit(ctx, name, oid); err == nil {
				found = c
			}
			return false
		}
		return true
	})
	if found == nil {
		return nil, status.Error(codes.NotFound, "gitaly: no commit touches the path")
	}
	return &gitalypb.LastCommitForPathResponse{Commit: found}, nil
}

// ListLastCommitsForTree: the last commit per top-level entry under a path.
func (cs *commitService) ListLastCommitsForTree(req *gitalypb.ListLastCommitsForTreeRequest, stream gitalypb.CommitService_ListLastCommitsForTreeServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	head, err := cs.ResolveRevision(ctx, name, req.GetRevision())
	if err != nil {
		return err
	}
	rec, err := cs.CommitRecord(ctx, name, head)
	if err != nil {
		return err
	}
	treeOID := rec.Tree
	base := ""
	if p := strings.Trim(string(req.GetPath()), "/"); p != "" {
		entry, err := cs.EntryAtPath(ctx, name, rec.Tree, p)
		if err != nil || entry == nil || !entry.IsDir {
			return nil
		}
		treeOID = entry.Target
		base = p + "/"
	}
	t, err := cs.Objects.Lookup(name).Tree(treeOID)
	if err != nil {
		return nil
	}
	sent, skipped := int32(0), int32(0)
	limit := req.GetLimit()
	for _, e := range t.Entries {
		if skipped < req.GetOffset() {
			skipped++
			continue
		}
		if limit > 0 && sent >= limit {
			break
		}
		path := base + e.Name
		var commit *gitalypb.GitCommit
		_ = cs.WalkParents(ctx, name, []model.OID{head}, &kit.WalkFilter{}, func(oid model.OID, r *kit.CommitRecord) bool {
			if cs.PathTouched(ctx, name, r, path) {
				if c, err := cs.GitCommit(ctx, name, oid); err == nil {
					commit = c
				}
				return false
			}
			return true
		})
		if commit == nil {
			continue
		}
		if err := stream.Send(&gitalypb.ListLastCommitsForTreeResponse{
			Commits: []*gitalypb.ListLastCommitsForTreeResponse_CommitForTree{{
				Commit: commit, PathBytes: []byte(path),
			}}}); err != nil {
			return err
		}
		sent++
	}
	return nil
}
