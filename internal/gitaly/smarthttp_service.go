package gitaly

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// smartHTTPService bridges workhorse's git-HTTP traffic onto gitwire: the
// same handlers the /r/ endpoints run, invoked through an internal mux with
// recorded responses — one HTTP exchange per gRPC message exchange, the
// stateless-rpc contract both sides speak.
//
// PostUploadPackWithSidechannel (and the SSH sidechannel variants) need the
// yamux multiplexing transport — next slice, docs/19 §5.
type smartHTTPService struct {
	gitalypb.UnimplementedSmartHTTPServiceServer
	s *Server
}

// httpExchange runs one request against the internal gitwire mux.
func (s *Server) httpExchange(method, urlPath, contentType, gitProtocol string, body []byte) (*httptest.ResponseRecorder, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://gitaly.internal"+urlPath, rd)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if gitProtocol != "" {
		req.Header.Set("Git-Protocol", gitProtocol)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec, nil
}

// mappedPath renders the gitwire URL for a mapped repo name.
func mappedPath(name string) string { return "/r/" + name + ".git" }

const wireChunk = 1 << 16 // 64KiB wire chunks: a few messages per pack page

func (sh *smartHTTPService) infoRefs(ctx context.Context, repo *gitalypb.Repository, service, gitProtocol string, send func([]byte) error) error {
	name, err := sh.s.existingRepo(ctx, repo)
	if err != nil {
		return err
	}
	rec, err := sh.s.httpExchange(http.MethodGet, mappedPath(name)+"/info/refs?service="+service, "", gitProtocol, nil)
	if err != nil {
		return err
	}
	if rec.Code != http.StatusOK {
		return status.Errorf(codes.NotFound, "gitaly: %s advertisement failed: %s", service, rec.Body.String())
	}
	return streamBytes(rec.Body.Bytes(), send)
}

func streamBytes(data []byte, send func([]byte) error) error {
	for off := 0; off < len(data); off += wireChunk {
		end := off + wireChunk
		if end > len(data) {
			end = len(data)
		}
		if err := send(data[off:end]); err != nil {
			return err
		}
	}
	return nil
}

func (sh *smartHTTPService) InfoRefsUploadPack(req *gitalypb.InfoRefsRequest, stream gitalypb.SmartHTTPService_InfoRefsUploadPackServer) error {
	return sh.infoRefs(stream.Context(), req.GetRepository(), "git-upload-pack", req.GetGitProtocol(),
		func(b []byte) error { return stream.Send(&gitalypb.InfoRefsResponse{Data: b}) })
}

func (sh *smartHTTPService) InfoRefsReceivePack(req *gitalypb.InfoRefsRequest, stream gitalypb.SmartHTTPService_InfoRefsReceivePackServer) error {
	return sh.infoRefs(stream.Context(), req.GetRepository(), "git-receive-pack", req.GetGitProtocol(),
		func(b []byte) error { return stream.Send(&gitalypb.InfoRefsResponse{Data: b}) })
}

func (sh *smartHTTPService) PostReceivePack(stream gitalypb.SmartHTTPService_PostReceivePackServer) error {
	ctx := stream.Context()
	var name string
	var body []byte
	gitProtocol := ""
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = sh.s.existingRepo(ctx, msg.GetRepository()); err != nil {
				return err
			}
		}
		if gp := msg.GetGitProtocol(); gp != "" {
			gitProtocol = gp
		}
		body = append(body, msg.GetData()...)
	}
	rec, err := sh.s.httpExchange(http.MethodPost, mappedPath(name)+"/git-receive-pack",
		"application/x-git-receive-pack-request", gitProtocol, body)
	if err != nil {
		return err
	}
	if rec.Code != http.StatusOK {
		return status.Errorf(codes.Internal, "gitaly: receive-pack failed: %s", rec.Body.String())
	}
	return streamBytes(rec.Body.Bytes(), func(b []byte) error {
		return stream.Send(&gitalypb.PostReceivePackResponse{Data: b})
	})
}
