// T62 — sidechannel transport e2e (docs/19 §5 slice 3): the multiplexed
// connection gitaly/workhorse actually use — TCP conn opens with the
// "backchannel" magic, yamux session follows, gRPC rides stream 1, and bulk
// fetch traffic returns over server-initiated sidechannel streams. The test
// drives it with a mini-workhorse (yamux client + sidechannel callback
// registry) and a REAL git clone through the HTTP emulator; plain gRPC
// clients must keep working on the same listener.
package integration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// miniWorkhorse speaks the client half of the multiplexed contract: it dials
// with the backchannel magic, runs gRPC over the first yamux stream, and
// dispatches server-initiated sidechannel streams to registered callbacks.
type miniWorkhorse struct {
	conn    *grpc.ClientConn
	session *yamux.Session

	mu        sync.Mutex
	nextID    int64
	callbacks map[int64]func(net.Conn) error
	waiters   map[int64]chan error
}

func newMiniWorkhorse(t *testing.T, addr, token string) *miniWorkhorse {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	_, err = raw.Write([]byte("backchannel"))
	require.NoError(t, err)
	cfg := yamux.DefaultConfig()
	cfg.MaxStreamWindowSize = 16 << 20
	session, err := yamux.Client(raw, cfg)
	require.NoError(t, err)
	stream1, err := session.Open()
	require.NoError(t, err)
	conn, err := grpc.NewClient("passthrough://miniworkhorse/",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return stream1, nil }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tokenInterceptor(token)),
		grpc.WithStreamInterceptor(tokenStreamInterceptor(token)))
	require.NoError(t, err)
	wh := &miniWorkhorse{
		conn:      conn,
		session:   session,
		callbacks: map[int64]func(net.Conn) error{},
		waiters:   map[int64]chan error{},
	}
	go wh.dispatch()
	t.Cleanup(func() {
		_ = session.Close()
		_ = conn.Close()
	})
	return wh
}

func (wh *miniWorkhorse) dispatch() {
	for {
		stream, err := wh.session.Accept()
		if err != nil {
			return
		}
		go wh.serveSidechannel(stream)
	}
}

func (wh *miniWorkhorse) serveSidechannel(stream net.Conn) {
	magic := make([]byte, len("sidechannel"))
	if _, err := io.ReadFull(stream, magic); err != nil || string(magic) != "sidechannel" {
		_ = stream.Close()
		return
	}
	var id int64
	if err := binary.Read(stream, binary.BigEndian, &id); err != nil {
		_ = stream.Close()
		return
	}
	wh.mu.Lock()
	cb := wh.callbacks[id]
	done := wh.waiters[id]
	delete(wh.callbacks, id)
	delete(wh.waiters, id)
	wh.mu.Unlock()
	if cb == nil {
		_ = stream.Close()
		return
	}
	if _, err := stream.Write([]byte("ok")); err != nil {
		_ = stream.Close()
		done <- err
		return
	}
	done <- cb(stream)
}

// register attaches a sidechannel callback and returns the context carrying
// the sidechannel ID, mirroring the upstream client registry.
func (wh *miniWorkhorse) register(ctx context.Context, cb func(net.Conn) error) context.Context {
	wh.mu.Lock()
	wh.nextID++
	id := wh.nextID
	wh.callbacks[id] = cb
	wh.waiters[id] = make(chan error, 1)
	wh.mu.Unlock()
	return metadata.AppendToOutgoingContext(ctx, "gitaly-sidechannel-id", strconv.FormatInt(id, 10))
}

// sidechannelWait blocks until the callback registered on ctx completes and
// returns its error.
func (wh *miniWorkhorse) sidechannelWait(ctx context.Context) error {
	idStr := lastSidechannelID(ctx)
	id, _ := strconv.ParseInt(idStr, 10, 64)
	wh.mu.Lock()
	done := wh.waiters[id]
	wh.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case err := <-done:
		wh.mu.Lock()
		delete(wh.waiters, id)
		wh.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// bearerInterceptors attach the token on every call, the client side of the
// compat server's auth gate.
func tokenInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), method, req, reply, cc, opts...)
	}
}

func tokenStreamInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), desc, cc, method, opts...)
	}
}

func lastSidechannelID(ctx context.Context) string {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return ""
	}
	ids := md.Get("gitaly-sidechannel-id")
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

