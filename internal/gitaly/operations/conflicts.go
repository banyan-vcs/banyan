package operations

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/merge"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// Conflicts surface on the 3-way merge engine (internal/merge, the same
// machinery the operation surface rides): listing renders git's conflict
// markers per path, resolving applies user resolutions and lands the merge
// commit on the target branch.

// conflictResolution mirrors upstream's files_json entry shape.
type conflictResolution struct {
	OldPath  string            `json:"old_path"`
	NewPath  string            `json:"new_path"`
	Sections map[string]string `json:"sections"`
	Content  string            `json:"content"`
}

// sidesOf loads the three trees behind a conflict listing: our tip, their
// tip and the merge base.
func (cf *conflictsService) sidesOf(ctx context.Context, repo string, ourOID, theirOID model.OID) (base, ours, theirs model.OID, err error) {
	ourRec, err := cf.CommitRecord(ctx, repo, ourOID)
	if err != nil {
		return "", "", "", err
	}
	theirRec, err := cf.CommitRecord(ctx, repo, theirOID)
	if err != nil {
		return "", "", "", err
	}
	mb, err := cf.MergeBaseOf(ctx, repo, ourOID, theirOID)
	if err != nil {
		return "", "", "", err
	}
	if !mb.IsZero() {
		if rec, err := cf.CommitRecord(ctx, repo, mb); err == nil {
			base = rec.Tree
		}
	}
	return base, ourRec.Tree, theirRec.Tree, nil
}

// ListConflictFiles streams the conflicted paths of our..their with their
// marker-rendered contents (`<<<<<<<` blocks, ours side first).
func (cf *conflictsService) ListConflictFiles(req *gitalypb.ListConflictFilesRequest, stream gitalypb.ConflictsService_ListConflictFilesServer) error {
	ctx := stream.Context()
	name, err := cf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	ourOID := model.OID(strings.ToLower(req.GetOurCommitOid()))
	theirOID := model.OID(strings.ToLower(req.GetTheirCommitOid()))
	base, ours, theirs, err := cf.sidesOf(ctx, name, ourOID, theirOID)
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: unknown commits")
	}
	store := &operationMergeStore{TreeStore: cf.Objects.TreeStoreFor(name), objects: cf.Objects, repo: name}
	_, conflicts, err := merge.Trees(store, cf.Algo, base, ours, theirs, "", merge.Options{})
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	for _, path := range conflicts {
		markerBody := cf.markerBody(ctx, name, base, ours, theirs, path)
		header := &gitalypb.ConflictFileHeader{
			CommitOid:    theirOID.String(),
			TheirPath:    []byte(path),
			OurPath:      []byte(path),
			AncestorPath: []byte(path),
			OurMode:      int32(0o100644),
		}
		if e, err := cf.EntryAtPath(ctx, name, ours, path); err == nil && e != nil {
			header.OurMode = int32(kit.ParseMode(e.Mode))
		}
		if err := stream.Send(&gitalypb.ListConflictFilesResponse{Files: []*gitalypb.ConflictFile{
			{ConflictFilePayload: &gitalypb.ConflictFile_Header{Header: header}},
		}}); err != nil {
			return err
		}
		if req.GetSkipContent() || markerBody == nil {
			continue
		}
		if err := kit.StreamBytes(markerBody, func(p []byte) error {
			return stream.Send(&gitalypb.ListConflictFilesResponse{Files: []*gitalypb.ConflictFile{
				{ConflictFilePayload: &gitalypb.ConflictFile_Content{Content: p}},
			}})
		}); err != nil {
			return err
		}
	}
	return nil
}

// markerBody renders the three sides of one conflicted path with git's
// conflict markers (nil for tree-level conflicts where a side has no blob).
func (cf *conflictsService) markerBody(ctx context.Context, repo string, base, ours, theirs model.OID, path string) []byte {
	blobAt := func(tree model.OID) []byte {
		e, err := cf.EntryAtPath(ctx, repo, tree, path)
		if err != nil || e == nil || e.IsDir {
			return nil
		}
		_, raw, err := cf.Objects.Raw(ctx, repo, e.Target)
		if err != nil {
			return nil
		}
		return raw
	}
	merged, _ := merge.Blobs(blobAt(base), blobAt(ours), blobAt(theirs), merge.Options{})
	return merged
}

