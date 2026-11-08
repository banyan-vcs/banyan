package commit

import (
	"context"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// commitService implements the read slice of gitaly.CommitService: commit
// lookup, ancestry, history walks and tree/blob listing. Blame, signature
// and language-analysis RPCs are later slices (docs/19).
type commitService struct {
	gitalypb.UnimplementedCommitServiceServer
	*kit.Base
}

func (cs *commitService) FindCommit(ctx context.Context, req *gitalypb.FindCommitRequest) (*gitalypb.FindCommitResponse, error) {
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	oid, err := cs.ResolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		// Upstream answers a missing revision with an absent commit, not an
		// error — Rails branches on Commit == nil.
		return &gitalypb.FindCommitResponse{}, nil
	}
	commit, err := cs.GitCommit(ctx, name, oid)
	if err != nil {
		return &gitalypb.FindCommitResponse{}, nil
	}
	return &gitalypb.FindCommitResponse{Commit: commit}, nil
}

func (cs *commitService) CommitIsAncestor(ctx context.Context, req *gitalypb.CommitIsAncestorRequest) (*gitalypb.CommitIsAncestorResponse, error) {
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	ok, err := cs.IsAncestor(ctx, name, model.OID(strings.ToLower(req.GetAncestorId())), model.OID(strings.ToLower(req.GetChildId())))
	if err != nil {
		return nil, err
	}
	return &gitalypb.CommitIsAncestorResponse{Value: ok}, nil
}

func (cs *commitService) ListCommits(req *gitalypb.ListCommitsRequest, stream gitalypb.CommitService_ListCommitsServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	filter, heads, err := cs.walkQuery(ctx, name, req.GetRevisions(), req.GetFirstParent(), req.GetMaxParents(),
		req.GetAfter(), req.GetBefore(), string(req.GetAuthor()))
	if err != nil {
		return err
	}
	limit, offset := kit.PaginationWindow(req.GetPaginationParams())
	sent, skipped := uint32(0), uint32(0)
	var errOut error
	err = cs.WalkParents(ctx, name, heads, filter, func(oid model.OID, rec *kit.CommitRecord) bool {
		if !filter.Matches(oid, rec, cs.rawCommit(ctx, name)) {
			return true
		}
		if skipped < offset {
			skipped++
			return true
		}
		if limit > 0 && sent >= limit {
			return false
		}
		commit, cerr := cs.GitCommit(ctx, name, oid)
		if cerr != nil {
			errOut = cerr
			return false
		}
		if serr := stream.Send(&gitalypb.ListCommitsResponse{Commits: []*gitalypb.GitCommit{commit}}); serr != nil {
			errOut = serr
			return false
		}
		sent++
		return true
	})
	if errOut != nil {
		return errOut
	}
	return err
}

func (cs *commitService) ListAllCommits(req *gitalypb.ListAllCommitsRequest, stream gitalypb.CommitService_ListAllCommitsServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	refs, err := cs.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	var heads []model.OID
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "refs/heads/") {
			heads = append(heads, r.Head)
		}
	}
	limit, offset := kit.PaginationWindow(req.GetPaginationParams())
	sent, skipped := uint32(0), uint32(0)
	filter := &kit.WalkFilter{}
	var errOut error
	err = cs.WalkParents(ctx, name, heads, filter, func(oid model.OID, rec *kit.CommitRecord) bool {
		if skipped < offset {
			skipped++
			return true
		}
		if limit > 0 && sent >= limit {
			return false
		}
		commit, cerr := cs.GitCommit(ctx, name, oid)
		if cerr != nil {
			errOut = cerr
			return false
		}
		if serr := stream.Send(&gitalypb.ListAllCommitsResponse{Commits: []*gitalypb.GitCommit{commit}}); serr != nil {
			errOut = serr
			return false
		}
		sent++
		return true
	})
	if errOut != nil {
		return errOut
	}
	return err
}

