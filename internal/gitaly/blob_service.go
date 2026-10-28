package gitaly

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// blobService implements gitaly.BlobService's read slice: GetBlob streams a
// single blob; the enumeration RPCs (ListBlobs, LFS pointers) are later
// slices over the same object plane.
type blobService struct {
	gitalypb.UnimplementedBlobServiceServer
	s *Server
}

// blobChunk is the wire chunking upstream clients accept for blob data.
const blobChunk = 1 << 20

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
	for off := 0; off < len(data); off += blobChunk {
		end := off + blobChunk
		if end > len(data) {
			end = len(data)
		}
		if err := stream.Send(&gitalypb.GetBlobResponse{Data: data[off:end]}); err != nil {
			return err
		}
	}
	return nil
}
