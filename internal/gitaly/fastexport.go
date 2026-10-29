package gitaly

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// Fork, fast-export and the hooks-backup spelling. The snapshot/replicate/
// bundle-URI/backup-sink faces stay Unimplemented on purpose: they belong
// to praefect WAL replication, the bundle CDN and go-cloud sink
// deployments, none of which a single banyan gitaly node fronts (docs/19
// §5-6) — upstream serves the same RPCs unconfigured-failure without them.

// CreateFork seeds a new repository with every reference of the source
// (the object closure copied through the object plane, refs force-set).
func (rs *repositoryService) CreateFork(ctx context.Context, req *gitalypb.CreateForkRequest) (*gitalypb.CreateForkResponse, error) {
	source, err := rs.s.existingRepo(ctx, req.GetSourceRepository())
	if err != nil {
		return nil, err
	}
	if err := rs.createRepositoryChecked(ctx, req.GetRepository()); err != nil {
		return nil, err
	}
	forkName, err := rs.s.repoName(req.GetRepository())
	if err != nil {
		return nil, err
	}
	refs, err := rs.bundleRefsOf(ctx, source)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if !strings.HasPrefix(r.ref, "refs/") || r.ref == "HEAD" {
			continue
		}
		// Annotated tags carry a tag object: copy it too, and walk the
		// peeled commit for the closure.
		if typ, _, err := rs.s.Objects.Raw(ctx, source, r.oid); err == nil && typ == model.TagType {
			if _, raw, err := rs.s.Objects.Raw(ctx, source, r.oid); err == nil {
				tagOID := model.ObjectOID(model.TagType, raw, rs.s.Algo)
				_ = rs.s.Svc.PutObjects(ctx, forkName, []service.PutObject{{
					OID: tagOID, Type: "tag", Data: raw}})
				_ = tagOID
			}
		}
		tip := r.oid
		if typ, _, err := rs.s.Objects.Raw(ctx, source, r.oid); err == nil && typ == model.TagType {
			if peeled, perr := rs.s.peelToCommit(ctx, source, r.oid); perr == nil {
				tip = peeled
			}
		}
		if err := rs.copyClosure(ctx, source, forkName, tip); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if err := rs.s.Svc.UpdateRef(ctx, service.RefUpdate{
			Repo: forkName, Name: r.ref, New: r.oid}); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	return &gitalypb.CreateForkResponse{}, nil
}

// BackupCustomHooks is the backup-sink spelling of GetCustomHooks: the
// same tarball stream under the backup pipe's name.
func (rs *repositoryService) BackupCustomHooks(req *gitalypb.BackupCustomHooksRequest, stream gitalypb.RepositoryService_BackupCustomHooksServer) error {
	ctx := stream.Context()
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	data, found, err := rs.s.Meta.GetRepoBlob(ctx, name, customHooksKey)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if !found || len(data) == 0 {
		return nil
	}
	return streamBytes(data, func(p []byte) error {
		return stream.Send(&gitalypb.BackupCustomHooksResponse{Data: p})
	})
}

// ---- FastExport ----

// FastExport streams the `git fast-export` stream of all heads:
// blob/commit/reset commands with mark numbers, data blocks and M entries
// — the subset git fast-import round-trips (rename and copy commands,
// annotated tags and progress markers stay out).
func (rs *repositoryService) FastExport(req *gitalypb.FastExportRequest, stream gitalypb.RepositoryService_FastExportServer) error {
	ctx := stream.Context()
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	refs, err := rs.bundleRefsOf(ctx, name)
	if err != nil {
		return err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ref < refs[j].ref })
	w := streamWriter(func(p []byte) error {
		return stream.Send(&gitalypb.FastExportResponse{Data: p})
	})
	return rs.writeFastExport(ctx, name, refs, w)
}

