package repository

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// GetArchive streams a tree snapshot as tar, tar.gz or zip, byte-compatible
// with `git archive` for the tar flavors (pax_global_header comment, root
// uname/gname, tar_umask 002 modes, 10240-byte record padding). bzip2 needs
// a compressor outside the standard library and stays Unimplemented; LFS
// smudging has no banyan-side storage plane and refuses explicitly.
func (rs *repositoryService) GetArchive(req *gitalypb.GetArchiveRequest, stream gitalypb.RepositoryService_GetArchiveServer) error {
	ctx := stream.Context()
	name, err := rs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	switch req.GetFormat() {
	case gitalypb.GetArchiveRequest_TAR, gitalypb.GetArchiveRequest_TAR_GZ, gitalypb.GetArchiveRequest_ZIP:
	case gitalypb.GetArchiveRequest_TAR_BZ2:
		// bzip2 compression has no standard-library writer; refusing
		// beats a wrong-format answer (see docs/integration/gitaly-compat.md §6).
		return status.Error(codes.Unimplemented, "gitaly: tar.bz2 archives need a bzip2 compressor")
	default:
		return status.Error(codes.InvalidArgument, "gitaly: invalid format")
	}
	commitOID, err := rs.ResolveRevision(ctx, name, req.GetCommitId())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: invalid commitId: %s", req.GetCommitId())
	}
	rec, err := rs.CommitRecord(ctx, name, commitOID)
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: invalid commitId")
	}

	root := strings.Trim(string(req.GetPath()), "/")
	treeOID := rec.Tree
	base := ""
	if root != "" {
		entry, aerr := rs.EntryAtPath(ctx, name, rec.Tree, root)
		if aerr != nil || entry == nil || !entry.IsDir {
			return kit.WithDetail(codes.FailedPrecondition, "gitaly: path doesn't exist",
				&gitalypb.GetArchiveError{Error: &gitalypb.GetArchiveError_ArchivePathNotFound{
					ArchivePathNotFound: &gitalypb.PathNotFoundError{Path: req.GetPath()}}})
		}
		treeOID = entry.Target
		base = root + "/"
	}

	// include_lfs_blobs: GitLab's archive endpoint sends it unconditionally.
	// Without LFS pointers in the tree the flag changes nothing — the honest
	// refusal fires only when a pointer is present and smudging would alter
	// the bytes (docs/integration/gitaly-compat.md §6).
	if req.GetIncludeLfsBlobs() {
		has, aerr := rs.ContainsLFSPointer(ctx, name, treeOID)
		if aerr != nil {
			return aerr
		}
		if has {
			return status.Error(codes.Unimplemented, "gitaly: include_lfs_blobs requires the LFS smudge plane")
		}
	}

	excludes := make([]string, 0, len(req.GetExclude()))
	for i, ex := range req.GetExclude() {
		clean := strings.Trim(string(ex), "/")
		if clean == "" {
			continue
		}
		if !rs.pathUnderCommit(ctx, name, rec.Tree, clean) {
			return kit.WithDetail(codes.FailedPrecondition,
				fmt.Sprintf("gitaly: exclude[%d] doesn't exist", i),
				&gitalypb.GetArchiveError{Error: &gitalypb.GetArchiveError_ExcludePathNotFound{
					ExcludePathNotFound: &gitalypb.GetArchiveError_ExcludePathNotFoundError{
						Index: uint32(i), Path: ex}}})
		}
		excludes = append(excludes, clean)
	}

	entries, err := rs.CollectArchiveEntries(ctx, name, treeOID, base, req.GetElidePath(), excludes)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if len(entries) == 0 && root == "" {
		return kit.WithDetail(codes.FailedPrecondition, "gitaly: path doesn't exist",
			&gitalypb.GetArchiveError{Error: &gitalypb.GetArchiveError_EmptyRootTree{
				EmptyRootTree: &gitalypb.GetArchiveError_EmptyRootTreeError{}}})
	}

	when := time.Unix(rec.When, 0).UTC()
	prefix := req.GetPrefix()
	var out io.Writer = kit.StreamWriter(func(p []byte) error {
		return stream.Send(&gitalypb.GetArchiveResponse{Data: p})
	})
	switch req.GetFormat() {
	case gitalypb.GetArchiveRequest_ZIP:
		return kit.WriteZipArchive(out, entries, prefix, when, name, rs.Base)
	case gitalypb.GetArchiveRequest_TAR_GZ:
		zw := gzip.NewWriter(out)
		zw.Header.OS = 3 // gzip -n matches git's compressor byte for byte
		if err := kit.WriteTarArchive(zw, entries, prefix, when, commitOID, name, rs.Base); err != nil {
			return err
		}
		return zw.Close()
	default:
		return kit.WriteTarArchive(out, entries, prefix, when, commitOID, name, rs.Base)
	}
}

// pathUnderCommit reports whether a cleaned path resolves inside the tree.
func (rs *repositoryService) pathUnderCommit(ctx context.Context, repo string, tree model.OID, path string) bool {
	entry, err := rs.EntryAtPath(ctx, repo, tree, path)
	return err == nil && entry != nil
}