// walkQuery parses revision lists: one positive revision plus optional
// "^rev" exclusions, the subset of git rev-list syntax Rails sends here.
func (cs *commitService) walkQuery(ctx context.Context, repo string, revisions []string, firstParent bool, maxParents uint32, after, before *timestamppb.Timestamp, author string) (*kit.WalkFilter, []model.OID, error) {
	filter := &kit.WalkFilter{FirstParent: firstParent, MaxParents: maxParents, Author: author}
	if after != nil {
		filter.After = after.AsTime().Unix()
	}
	if before != nil {
		filter.Before = before.AsTime().Unix()
	}
	var heads []model.OID
	for _, rev := range revisions {
		if strings.HasPrefix(rev, "^") {
			oid, err := cs.ResolveRevision(ctx, repo, rev[1:])
			if err != nil {
				return nil, nil, err
			}
			if filter.Exclude == nil {
				filter.Exclude = map[model.OID]bool{}
			}
			filter.Exclude[oid] = true
			continue
		}
		oid, err := cs.ResolveRevision(ctx, repo, rev)
		if err != nil {
			return nil, nil, err
		}
		heads = append(heads, oid)
	}
	if len(heads) == 0 {
		return nil, nil, status.Error(codes.InvalidArgument, "gitaly: revisions require at least one positive revision")
	}
	return filter, heads, nil
}

// rawCommit loads the full decoded commit when filters need identity data.
func (s *commitService) rawCommit(ctx context.Context, repo string) func(model.OID) (*model.AnchoredCommit, bool) {
	return func(oid model.OID) (*model.AnchoredCommit, bool) {
		typ, raw, err := s.Objects.Raw(ctx, repo, oid)
		if err != nil || typ != model.CommitType {
			return nil, false
		}
		c, err := model.DecodeCommit(raw)
		return c, err == nil
	}
}

