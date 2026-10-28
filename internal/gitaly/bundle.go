package gitaly

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Bundle surface: v2 git bundles (header refs + one pack of the closure).
// CreateBundle/FromRefList write them, FetchBundle/FromBundle ingest them —
// the interchange format for backup, seed and offline transfer flows.

const bundleHeaderV2 = "# v2 git bundle"

// bundleRef is one advertised reference line.
type bundleRef struct {
	oid model.OID
	ref string
}

// bundleRefsOf lists refs/heads and refs/tags of the advertised view, sorted
// by ref name with a trailing HEAD line — the exact shape `git bundle create
// --all` writes and clones guess their default branch from.
func (rs *repositoryService) bundleRefsOf(ctx context.Context, repo string) ([]bundleRef, error) {
	refs, err := rs.s.listRefs(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []bundleRef
	var headOID model.OID
	for _, r := range refs {
		switch {
		case r.Name == "HEAD":
			headOID = r.Head
		case strings.HasPrefix(r.Name, "refs/heads/"), strings.HasPrefix(r.Name, "refs/tags/"):
			out = append(out, bundleRef{oid: r.Head, ref: r.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ref < out[j].ref })
	if !headOID.IsZero() {
		out = append(out, bundleRef{oid: headOID, ref: "HEAD"})
	}
	return out, nil
}

// writeBundle streams a complete v2 bundle for the given refs.
func (rs *repositoryService) writeBundle(ctx context.Context, repo string, refs []bundleRef, w io.Writer) error {
	if len(refs) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: refusing to create empty bundle")
	}
	var b strings.Builder
	b.WriteString(bundleHeaderV2 + "\n")
	for _, r := range refs {
		b.WriteString(string(r.oid) + " " + r.ref + "\n")
	}
	b.WriteString("\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	order, err := rs.s.bundleClosure(ctx, repo, refs)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	i := 0
	return storage.StreamEncodePack(w, rs.s.Algo, len(order), func(int) (model.ObjectType, []byte, error) {
		oid := order[i]
		i++
		typ, raw, err := rs.s.Objects.Raw(ctx, repo, oid)
		return typ, raw, err
	})
}

// bundleClosure walks the union closure of the refs' objects (commits,
// trees, blobs and tag objects) in a stable order.
func (s *Server) bundleClosure(ctx context.Context, repo string, refs []bundleRef) ([]model.OID, error) {
	lookup := s.Objects.Lookup(repo)
	var order []model.OID
	seen := map[model.OID]bool{}
	add := func(oid model.OID) {
		if !seen[oid] {
			seen[oid] = true
			order = append(order, oid)
		}
	}
	var addTree func(tree model.OID)
	addTree = func(tree model.OID) {
		t, err := lookup.Tree(tree)
		if err != nil {
			return
		}
		add(tree)
		for _, e := range t.Entries {
			if e.IsDir {
				addTree(e.Target)
				continue
			}
			if e.Mode != "160000" {
				add(e.Target)
			}
		}
	}
	var heads []model.OID
	for _, r := range refs {
		typ, _, err := s.Objects.Raw(ctx, repo, r.oid)
		if err != nil {
			return nil, fmt.Errorf("bundle ref %s: %w", r.ref, err)
		}
		switch typ {
		case model.TagType:
			add(r.oid)
			target, perr := s.peelToCommit(ctx, repo, r.oid)
			if perr == nil {
				heads = append(heads, target)
			}
		case model.CommitType:
			heads = append(heads, r.oid)
		}
	}
	if err := s.walkParents(ctx, repo, heads, &walkFilter{}, func(oid model.OID, rec *commitRecord) bool {
		add(oid)
		addTree(rec.Tree)
		return true
	}); err != nil {
		return nil, err
	}
	return order, nil
}

// CreateBundle streams the whole-repository bundle (all heads and tags).
func (rs *repositoryService) CreateBundle(req *gitalypb.CreateBundleRequest, stream gitalypb.RepositoryService_CreateBundleServer) error {
	ctx := stream.Context()
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	refs, err := rs.bundleRefsOf(ctx, name)
	if err != nil {
		return err
	}
	w := streamWriter(func(p []byte) error {
		return stream.Send(&gitalypb.CreateBundleResponse{Data: p})
	})
	return rs.writeBundle(ctx, name, refs, w)
}

// CreateBundleFromRefList streams a bundle restricted to refs matching the
// client-streamed patterns (git for-each-ref globs).
func (rs *repositoryService) CreateBundleFromRefList(stream gitalypb.RepositoryService_CreateBundleFromRefListServer) error {
	ctx := stream.Context()
	var name string
	var patterns []string
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = rs.s.existingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		}
		for _, p := range req.GetPatterns() {
			patterns = append(patterns, strings.TrimSpace(string(p)))
		}
	}
	if len(patterns) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: no patterns given")
	}
	all, err := rs.bundleRefsOf(ctx, name)
	if err != nil {
		return err
	}
	var matched []bundleRef
	for _, r := range all {
		for _, p := range patterns {
			if globMatch(p, r.ref) {
				matched = append(matched, r)
				break
			}
		}
	}
	w := streamWriter(func(p []byte) error {
		return stream.Send(&gitalypb.CreateBundleFromRefListResponse{Data: p})
	})
	return rs.writeBundle(ctx, name, matched, w)
}

