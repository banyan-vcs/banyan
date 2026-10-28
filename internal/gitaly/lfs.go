package gitaly

import (
	"bytes"
	"context"
	"regexp"
	"sort"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// LFS pointers (docs/19). A pointer is a tiny text blob naming the LFS spec
// with oid/size lines carrying the big object's identity. Upstream gates
// candidates at 200 bytes before parsing and silently drops non-pointer
// objects (internal/git/lfs.go + service/blob/lfs_pointers.go, v19.5.2).
const lfsPointerMaxSize = 200

var (
	lfsOIDRe  = regexp.MustCompile(`(?m)^oid sha256:([0-9a-f]{64})$`)
	lfsSizeRe = regexp.MustCompile(`(?m)^size ([0-9]+)$`)
)

// parseLFSPointer mirrors gitaly's git.IsLFSPointer: spec-version prefix plus
// multiline oid/size lines; fileOid is the bare hex digest (no "sha256:").
func parseLFSPointer(b []byte) (fileOid []byte, fileSize int64, ok bool) {
	if !bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec")) {
		return nil, 0, false
	}
	oid := lfsOIDRe.FindSubmatch(b)
	if len(oid) != 2 {
		return nil, 0, false
	}
	sz := lfsSizeRe.FindSubmatch(b)
	if len(sz) != 2 {
		return nil, 0, false
	}
	n, err := strconv.ParseInt(string(sz[1]), 10, 64)
	if err != nil {
		return nil, 0, false
	}
	out := make([]byte, len(oid[1]))
	copy(out, oid[1])
	return out, n, true
}

// lfsPointerBatch is the chunked-pointer sender shared by the three RPCs.
const lfsPointerBatch = 100

// GetLFSPointers filters the requested blob IDs down to valid LFS pointers.
// Non-pointer objects (including trees and commits) answer with silence, per
// upstream; a missing object is an error.
func (bs *blobService) GetLFSPointers(req *gitalypb.GetLFSPointersRequest, stream gitalypb.BlobService_GetLFSPointersServer) error {
	ctx := stream.Context()
	name, err := bs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	if len(req.GetBlobIds()) == 0 {
		return status.Error(codes.InvalidArgument, "empty BlobIds")
	}
	batch := make([]*gitalypb.LFSPointer, 0, lfsPointerBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out := batch
		batch = make([]*gitalypb.LFSPointer, 0, lfsPointerBatch)
		return stream.Send(&gitalypb.GetLFSPointersResponse{LfsPointers: out})
	}
	for _, id := range req.GetBlobIds() {
		p, err := bs.lfsPointerFromBlob(ctx, name, model.OID(id))
		if err != nil {
			return status.Errorf(codes.NotFound, "gitaly: object %s not found", id)
		}
		if p == nil {
			continue
		}
		batch = append(batch, p)
		if len(batch) >= lfsPointerBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// ListLFSPointers finds all pointers transitively reachable from the given
// revisions — the full commit history, so pointers deleted at the tip still
// answer (rev-list --objects semantics).
func (bs *blobService) ListLFSPointers(req *gitalypb.ListLFSPointersRequest, stream gitalypb.BlobService_ListLFSPointersServer) error {
	ctx := stream.Context()
	name, err := bs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	if len(req.GetRevisions()) == 0 {
		return status.Error(codes.InvalidArgument, "missing revisions")
	}
	blobs, err := bs.walkHistoryBlobs(ctx, name, req.GetRevisions(), req.GetLimit())
	if err != nil {
		return err
	}
	return bs.sendPointers(ctx, name, blobs, req.GetLimit(),
		func(ps []*gitalypb.LFSPointer) error {
			return stream.Send(&gitalypb.ListLFSPointersResponse{LfsPointers: ps})
		})
}

// ListAllLFSPointers enumerates pointers over every object in the database —
// including unreachable ones (cat-file --batch-all-objects semantics).
func (bs *blobService) ListAllLFSPointers(req *gitalypb.ListAllLFSPointersRequest, stream gitalypb.BlobService_ListAllLFSPointersServer) error {
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
	sent := 0
	limit := req.GetLimit()
	for _, oid := range oids {
		if limit > 0 && int32(sent) >= limit {
			break
		}
		typ, raw, err := bs.s.Objects.Raw(ctx, name, oid)
		if err != nil || typ != model.BlobType || int64(len(raw)) > lfsPointerMaxSize {
			continue
		}
		blobs = append(blobs, listedBlob{oid: oid, size: int64(len(raw))})
		sent++
	}
	return bs.sendPointers(ctx, name, blobs, 0,
		func(ps []*gitalypb.LFSPointer) error {
			return stream.Send(&gitalypb.ListAllLFSPointersResponse{LfsPointers: ps})
		})
}

// sendPointers parses the enumerated blobs and streams the valid ones in
// batches; limit caps the emitted pointer count when positive.
func (bs *blobService) sendPointers(ctx context.Context, repo string, blobs []listedBlob, limit int32, send func([]*gitalypb.LFSPointer) error) error {
	batch := make([]*gitalypb.LFSPointer, 0, lfsPointerBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out := batch
		batch = make([]*gitalypb.LFSPointer, 0, lfsPointerBatch)
		return send(out)
	}
	n := 0
	for _, b := range blobs {
		if limit > 0 && int32(n) >= limit {
			break
		}
		p, err := bs.lfsPointerFromBlob(ctx, repo, b.oid)
		if err != nil || p == nil {
			continue
		}
		batch = append(batch, p)
		n++
		if len(batch) >= lfsPointerBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// lfsPointerFromBlob loads one OID as a pointer: nil means "not a pointer"
// (wrong type or oversized); a hard miss propagates as an error.
func (bs *blobService) lfsPointerFromBlob(ctx context.Context, repo string, oid model.OID) (*gitalypb.LFSPointer, error) {
	typ, raw, err := bs.s.Objects.Raw(ctx, repo, oid)
	if err != nil {
		return nil, err
	}
	if typ != model.BlobType || int64(len(raw)) > lfsPointerMaxSize {
		return nil, nil
	}
	fileOid, fileSize, ok := parseLFSPointer(raw)
	if !ok {
		return nil, nil
	}
	data := make([]byte, len(raw))
	copy(data, raw)
	return &gitalypb.LFSPointer{
		Size: int64(len(data)), Data: data, Oid: oid.String(),
		FileOid: fileOid, FileSize: fileSize,
	}, nil
}

// allObjectOIDs enumerates every registered object of the repo, sorted by OID
// — the same ordered view `git cat-file --batch-all-objects` prints.
func (bs *blobService) allObjectOIDs(ctx context.Context, repo string) ([]model.OID, error) {
	packs, err := bs.s.Meta.ListPacks(ctx, repo)
	if err != nil {
		return nil, err
	}
	set := map[model.OID]bool{}
	for _, p := range packs {
		oids, err := bs.s.Meta.PackObjects(ctx, repo, p.PackOID)
		if err != nil {
			return nil, err
		}
		for _, o := range oids {
			set[o] = true
		}
	}
	out := make([]model.OID, 0, len(set))
	for o := range set {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