func (cs *commitService) CountCommits(ctx context.Context, req *gitalypb.CountCommitsRequest) (*gitalypb.CountCommitsResponse, error) {
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	heads := []model.OID{}
	if oid, rerr := cs.ResolveRevision(ctx, name, string(req.GetRevision())); rerr == nil {
		heads = append(heads, oid)
	}
	filter := &kit.WalkFilter{}
	if req.GetAfter() != nil {
		filter.After = req.GetAfter().AsTime().Unix()
	}
	if req.GetBefore() != nil {
		filter.Before = req.GetBefore().AsTime().Unix()
	}
	count := int32(0)
	raw := cs.rawCommit(ctx, name)
	err = cs.WalkParents(ctx, name, heads, filter, func(oid model.OID, rec *kit.CommitRecord) bool {
		if filter.Matches(oid, rec, raw) {
			count++
		}
		if req.GetMaxCount() > 0 && count >= req.GetMaxCount() {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return &gitalypb.CountCommitsResponse{Count: count}, nil
}

func (cs *commitService) GetTreeEntries(req *gitalypb.GetTreeEntriesRequest, stream gitalypb.CommitService_GetTreeEntriesServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	oid, err := cs.ResolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return err
	}
	entries, err := cs.treeEntries(ctx, name, oid, string(req.GetPath()), req.GetRecursive())
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return stream.Send(&gitalypb.GetTreeEntriesResponse{Entries: entries})
	}
	return nil
}

func (cs *commitService) treeEntries(ctx context.Context, repo string, commit model.OID, path string, recursive bool) ([]*gitalypb.TreeEntry, error) {
	rec, err := cs.CommitRecord(ctx, repo, commit)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "gitaly: commit %s not found", commit)
	}
	tree := rec.Tree
	// Paths arrive in git form: Rails lists the tree root as "." — clean
	// segments (splitPath drops "." and empties) so that is the root walk.
	segs := kit.SplitPath(path)
	base := strings.Join(segs, "/")
	if base != "" {
		lookup := cs.Objects.Lookup(repo)
		for _, seg := range segs {
			t, err := lookup.Tree(tree)
			if err != nil {
				return nil, status.Errorf(codes.NotFound, "gitaly: path %q not found", path)
			}
			var next *model.TreeEntry
			for i := range t.Entries {
				if t.Entries[i].Name == seg {
					next = &t.Entries[i]
					break
				}
			}
			if next == nil {
				return nil, status.Errorf(codes.NotFound, "gitaly: path %q not found", path)
			}
			if !next.IsDir {
				return nil, status.Errorf(codes.InvalidArgument, "gitaly: path %q is not a directory", path)
			}
			tree = next.Target
		}
	}
	lookup := cs.Objects.Lookup(repo)
	var out []*gitalypb.TreeEntry
	prefix := ""
	if base != "" {
		prefix = base + "/"
	}
	expand := func(treeOID model.OID, prefix string) error {
		t, err := lookup.Tree(treeOID)
		if err != nil {
			return status.Errorf(codes.NotFound, "gitaly: tree %s not found", treeOID)
		}
		for _, e := range t.Entries {
			out = append(out, &gitalypb.TreeEntry{
				Oid:       e.Target.String(),
				Path:      []byte(prefix + e.Name),
				FlatPath:  []byte(e.Name),
				Type:      entryType(e),
				Mode:      kit.ModeValue(e.Mode),
				CommitOid: commit.String(),
			})
		}
		return nil
	}
	if err := expand(tree, prefix); err != nil {
		return nil, err
	}
	if recursive {
		for i := 0; i < len(out); i++ {
			if out[i].Type != gitalypb.TreeEntry_TREE {
				continue
			}
			if err := expand(model.OID(out[i].Oid), string(out[i].Path)+"/"); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func entryType(e model.TreeEntry) gitalypb.TreeEntry_EntryType {
	switch {
	case e.IsDir:
		return gitalypb.TreeEntry_TREE
	case e.Mode == "160000":
		return gitalypb.TreeEntry_COMMIT
	default:
		return gitalypb.TreeEntry_BLOB
	}
}

func (cs *commitService) ListFiles(req *gitalypb.ListFilesRequest, stream gitalypb.CommitService_ListFilesServer) error {
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
	lookup := cs.Objects.Lookup(name)
	var batch [][]byte
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out := batch
		batch = nil
		return stream.Send(&gitalypb.ListFilesResponse{Paths: out})
	}
	var walk func(tree model.OID, prefix string) error
	walk = func(tree model.OID, prefix string) error {
		t, err := lookup.Tree(tree)
		if err != nil {
			return nil // pruned subtree: report what is readable
		}
		for _, e := range t.Entries {
			switch {
			case e.IsDir:
				if err := walk(e.Target, prefix+e.Name+"/"); err != nil {
					return err
				}
			case e.Mode == "160000":
				continue // submodule pointers are not files
			default:
				batch = append(batch, []byte(prefix+e.Name))
				if len(batch) >= 500 {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(rec.Tree, ""); err != nil {
		return err
	}
	return flush()
}

func (cs *commitService) ListCommitsByOid(req *gitalypb.ListCommitsByOidRequest, stream gitalypb.CommitService_ListCommitsByOidServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	for _, oid := range req.GetOid() {
		commit, err := cs.GitCommit(ctx, name, model.OID(strings.ToLower(oid)))
		if err != nil {
			continue // absent OIDs are skipped, per upstream contract
		}
		if err := stream.Send(&gitalypb.ListCommitsByOidResponse{Commits: []*gitalypb.GitCommit{commit}}); err != nil {
			return err
		}
	}
	return nil
}

func (cs *commitService) GetCommitMessages(req *gitalypb.GetCommitMessagesRequest, stream gitalypb.CommitService_GetCommitMessagesServer) error {
	ctx := stream.Context()
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	for _, cid := range req.GetCommitIds() {
		typ, raw, err := cs.Objects.Raw(ctx, name, model.OID(strings.ToLower(cid)))
		if err != nil || typ != model.CommitType {
			continue
		}
		c, err := model.DecodeCommit(raw)
		if err != nil {
			continue
		}
		if err := stream.Send(&gitalypb.GetCommitMessagesResponse{CommitId: cid, Message: []byte(c.Message)}); err != nil {
			return err
		}
	}
	return nil
}

// CheckObjectsExist is bidirectional: revisions stream in, existences stream
// back in order.
func (cs *commitService) CheckObjectsExist(stream gitalypb.CommitService_CheckObjectsExistServer) error {
	ctx := stream.Context()
	var name string
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = cs.ExistingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		}
		for _, rev := range req.GetRevisions() {
			exists := true
			if oid, rerr := cs.ResolveRevision(ctx, name, string(rev)); rerr != nil {
				exists = false
			} else if _, _, oerr := cs.Objects.Raw(ctx, name, oid); oerr != nil {
				exists = false
			}
			if serr := stream.Send(&gitalypb.CheckObjectsExistResponse{Revisions: []*gitalypb.CheckObjectsExistResponse_RevisionExistence{{Name: rev, Exists: exists}}}); serr != nil {
				return serr
			}
		}
	}
}
