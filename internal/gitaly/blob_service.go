package gitaly

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// blobService implements gitaly.BlobService: GetBlob streams a single blob;
// GetBlobs resolves revision:path pairs; ListBlobs/ListAllBlobs enumerate a
// revision's full-history blobs (rev-list --objects) or the whole object
// database (batch-all-objects); the LFS-pointer trio rides the same
// enumeration with pointer parsing (lfs.go).
type blobService struct {
	gitalypb.UnimplementedBlobServiceServer
	s *Server
}

func (bs *blobService) GetBlob(req *gitalypb.GetBlobRequest, stream gitalypb.BlobService_GetBlobServer) error {
	ctx := stream.Context()
	name, err := bs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	typ, raw, err := bs.s.Objects.Raw(ctx, name, model.OID(req.GetOid()))
	if err != nil {
		return status.Errorf(codes.NotFound, "gitaly: blob %s not found", req.GetOid())
	}
	if typ != model.BlobType {
		return status.Errorf(codes.InvalidArgument, "gitaly: object %s is a %s, not a blob", req.GetOid(), typ)
	}
	data := raw
	if req.GetLimit() > 0 && int64(len(data)) > req.GetLimit() {
		data = data[:req.GetLimit()]
	}
	if err := stream.Send(&gitalypb.GetBlobResponse{Oid: req.GetOid(), Size: int64(len(raw))}); err != nil {
		return err
	}
	return streamBytes(data, func(b []byte) error {
		return stream.Send(&gitalypb.GetBlobResponse{Data: b})
	})
}

// GetBlobs answers revision:path pairs with each blob's metadata and
// chunked content; misses answer with an empty message.
func (bs *blobService) GetBlobs(req *gitalypb.GetBlobsRequest, stream gitalypb.BlobService_GetBlobsServer) error {
	ctx := stream.Context()
	name, err := bs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	for _, rp := range req.GetRevisionPaths() {
		if err := bs.sendBlobAt(ctx, stream, name, rp, req.GetLimit()); err != nil {
			return err
		}
	}
	return nil
}

func (bs *blobService) sendBlobAt(ctx context.Context, stream gitalypb.BlobService_GetBlobsServer, repo string, rp *gitalypb.GetBlobsRequest_RevisionPath, limit int64) error {
	out := &gitalypb.GetBlobsResponse{Path: rp.GetPath()}
	oid, err := bs.s.resolveRevision(ctx, repo, string(rp.GetRevision()))
	if err != nil {
		return stream.Send(out)
	}
	rec, err := bs.s.commitRecord(ctx, repo, oid)
	if err != nil {
		return stream.Send(out)
	}
	tree := rec.Tree
	lookup := bs.s.Objects.Lookup(repo)
	var entry *model.TreeEntry
	for _, seg := range splitPath(string(rp.GetPath())) {
		t, err := lookup.Tree(tree)
		if err != nil {
			return stream.Send(out)
		}
		entry = nil
		for i := range t.Entries {
			if t.Entries[i].Name == seg {
				entry = &t.Entries[i]
				break
			}
		}
		if entry == nil {
			return stream.Send(out)
		}
		if !entry.IsDir {
			break
		}
		tree = entry.Target
	}
	if entry == nil {
		return stream.Send(out)
	}
	if entry.Mode == "160000" {
		out.IsSubmodule = true
		out.Oid = entry.Target.String()
		return stream.Send(out)
	}
	if entry.IsDir {
		return stream.Send(out)
	}
	typ, raw, err := bs.s.Objects.Raw(ctx, repo, entry.Target)
	if err != nil || typ != model.BlobType {
		return stream.Send(out)
	}
	data := raw
	if limit > 0 && int64(len(raw)) > limit {
		data = raw[:limit]
	}
	out.Oid = entry.Target.String()
	out.Size = int64(len(raw))
	out.Mode = modeValue(entry.Mode)
	if err := stream.Send(out); err != nil {
		return err
	}
	return streamBytes(data, func(b []byte) error {
		return stream.Send(&gitalypb.GetBlobsResponse{Data: b})
	})
}

// splitPath splits a slash path into clean segments.
func splitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

// listedBlob is one enumerated blob (dedup key = OID).
type listedBlob struct {
	oid  model.OID
	path string
	size int64
}

