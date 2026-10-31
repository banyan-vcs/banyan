package gitaly

import (
	"context"
	"strings"
	"fmt"
	"io"

	"github.com/go-enry/go-enry/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// AnalysisService (docs/19). CheckBlobsGenerated runs the same generated-
// file heuristic upstream uses — go-enry's IsGenerated over the first 2KiB
// of each blob (extension and content markers: lockfiles, minified JS,
// generated-code banners) — answering one row per requested revision.

type analysisService struct {
	gitalypb.UnimplementedAnalysisServiceServer
	s *Server
}

// generatedPeekBytes mirrors upstream's read budget: enough of the file
// for every content heuristic.
const generatedPeekBytes = 2048

func (as *analysisService) CheckBlobsGenerated(stream gitalypb.AnalysisService_CheckBlobsGeneratedServer) error {
	ctx := stream.Context()
	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving first request: %v", err)
	}
	name, err := as.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}

	for {
		if len(req.GetBlobs()) == 0 {
			return status.Error(codes.InvalidArgument, "empty blobs")
		}
		out := &gitalypb.CheckBlobsGeneratedResponse{}
		for _, blob := range req.GetBlobs() {
			rev := blob.GetRevision()
			if len(rev) == 0 || len(blob.GetPath()) == 0 {
				return status.Error(codes.InvalidArgument, "blob revision and path are required")
			}
			oid, rerr := as.resolveBlobRevision(ctx, name, string(rev))
			if rerr != nil {
				return status.Errorf(codes.InvalidArgument, "resolving %q: %v", rev, rerr)
			}
			typ, raw, oerr := as.s.Objects.Raw(ctx, name, oid)
			if oerr != nil || typ != model.BlobType {
				return status.Error(codes.InvalidArgument, "object is not a blob")
			}
			peek := raw
			if len(peek) > generatedPeekBytes {
				peek = peek[:generatedPeekBytes]
			}
			out.Blobs = append(out.Blobs, &gitalypb.CheckBlobsGeneratedResponse_Blob{
				Revision:  rev,
				Generated: enry.IsGenerated(string(blob.GetPath()), peek),
			})
		}
		// One response per request message — upstream's batching contract.
		if err := stream.Send(out); err != nil {
			return err
		}
		req, err = stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

var _ = fmt.Sprintf // guard: keep fmt for future detail errors

// resolveBlobRevision maps the path-scoped revision form upstream's
// validator admits ("<rev>:<path>", e.g. "HEAD:package-lock.json") onto
// the blob OID, falling back to a plain revision for direct blob names.
func (as *analysisService) resolveBlobRevision(ctx context.Context, repo, rev string) (model.OID, error) {
	if i := strings.LastIndexByte(rev, ':'); i > 0 {
		commitOID, err := as.s.resolveRevision(ctx, repo, rev[:i])
		if err != nil {
			return "", err
		}
		rec, err := as.s.commitRecord(ctx, repo, commitOID)
		if err != nil {
			return "", err
		}
		entry, err := as.s.entryAtPath(ctx, repo, rec.Tree, strings.TrimPrefix(rev[i+1:], "/"))
		if err != nil || entry == nil || entry.IsDir {
			return "", fmt.Errorf("path %q not found", rev[i+1:])
		}
		return entry.Target, nil
	}
	return as.s.resolveRevision(ctx, repo, rev)
}