// ResolveConflicts applies user resolutions and lands the merge commit:
// automatic merge for untouched paths, resolution contents for the listed
// ones, two-parent commit onto the target branch.
func (cf *conflictsService) ResolveConflicts(stream gitalypb.ConflictsService_ResolveConflictsServer) error {
	ctx := stream.Context()
	var header *gitalypb.ResolveConflictsRequestHeader
	var filesJSON []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch payload := req.GetResolveConflictsRequestPayload().(type) {
		case *gitalypb.ResolveConflictsRequest_Header:
			header = payload.Header
		case *gitalypb.ResolveConflictsRequest_FilesJson:
			filesJSON = payload.FilesJson
		}
	}
	if header == nil {
		return status.Error(codes.InvalidArgument, "gitaly: header is required")
	}
	name, err := cf.ExistingRepo(ctx, header.GetRepository())
	if err != nil {
		return err
	}
	ourOID := model.OID(strings.ToLower(header.GetOurCommitOid()))
	theirOID := model.OID(strings.ToLower(header.GetTheirCommitOid()))

	// Automatic merge first; resolved paths overwrite with user content.
	_, oursTree, _, err := cf.sidesOf(ctx, name, ourOID, theirOID)
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: unknown commits")
	}
	store := &operationMergeStore{TreeStore: cf.Objects.TreeStoreFor(name), objects: cf.Objects, repo: name}
	mergedTree, conflicts, err := merge.Trees(store, cf.Algo, cf.baseTreeOf(ctx, name, ourOID, theirOID), oursTree, cf.treeOf(ctx, name, theirOID), "", merge.Options{})
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	conflictSet := map[string]bool{}
	for _, p := range conflicts {
		conflictSet[p] = true
	}
	var resolutions []conflictResolution
	if err := json.Unmarshal(filesJSON, &resolutions); err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: files_json is malformed")
	}
	resolved := map[string]bool{}
	overrides := map[string]model.OID{}
	for _, r := range resolutions {
		path := r.OldPath
		if path == "" {
			path = r.NewPath
		}
		if path == "" {
			continue
		}
		content := r.Content
		if len(r.Sections) > 0 {
			marker := cf.markerBody(ctx, name, cf.baseTreeOf(ctx, name, ourOID, theirOID), oursTree, cf.treeOf(ctx, name, theirOID), path)
			content, err = resolveSections(marker, r.Sections)
			if err != nil {
				_ = stream.SendAndClose(&gitalypb.ResolveConflictsResponse{ResolutionError: err.Error()})
				return nil
			}
		}
		blob := model.ObjectOID(model.BlobType, []byte(content), cf.Algo)
		if err := cf.Svc.PutObjects(ctx, name, []service.PutObject{{
			OID: blob, Type: "blob", Data: []byte(content)}}); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		overrides[path] = blob
		resolved[path] = true
	}
	for p := range conflictSet {
		if !resolved[p] {
			_ = stream.SendAndClose(&gitalypb.ResolveConflictsResponse{
				ResolutionError: "unresolved conflict: " + p})
			return nil
		}
	}

	// The resolved tree: the merged tree (ours side at conflicted paths)
	// with every override applied, then a two-parent merge commit onto the
	// target branch of the target repository (ours repository when absent).
	newTree, err := cf.overlayTree(ctx, name, mergedTree, overrides)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	c := &model.AnchoredCommit{
		Anchor:    model.RootAnchor,
		TreeOID:   newTree,
		Parents:   []model.OID{ourOID, theirOID},
		Author:    identityFromUser(header.GetUser(), header.GetTimestamp()),
		Committer: identityFromUser(header.GetUser(), header.GetTimestamp()),
		Message:   ensureNewline(string(header.GetCommitMessage())),
	}
	commitOID := model.ObjectOID(model.CommitType, c.Encode(), cf.Algo)
	if err := cf.Svc.PutObjects(ctx, name, []service.PutObject{{
		OID: commitOID, Type: "commit", Data: c.Encode()}}); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	targetRepo := name
	if t := header.GetTargetRepository(); t != nil {
		if targetRepo, err = cf.RepoName(t); err != nil {
			return err
		}
	}
	targetRef := string(header.GetTargetBranch())
	if targetRef == "" {
		targetRef = "refs/heads/main"
	}
	if !strings.HasPrefix(targetRef, "refs/") {
		targetRef = "refs/heads/" + targetRef
	}
	if err := cf.Svc.UpdateRef(ctx, service.RefUpdate{
		Repo: targetRepo, Name: targetRef, New: commitOID}); err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
	}
	return stream.SendAndClose(&gitalypb.ResolveConflictsResponse{})
}