// walkHistoryBlobs enumerates every blob reachable from the revisions' full
// commit history (rev-list --objects semantics): shared subtrees are walked
// once (tree memo) and each blob answers at its first occurrence, so files
// deleted at the tip still list. Unresolvable revisions error out — callers
// decide the wire code.
func (bs *blobService) walkHistoryBlobs(ctx context.Context, repo string, revisions []string, limit int32) ([]listedBlob, error) {
	var heads []model.OID
	for _, rev := range revisions {
		oid, err := bs.s.resolveRevision(ctx, repo, rev)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "gitaly: revision %q not found", rev)
		}
		heads = append(heads, oid)
	}
	lookup := bs.s.Objects.Lookup(repo)
	seenBlobs := map[model.OID]bool{}
	seenTrees := map[model.OID]bool{}
	out := []listedBlob{}
	var emit func(tree model.OID, prefix string)
	emit = func(tree model.OID, prefix string) {
		if seenTrees[tree] {
			return
		}
		seenTrees[tree] = true
		t, err := lookup.Tree(tree)
		if err != nil {
			return // unreadable subtree: report what is readable
		}
		for _, e := range t.Entries {
			switch {
			case e.IsDir:
				emit(e.Target, prefix+e.Name+"/")
			case e.Mode == "160000":
				continue
			default:
				if seenBlobs[e.Target] {
					continue
				}
				seenBlobs[e.Target] = true
				size := int64(0)
				if typ, raw, err := bs.s.Objects.Raw(ctx, repo, e.Target); err == nil && typ == model.BlobType {
					size = int64(len(raw))
				}
				out = append(out, listedBlob{oid: e.Target, path: prefix + e.Name, size: size})
			}
		}
	}
	var walkErr error
	err := bs.s.walkHistory(ctx, repo, heads, func(_ model.OID, rec *commitRecord) bool {
		emit(rec.Tree, "")
		return !(limit > 0 && int32(len(out)) >= limit)
	})
	if err != nil {
		walkErr = status.Errorf(codes.Internal, "gitaly: walking history: %v", err)
	}
	if limit > 0 && int32(len(out)) >= limit {
		out = out[:limit]
	}
	return out, walkErr
}

// ListBlobs enumerates blobs reachable from the given revisions. Contents
// stream when the blob fits the byte budget; larger blobs carry metadata
// only (clients fetching them do so via GetBlob).
func (bs *blobService) ListBlobs(req *gitalypb.ListBlobsRequest, stream gitalypb.BlobService_ListBlobsServer) error {
	ctx := stream.Context()
	name, err := bs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	if len(req.GetRevisions()) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: revisions is empty")
	}
	blobs, err := bs.walkHistoryBlobs(ctx, name, req.GetRevisions(), int32(req.GetLimit()))
	if err != nil {
		return err
	}
	return bs.sendListed(ctx, name, blobs, req.GetBytesLimit(), req.GetWithPaths(),
		func(oid, path string, size int64, data []byte) error {
			return stream.Send(&gitalypb.ListBlobsResponse{Blobs: []*gitalypb.ListBlobsResponse_Blob{{
				Oid: oid, Size: size, Data: data, Path: []byte(path),
			}}})
		})
}

// ListAllBlobs enumerates every blob in the repository — including
// unreachable ones (cat-file --batch-all-objects semantics), OID-sorted.
func (bs *blobService) ListAllBlobs(req *gitalypb.ListAllBlobsRequest, stream gitalypb.BlobService_ListAllBlobsServer) error {
	ctx := stream.Context()
	name, err := bs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	oids, err := bs.allObjectOIDs(ctx, name)
	if err != nil {
		return err
	}
	blobs := make([]listedBlob, 0, len(oids))
	for _, oid := range oids {
		if req.GetLimit() > 0 && uint32(len(blobs)) >= req.GetLimit() {
			break
		}
		typ, raw, err := bs.s.Objects.Raw(ctx, name, oid)
		if err != nil || typ != model.BlobType {
			continue
		}
		blobs = append(blobs, listedBlob{oid: oid, size: int64(len(raw))})
	}
	return bs.sendListed(ctx, name, blobs, req.GetBytesLimit(), false,
		func(oid, path string, size int64, data []byte) error {
			return stream.Send(&gitalypb.ListAllBlobsResponse{Blobs: []*gitalypb.ListAllBlobsResponse_Blob{{
				Oid: oid, Size: size, Data: data,
			}}})
		})
}

func (bs *blobService) sendListed(ctx context.Context, repo string, blobs []listedBlob, bytesLimit int64, withPaths bool, send func(oid, path string, size int64, data []byte) error) error {
	for _, b := range blobs {
		var data []byte
		if bytesLimit > 0 && b.size <= bytesLimit {
			if typ, raw, err := bs.s.Objects.Raw(ctx, repo, b.oid); err == nil && typ == model.BlobType {
				data = raw
			}
		}
		path := ""
		if withPaths {
			path = b.path
		}
		if err := send(b.oid.String(), path, b.size, data); err != nil {
			return err
		}
	}
	return nil
}
