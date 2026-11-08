package transport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/gitwire"
)

// smartHTTPService bridges workhorse's git-HTTP traffic onto gitwire: the
// same handlers the /r/ endpoints run, invoked through an internal mux with
// recorded responses — one HTTP exchange per gRPC message exchange, the
// stateless-rpc contract both sides speak. The sidechannel variant moves
// the pack bytes onto a yamux stream instead of gRPC messages.
type smartHTTPService struct {
	gitalypb.UnimplementedSmartHTTPServiceServer
	*kit.Base
}

// PostUploadPackWithSidechannel runs the fetch exchange over a yamux stream
// dialed back to the client (docs/19 §5): the request body arrives on the
// sidechannel (the client half-closes after it), the pack response returns
// over the same stream. The gRPC response itself carries only statistics.
func (sh *smartHTTPService) PostUploadPackWithSidechannel(ctx context.Context, req *gitalypb.PostUploadPackWithSidechannelRequest) (*gitalypb.PostUploadPackWithSidechannelResponse, error) {
	name, err := sh.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	raw, err := openSidechannel(ctx)
	if err != nil {
		return nil, err
	}
	conn := newFramedConn(raw)
	defer conn.Close()
	body, err := io.ReadAll(conn) // the client's flush frame ends the request
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: read fetch request: %v", err)
	}
	rec, err := sh.httpExchange(http.MethodPost, mappedPath(name)+"/git-upload-pack",
		"application/x-git-upload-pack-request", req.GetGitProtocol(), body, nil)
	if err != nil {
		return nil, err
	}
	if rec.Code != http.StatusOK {
		return nil, status.Errorf(codes.Internal, "gitaly: upload-pack failed: %s", rec.Body.String())
	}
	if _, err := conn.Write(rec.Body.Bytes()); err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: write pack response: %v", err)
	}
	if err := conn.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: close sidechannel: %v", err)
	}
	return &gitalypb.PostUploadPackWithSidechannelResponse{}, nil
}

// httpExchange runs one request against the internal gitwire mux. gl is
// the GitLab push identity (workhorse sets GlId/GlRepository/…) — threaded
// as headers so the receive path can drive the platform's post-receive.
func (s *smartHTTPService) httpExchange(method, urlPath, contentType, gitProtocol string, body []byte, gl map[string]string) (*httptest.ResponseRecorder, error) {
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
	for k, v := range gl {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	s.Handler.ServeHTTP(rec, req)
	return rec, nil
}

// glHeaders collects the identity fields the wire handlers read.

// mappedPath renders the gitwire URL for a mapped repo name.
func mappedPath(name string) string { return "/r/" + name + ".git" }

func (sh *smartHTTPService) infoRefs(ctx context.Context, repo *gitalypb.Repository, service, gitProtocol string, send func([]byte) error) error {
	name, err := sh.ExistingRepo(ctx, repo)
	if err != nil {
		return err
	}
	rec, err := sh.httpExchange(http.MethodGet, mappedPath(name)+"/info/refs?service="+service, "", gitProtocol, nil, nil)
	if err != nil {
		return err
	}
	if rec.Code != http.StatusOK {
		return status.Errorf(codes.NotFound, "gitaly: %s advertisement failed: %s", service, rec.Body.String())
	}
	return kit.StreamBytes(rec.Body.Bytes(), send)
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
	gl := map[string]string{}
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = sh.ExistingRepo(ctx, msg.GetRepository()); err != nil {
				return err
			}
			// Workhorse sends the pusher's platform identity on the first
			// message — it drives the post-receive adapter (docs/20 §2).
			gl[gitwire.GlIDHeader] = msg.GetGlId()
			gl[gitwire.GlRepoHeader] = msg.GetGlRepository()
			gl[gitwire.GlUsernameHeader] = msg.GetGlUsername()
		}
		if gp := msg.GetGitProtocol(); gp != "" {
			gitProtocol = gp
		}
		body = append(body, msg.GetData()...)
	}
	rec, err := sh.httpExchange(http.MethodPost, mappedPath(name)+"/git-receive-pack",
		"application/x-git-receive-pack-request", gitProtocol, body, gl)
	if err != nil {
		return err
	}
	if rec.Code != http.StatusOK {
		return status.Errorf(codes.Internal, "gitaly: receive-pack failed: %s", rec.Body.String())
	}
	return kit.StreamBytes(rec.Body.Bytes(), func(b []byte) error {
		return stream.Send(&gitalypb.PostReceivePackResponse{Data: b})
	})
}

// AdvertiseBundleURI: banyan runs no bundle-URI CDN, so the advertisement
// is empty — the same shape upstream serves when the bundle-uri feature is
// unconfigured (clients never learn a URI and fall back to normal fetches).
func (sh *smartHTTPService) AdvertiseBundleURI(ctx context.Context, req *gitalypb.AdvertiseBundleURIRequest) (*gitalypb.AdvertiseBundleURIResponse, error) {
	if _, err := sh.ExistingRepo(ctx, req.GetRepository()); err != nil {
		return nil, err
	}
	return &gitalypb.AdvertiseBundleURIResponse{}, nil
}