// identityFromUser maps the gitaly User onto a commit identity.
func identityFromUser(u *gitalypb.User, ts *timestamppb.Timestamp) model.Identity {
	id := model.Identity{Name: "banyan", Email: "banyan@local"}
	if u != nil {
		if len(u.GetName()) > 0 {
			id.Name = string(u.GetName())
		}
		if len(u.GetEmail()) > 0 {
			id.Email = string(u.GetEmail())
		}
	}
	if ts != nil && ts.AsTime().Unix() > 0 {
		id.When = ts.AsTime()
	} else {
		id.When = time.Now()
	}
	return id
}

func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// overlayTree returns treeOID with every override path rewritten to its
// blob (writing the intermediate trees as objects).
func (cf *conflictsService) overlayTree(ctx context.Context, repo string, treeOID model.OID, overrides map[string]model.OID) (model.OID, error) {
	if len(overrides) == 0 {
		return treeOID, nil
	}
	var rebuild func(tree model.OID, prefix string) (model.OID, bool, error)
	rebuild = func(tree model.OID, prefix string) (model.OID, bool, error) {
		t, err := cf.Objects.Lookup(repo).Tree(tree)
		if err != nil {
			return "", false, err
		}
		changed := false
		for i := range t.Entries {
			e := &t.Entries[i]
			path := prefix + e.Name
			switch {
			case overrides[path] != "" && !e.IsDir:
				e.Target = overrides[path]
				changed = true
			case e.IsDir:
				sub, did, err := rebuild(e.Target, path+"/")
				if err != nil {
					return "", false, err
				}
				if did {
					e.Target = sub
					changed = true
				}
			}
		}
		if !changed {
			return tree, false, nil
		}
		enc, err := t.Encode()
		if err != nil {
			return "", false, err
		}
		oid := model.ObjectOID(model.TreeType, enc, cf.Algo)
		if err := cf.Svc.PutObjects(ctx, repo, []service.PutObject{{
			OID: oid, Type: "tree", Data: enc}}); err != nil {
			return "", false, err
		}
		return oid, true, nil
	}
	newRoot, _, err := rebuild(treeOID, "")
	return newRoot, err
}

// resolveSections picks head/origin sides per conflict block of a marked
// body (git's conflict markers, ours side first).
func resolveSections(marked []byte, sections map[string]string) (string, error) {
	if marked == nil {
		return "", status.Error(codes.InvalidArgument, "resolve: no marked body")
	}
	var out strings.Builder
	id := 0
	lines := strings.SplitAfter(string(marked), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimRight(line, "\n")
		if trimmed == "<<<<<<<" || strings.HasPrefix(trimmed, "<<<<<<< ") {
			id++
			side := sections[strconv.Itoa(id)]
			var head, origin []string
			cur := &head
			closed := false
			for i++; i < len(lines); i++ {
				t := strings.TrimRight(lines[i], "\n")
				if t == "=======" {
					cur = &origin
					continue
				}
				if t == ">>>>>>>" || strings.HasPrefix(t, ">>>>>>> ") {
					closed = true
					break
				}
				*cur = append(*cur, lines[i])
			}
			if !closed {
				return "", status.Error(codes.InvalidArgument, "resolve: unterminated conflict block")
			}
			switch side {
			case "head":
				for _, l := range head {
					out.WriteString(l)
				}
			case "origin":
				for _, l := range origin {
					out.WriteString(l)
				}
			default:
				return "", status.Errorf(codes.InvalidArgument, "resolve: unknown side for section %d", id)
			}
			continue
		}
		out.WriteString(line)
	}
	return out.String(), nil
}

// baseTreeOf and treeOf fetch the merge-base and side trees for the
// section resolver.
func (cf *conflictsService) baseTreeOf(ctx context.Context, repo string, a, b model.OID) model.OID {
	base, _, _, err := cf.sidesOf(ctx, repo, a, b)
	if err != nil {
		return ""
	}
	return base
}

func (cf *conflictsService) treeOf(ctx context.Context, repo string, oid model.OID) model.OID {
	rec, err := cf.CommitRecord(ctx, repo, oid)
	if err != nil {
		return ""
	}
	return rec.Tree
}

type conflictsService struct {
	gitalypb.UnimplementedConflictsServiceServer
	*kit.Base
}