// writeFastExport renders the stream: one reset per ref, then commits
// oldest-first with blob marks memoized across refs; parents already
// streamed resolve through the shared mark map.
func (rs *repositoryService) writeFastExport(ctx context.Context, repo string, refs []bundleRef, w io.Writer) error {
	blobMarks := map[model.OID]int{}
	commitMarks := map[model.OID]int{}
	nextMark := 1
	dataBlock := func(payload string) {
		fmt.Fprintf(w, "data %d\n", len(payload))
		_, _ = io.WriteString(w, payload)
	}
	emitBlob := func(oid model.OID) bool {
		if _, ok := blobMarks[oid]; ok {
			return true
		}
		typ, raw, err := rs.s.Objects.Raw(ctx, repo, oid)
		if err != nil || typ != model.BlobType {
			return false
		}
		blobMarks[oid] = nextMark
		nextMark++
		fmt.Fprintf(w, "blob\nmark :%d\n", blobMarks[oid])
		dataBlock(string(raw))
		return true
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref.ref, "refs/heads/") {
			continue // annotated tags and HEAD stay out of the stream subset
		}
		var oids []model.OID
		var bodies []*model.AnchoredCommit
		_ = rs.s.walkParents(ctx, repo, []model.OID{ref.oid}, &walkFilter{}, func(oid model.OID, rec *commitRecord) bool {
			if commitMarks[oid] != 0 {
				return false
			}
			_, raw, err := rs.s.Objects.Raw(ctx, repo, oid)
			if err != nil {
				return true
			}
			c, err := model.DecodeCommit(raw)
			if err != nil {
				return true
			}
			oids = append(oids, oid)
			bodies = append(bodies, c)
			return true
		})
		if len(oids) == 0 {
			continue
		}
		fmt.Fprintf(w, "reset %s\n", ref.ref)
		// walkParents yields newest-first; fast-export wants oldest-first.
		// Blob commands must precede the commit they belong to (fast-import
		// rejects blobs interleaved into a commit's filemodify run), so each
		// commit emits its new blobs first, then the command itself.
		for i := len(oids) - 1; i >= 0; i-- {
			c := bodies[i]
			var mEntries []string
			treeEntries(ctx, rs.s, repo, c.TreeOID, func(path, mode string, oid model.OID) {
				if emitBlob(oid) {
					mEntries = append(mEntries, fmt.Sprintf("M %s :%d %s\n", mode, blobMarks[oid], path))
				}
			})
			commitMarks[oids[i]] = nextMark
			mark := nextMark
			nextMark++
			fmt.Fprintf(w, "commit %s\nmark :%d\n", ref.ref, mark)
			fmt.Fprintf(w, "author %s <%s> %d %s\n",
				c.Author.Name, c.Author.Email, c.Author.When.Unix(), c.Author.When.Format("-0700"))
			fmt.Fprintf(w, "committer %s <%s> %d %s\n",
				c.Committer.Name, c.Committer.Email, c.Committer.When.Unix(), c.Committer.When.Format("-0700"))
			dataBlock(c.Message)
			// Root commits and foreign bases carry no from line; in-chain
			// commits reference the previous (older) mark, and the newest
			// commit of a later ref references its already-streamed parent.
			if len(c.Parents) > 0 {
				if pm, ok := commitMarks[c.Parents[0]]; ok {
					fmt.Fprintf(w, "from :%d\n", pm)
				} else if i < len(oids)-1 {
					fmt.Fprintf(w, "from :%d\n", mark-1)
				}
			}
			for _, m := range mEntries {
				io.WriteString(w, m)
			}
		}
	}
	return nil
}

// treeEntries walks a tree depth-first handing every file to visit.
func treeEntries(ctx context.Context, s *Server, repo string, tree model.OID, visit func(path, mode string, oid model.OID)) {
	lookup := s.Objects.Lookup(repo)
	var walk func(tree model.OID, prefix string)
	walk = func(tree model.OID, prefix string) {
		t, err := lookup.Tree(tree)
		if err != nil {
			return
		}
		for _, e := range t.Entries {
			if e.IsDir {
				walk(e.Target, prefix+e.Name+"/")
				continue
			}
			if e.Mode == "160000" {
				continue // submodule pointers stay out of the export subset
			}
			visit(prefix+e.Name, e.Mode, e.Target)
		}
	}
	walk(tree, "")
}