// parseBundleHeader splits a bundle into header refs, prerequisites and the
// pack payload offset.
func parseBundleHeader(data []byte) (refs []bundleRef, prerequisites []model.OID, pack []byte, err error) {
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 || string(data[:nl]) != bundleHeaderV2 {
		if bytes.HasPrefix(data, []byte("# v3 git bundle")) {
			return nil, nil, nil, fmt.Errorf("v3 bundles (capabilities/filters) are not supported")
		}
		return nil, nil, nil, fmt.Errorf("not a v2 git bundle")
	}
	rest := data[nl+1:]
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, nil, nil, fmt.Errorf("bundle header missing blank line")
		}
		line := string(rest[:nl])
		rest = rest[nl+1:]
		if line == "" {
			return refs, prerequisites, rest, nil
		}
		if strings.HasPrefix(line, "-") {
			fields := strings.Fields(line[1:])
			if len(fields) > 0 {
				prerequisites = append(prerequisites, model.OID(fields[0]))
			}
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 || !isHexOID(fields[0]) {
			return nil, nil, nil, fmt.Errorf("malformed bundle ref line %q", line)
		}
		refs = append(refs, bundleRef{oid: model.OID(fields[0]), ref: fields[1]})
	}
}

// ingestBundle validates and applies a bundle into an existing repository.
func (rs *repositoryService) ingestBundle(ctx context.Context, name string, data []byte, updateHead bool) error {
	refs, prerequisites, pack, err := parseBundleHeader(data)
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: "+err.Error())
	}
	for _, pre := range prerequisites {
		if _, _, perr := rs.s.Objects.Raw(ctx, name, pre); perr != nil {
			return status.Errorf(codes.FailedPrecondition,
				"gitaly: repository lacks prerequisite commit %s", pre)
		}
	}
	if len(pack) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: bundle carries no pack")
	}
	dir, err := os.MkdirTemp("", "banyan-bundle-*")
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer os.RemoveAll(dir)
	packPath := filepath.Join(dir, "bundle.pack")
	if err := os.WriteFile(packPath, pack, 0o644); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if _, err := rs.s.Objects.IngestPackFile(ctx, name, packPath, int64(len(pack))); err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: bundle pack rejected: %v", err)
	}
	for _, r := range refs {
		if r.ref == "HEAD" {
			continue
		}
		if !strings.HasPrefix(r.ref, "refs/") {
			continue
		}
		if err := rs.s.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: r.ref, New: r.oid}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	if updateHead {
		for _, r := range refs {
			if r.ref == "HEAD" {
				// The HEAD line's object id is the tip; the default branch
				// name comes from the heads we just wrote that point there.
				tip := r.oid
				for _, h := range refs {
					if strings.HasPrefix(h.ref, "refs/heads/") && h.oid == tip {
						if err := rs.s.Meta.SetDefaultBranch(ctx, name, strings.TrimPrefix(h.ref, "refs/heads/")); err != nil {
							return status.Error(codes.Internal, err.Error())
						}
						break
					}
				}
			}
		}
	}
	return nil
}

// FetchBundle ingests a client-streamed bundle into an existing repository.
func (rs *repositoryService) FetchBundle(stream gitalypb.RepositoryService_FetchBundleServer) error {
	ctx := stream.Context()
	var name string
	var data []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = rs.s.existingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		}
		data = append(data, req.GetData()...)
	}
	if err := rs.ingestBundle(ctx, name, data, false); err != nil {
		return err
	}
	return stream.SendAndClose(&gitalypb.FetchBundleResponse{})
}

// CreateRepositoryFromBundle creates the repository and seeds it from a
// client-streamed bundle in one call.
func (rs *repositoryService) CreateRepositoryFromBundle(stream gitalypb.RepositoryService_CreateRepositoryFromBundleServer) error {
	ctx := stream.Context()
	var name string
	var data []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if err := rs.createRepositoryChecked(ctx, req.GetRepository()); err != nil {
				return err
			}
			name, err = rs.s.repoName(req.GetRepository())
			if err != nil {
				return err
			}
		}
		data = append(data, req.GetData()...)
	}
	// UpdateHead on first seed: bundle HEAD names the default branch.
	if err := rs.ingestBundle(ctx, name, data, true); err != nil {
		_ = rs.s.Objects.DeleteRepo(ctx, name) // don't leave a half-seeded repo behind
		return err
	}
	return stream.SendAndClose(&gitalypb.CreateRepositoryFromBundleResponse{})
}

// createRepositoryChecked mirrors CreateRepository's validation and creation
// for the bundle-seed path.
func (rs *repositoryService) createRepositoryChecked(ctx context.Context, repo *gitalypb.Repository) error {
	name, err := rs.s.repoName(repo)
	if err != nil {
		return err
	}
	if err := rs.s.Meta.CreateRepo(ctx, name, rs.s.Algo); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return status.Errorf(codes.AlreadyExists, "gitaly: repository %q already exists", name)
		}
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}