func TestT62GitalySidechannelTransport(t *testing.T) {
	needGit(t)
	s, meta, objects := newServerPlane(t)

	gs := gitaly.NewServer(meta, objects, model.SHA1, gitaly.Options{Token: "t62"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	addr := lis.Addr().String()

	ctx := context.Background()
	wh := newMiniWorkhorse(t, addr, "t62")
	sh := gitalypb.NewSmartHTTPServiceClient(wh.conn)
	rsc := gitalypb.NewRepositoryServiceClient(wh.conn)
	ssc := gitalypb.NewSSHServiceClient(wh.conn)

	// GitLab flow: create the repo, seed it through the native surface.
	_, err = rsc.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}})
	require.NoError(t, err)
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", ".")
	write(t, src+"/README.md", "t62 sidechannel readme\n")
	write(t, src+"/lib/a.txt", "a\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "t62 seed")
	git(t, src, "push", "-q", s.repoURL("proj"), "main")

	// The workhorse front: git-HTTP in, sidechannel gRPC out.
	emulator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/info/refs"):
			stream, serr := sh.InfoRefsUploadPack(ctx, &gitalypb.InfoRefsRequest{
				Repository:  &gitalypb.Repository{StorageName: "default", RelativePath: "proj"},
				GitProtocol: r.Header.Get("Git-Protocol")})
			if serr != nil {
				http.Error(w, serr.Error(), http.StatusBadGateway)
				return
			}
			var adv []byte
			for {
				resp, rerr := stream.Recv()
				if rerr != nil {
					break
				}
				adv = append(adv, resp.GetData()...)
			}
			w.Header().Set("Content-Type", "application/x-"+r.URL.Query().Get("service")+"-advertisement")
			w.Write(adv)
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			body, _ := io.ReadAll(r.Body)
			sctx := wh.register(ctx, func(sc net.Conn) error {
				if err := writeFramed(sc, body); err != nil {
					return err
				}
				if _, err := io.WriteString(sc, "0000"); err != nil { // half-close
					return err
				}
				_, err := io.Copy(w, sc) // server→client is raw bytes
				return err
			})
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, err := sh.PostUploadPackWithSidechannel(sctx, &gitalypb.PostUploadPackWithSidechannelRequest{
				Repository:  &gitalypb.Repository{StorageName: "default", RelativePath: "proj"},
				GitProtocol: r.Header.Get("Git-Protocol")})
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			if werr := wh.sidechannelWait(sctx); werr != nil {
				http.Error(w, werr.Error(), http.StatusBadGateway)
				return
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(emulator.Close)

	t.Run("real git clone through the sidechannel", func(t *testing.T) {
		dst := t.TempDir()
		git(t, "", "clone", "-q", emulator.URL+"/proj.git", dst)
		assert.Equal(t, headOf(t, src), headOf(t, dst))
		assert.Equal(t, "t62 sidechannel readme\n", readFile(t, dst+"/README.md"))
		assert.Equal(t, "a\n", readFile(t, dst+"/lib/a.txt"))
		fsckClean(t, dst)
	})

	t.Run("ssh upload session over sidechannel", func(t *testing.T) {
		head := headOf(t, src)
		sctx := wh.register(ctx, func(sc net.Conn) error {
			br := newPktReader(sc)
			// 1) drain the ref advertisement (ends with a flush pkt)
			for {
				_, flush, err := br.pkt()
				if err != nil {
					return err
				}
				if flush {
					break
				}
			}
			// 2) a minimal v0 fetch: want + flush + done (framed), then the
			// half-close flush frame
			req := fmt.Sprintf("%04x", len("want "+head+"\n")+4) + "want " + head + "\n" +
				"0000" + fmt.Sprintf("%04x", len("done\n")+4) + "done\n"
			if err := writeFramed(sc, []byte(req)); err != nil {
				return err
			}
			if _, err := io.WriteString(sc, "0000"); err != nil {
				return err
			}
			// 3) response: NAK then the pack
			resp, err := io.ReadAll(sc)
			if err != nil {
				return err
			}
			assert.Contains(t, string(resp[:min(64, len(resp))]), "NAK")
			assert.Contains(t, string(resp), "PACK")
			return nil
		})
		_, err := ssc.SSHUploadPackWithSidechannel(sctx, &gitalypb.SSHUploadPackWithSidechannelRequest{
			Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}})
		require.NoError(t, err)
		require.NoError(t, wh.sidechannelWait(sctx))
	})

	t.Run("plain grpc still served on the same listener", func(t *testing.T) {
		conn, err := gitaly.Dial(addr, "t62")
		require.NoError(t, err)
		defer conn.Close()
		info, err := gitalypb.NewServerServiceClient(conn).ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
		require.NoError(t, err)
		assert.Equal(t, gitaly.ProtoVersion, info.GetServerVersion())
	})

	t.Run("sidechannel rpc on plain connection is rejected", func(t *testing.T) {
		conn, err := gitaly.Dial(addr, "t62")
		require.NoError(t, err)
		defer conn.Close()
		_, err = gitalypb.NewSmartHTTPServiceClient(conn).PostUploadPackWithSidechannel(ctx,
			&gitalypb.PostUploadPackWithSidechannelRequest{
				Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "proj"}})
		assert.Error(t, err) // no sidechannel-id metadata / no backchannel session
	})
}

// writeFramed sends payload as length-prefixed sidechannel frames (client→
// server direction of the sidechannel framing).
func writeFramed(w io.Writer, p []byte) error {
	for len(p) > 0 {
		chunk := len(p)
		if chunk > 65531 {
			chunk = 65531
		}
		if _, err := fmt.Fprintf(w, "%04x", chunk+4); err != nil {
			return err
		}
		if _, err := w.Write(p[:chunk]); err != nil {
			return err
		}
		p = p[chunk:]
	}
	return nil
}

// pktReader reads pkt-lines without buffering beyond the payload.
type pktReader struct {
	r io.Reader
}

func newPktReader(r io.Reader) *pktReader { return &pktReader{r: r} }

func (p *pktReader) pkt() (line string, flush bool, err error) {
	var hdr [4]byte
	if _, err = io.ReadFull(p.r, hdr[:]); err != nil {
		return "", false, err
	}
	var n int
	if _, err = fmt.Sscanf(string(hdr[:]), "%04x", &n); err != nil {
		return "", false, err
	}
	if n == 0 {
		return "", true, nil
	}
	buf := make([]byte, n-4)
	if _, err = io.ReadFull(p.r, buf); err != nil {
		return "", false, err
	}
	return string(buf), false, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
