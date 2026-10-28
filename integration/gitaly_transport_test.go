// T61 — gitaly transport slice e2e (docs/19 §5 slice 2): workhorse-shaped
// git-HTTP traffic flows through the gRPC SmartHTTPService (a local emulator
// plays workhorse: HTTP in → gRPC stream → gitwire mux), and the SSHService
// batch session runs a real receive-pack advertisement. Oracle = the direct
// /r/ endpoints: what a real git client pushes through the gRPC bridge must
// byte-match what the native HTTP surface serves.
package integration

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

func TestT61GitalyTransportSurface(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)

	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t61"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t61")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	sh := gitalypb.NewSmartHTTPServiceClient(conn)
	ss := gitalypb.NewSSHServiceClient(conn)

	// A local workhorse: git-HTTP on the front, gRPC on the back.
	workhorse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/info/refs"):
			service := r.URL.Query().Get("service")
			var adv []byte
			var err error
			switch service {
			case "git-upload-pack":
				stream, serr := sh.InfoRefsUploadPack(ctx, &gitalypb.InfoRefsRequest{
					Repository:  &gitalypb.Repository{StorageName: "default", RelativePath: "proj"},
					GitProtocol: r.Header.Get("Git-Protocol")})
				if serr != nil {
					http.Error(w, serr.Error(), http.StatusBadGateway)
					return
				}
				err = drainInfoRefs(stream, &adv)
			case "git-receive-pack":
				stream, serr := sh.InfoRefsReceivePack(ctx, &gitalypb.InfoRefsRequest{
					Repository:  &gitalypb.Repository{StorageName: "default", RelativePath: "proj"},
					GitProtocol: r.Header.Get("Git-Protocol")})
				if serr != nil {
					http.Error(w, serr.Error(), http.StatusBadGateway)
					return
				}
				err = drainInfoRefs(stream, &adv)
			default:
				http.Error(w, "smart http only", http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
			w.Write(adv)
		case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
			stream, serr := sh.PostReceivePack(ctx)
			if serr != nil {
				http.Error(w, serr.Error(), http.StatusBadGateway)
				return
			}
			buf := make([]byte, 64<<10)
			first := true
			for {
				n, rerr := r.Body.Read(buf)
				if n > 0 {
					msg := &gitalypb.PostReceivePackRequest{Data: buf[:n], GitProtocol: r.Header.Get("Git-Protocol")}
					if first {
						msg.Repository = &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}
						first = false
					}
					if serr := stream.Send(msg); serr != nil {
						http.Error(w, serr.Error(), http.StatusBadGateway)
						return
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					http.Error(w, rerr.Error(), http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			if serr := stream.CloseSend(); serr != nil {
				http.Error(w, serr.Error(), http.StatusBadGateway)
				return
			}
			for {
				resp, rerr := stream.Recv()
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					http.Error(w, rerr.Error(), http.StatusBadGateway)
					return
				}
				w.Write(resp.GetData())
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(workhorse.Close)

	// GitLab's flow: the repo exists before git traffic (Rails calls
	// CreateRepository on project creation).
	_, err = gitalypb.NewRepositoryServiceClient(conn).CreateRepository(ctx,
		&gitalypb.CreateRepositoryRequest{Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}})
	require.NoError(t, err)

	// Seed locally, push through the workhorse emulation.
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/README.md", "t61 readme\n")
	write(t, src+"/lib/a.txt", "a\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t61 initial")
	git(t, src, "push", "-q", workhorse.URL+"/proj.git", "main")

	// The native surface must serve exactly what was pushed through gRPC.
	clone := cloneRepo(t, s, s.repoURL("proj"))
	assert.Equal(t, headOf(t, src), headOf(t, clone))
	fsckClean(t, clone)
	assert.Equal(t, "t61 readme\n", readFile(t, clone+"/README.md"))
	assert.Equal(t, 1, countCommits(t, clone))

	t.Run("advertisement equals the native endpoint", func(t *testing.T) {
		stream, err := sh.InfoRefsUploadPack(ctx, &gitalypb.InfoRefsRequest{
			Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}})
		require.NoError(t, err)
		var adv []byte
		require.NoError(t, drainInfoRefs(stream, &adv))
		resp, err := http.Get(s.repoURL("proj") + "/info/refs?service=git-upload-pack")
		require.NoError(t, err)
		defer resp.Body.Close()
		native, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, string(native), string(adv))
	})

	t.Run("ssh receive session advertises", func(t *testing.T) {
		stream, err := ss.SSHReceivePack(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.SSHReceivePackRequest{
			Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}}))
		require.NoError(t, stream.CloseSend())
		var stdout []byte
		var exit *gitalypb.ExitStatus
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			stdout = append(stdout, resp.GetStdout()...)
			if resp.GetExitStatus() != nil {
				exit = resp.GetExitStatus()
			}
		}
		require.NotNil(t, exit)
		assert.Zero(t, exit.GetValue())
		assert.Contains(t, string(stdout), headOf(t, src)+" refs/heads/main")
	})

	t.Run("wrong storage name is not found", func(t *testing.T) {
		stream, err := sh.InfoRefsUploadPack(ctx, &gitalypb.InfoRefsRequest{
			Repository: &gitalypb.Repository{StorageName: "other", RelativePath: "proj"}})
		require.NoError(t, err) // server streams: the failure surfaces on Recv
		_, err = stream.Recv()
		assert.Equal(t, codes.NotFound, status.Code(err))
	})
}

func drainInfoRefs(stream interface {
	Recv() (*gitalypb.InfoRefsResponse, error)
}, out *[]byte) error {
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		*out = append(*out, resp.GetData()...)
	}
}
