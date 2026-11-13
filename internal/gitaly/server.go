// Package gitaly serves the Gitaly gRPC protocol (v19.5.2, vendored pb in
// internal/gitalypb) on top of banyan's object plane, so a GitLab Rails/workhorse
// stack can point at banyan instead of a stock gitaly node (docs/integration/gitaly-compat.md).
//
// Trust model: like upstream gitaly, the wire is trusted after a shared
// bearer token — authorization happens in the frontend (Rails/console), not
// here. The banyan auth plane (docs/gateway/authorization.md) is bypassed by design.
//
// Structure (2026-11-08 split): this package is the shell — server
// lifecycle, bearer-token auth, the gRPC mux — while each protocol service
// family lives in its own sub-package (repository, commit, ref, object,
// operations, transport, backup, node) over the shared kit.Base plane.
// Coverage grows in vertical slices; every service is registered from the
// start and unimplemented RPCs fail with codes.Unimplemented so clients see
// a stable contract, never a connection error.
package gitaly

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/backup"
	"github.com/banyan-vcs/banyan/internal/gitaly/commit"
	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitaly/node"
	"github.com/banyan-vcs/banyan/internal/gitaly/object"
	"github.com/banyan-vcs/banyan/internal/gitaly/operations"
	"github.com/banyan-vcs/banyan/internal/gitaly/ref"
	"github.com/banyan-vcs/banyan/internal/gitaly/repository"
	"github.com/banyan-vcs/banyan/internal/gitaly/transport"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// ProtoVersion is the vendored gitaly protocol this server speaks (kit
// owns the value; ServerInfo reports it as server_version).
const ProtoVersion = kit.ProtoVersion

// GitVersionReported is what ServerInfo reports as git_version (kit owns
// the value).
const GitVersionReported = kit.GitVersionReported

// Options configures the compat server (kit owns the shape; the alias
// keeps NewServer's signature stable for callers).
type Options = kit.Options

// maxWireMsg matches upstream's large-message posture: a fetch response or
// push body rides in few/one protobuf messages, not 4MiB defaults.
const maxWireMsg = 1 << 30

// Server is the gitaly-protocol frontend over banyan storage: the shell
// (auth, mux, lifecycle) around the per-service families, all sharing the
// embedded kit.Base plane.
type Server struct {
	*kit.Base

	// git/mux: an internal gitwire instance serving the git wire protocol;
	// smart-HTTP/SSH RPCs bridge onto it (docs/integration/gitaly-compat.md §1). The Base mirrors
	// both (Git, Handler) for the families.
	git     *gitwire.Server
	grpcSrv *grpc.Server
	lis     net.Listener
}

// NewServer builds a server registering every gitaly service. Wire it with
// ListenAndServe; absent listen config keeps it off.
func NewServer(meta meta.MetaStore, objects *storage.ObjectService, algo model.HashAlgo, opts Options) *Server {
	if opts.StorageName == "" {
		opts.StorageName = "default"
	}
	if opts.ReportedVersion == "" {
		opts.ReportedVersion = ProtoVersion
	}
	_, priv, _ := ed25519.GenerateKey(nil) // sign(); rotation = restart, Rails refetches
	b := &kit.Base{
		Meta:       meta,
		Objects:    objects,
		Algo:       algo,
		Svc:        service.New(meta, objects, algo),
		Opts:       opts,
		SigningKey: priv,
	}
	s := &Server{Base: b}
	s.git = gitwire.NewServer(meta, objects, algo)
	s.git.SetGitLab(opts.GitLab)
	s.git.Convergence = opts.Convergence
	s.git.FullCloneMaxBytes = opts.FullCloneMaxByte
	b.Git = s.git
	mux := http.NewServeMux()
	s.git.Register(mux)
	b.Handler = mux
	s.grpcSrv = grpc.NewServer(
		grpc.Creds(transport.MuxCredentials{}),
		grpc.ChainUnaryInterceptor(s.authUnary),
		grpc.ChainStreamInterceptor(s.authStream),
		grpc.MaxRecvMsgSize(maxWireMsg),
		grpc.MaxSendMsgSize(maxWireMsg),
	)
	repository.Register(s.grpcSrv, b)
	ref.Register(s.grpcSrv, b)
	commit.Register(s.grpcSrv, b)
	object.Register(s.grpcSrv, b)
	operations.Register(s.grpcSrv, b)
	transport.Register(s.grpcSrv, b)
	backup.Register(s.grpcSrv, b)
	node.Register(s.grpcSrv, b)
	return s
}

// ListenAndServe binds addr ("host:port", or "unix:<path>") and serves until
// Stop. It returns on bind errors only.
func (s *Server) ListenAndServe(addr string) error {
	network, target := "tcp", addr
	if u, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, target = "unix", u
	}
	lis, err := net.Listen(network, target)
	if err != nil {
		return err
	}
	s.lis = lis
	return s.grpcSrv.Serve(lis)
}

// Serve runs on an existing listener (tests bind ephemeral ports).
func (s *Server) Serve(l net.Listener) error {
	s.lis = l
	return s.grpcSrv.Serve(l)
}

// Stop tears the server down (tests, shutdown).
func (s *Server) Stop() { s.grpcSrv.Stop() }

// drainGrace bounds graceful gRPC shutdown (see GracefulStop).
const drainGrace = 15 * time.Second

// GracefulStop finishes in-flight RPCs — a streaming clone keeps its
// context — then closes the listener; whatever outlives the grace is cut
// with a hard Stop (rolling restarts must not hang on a stuck stream).
func (s *Server) GracefulStop() {
	done := make(chan struct{})
	go func() { s.grpcSrv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(drainGrace):
		s.grpcSrv.Stop()
	}
}

// --- authentication ---

const bearerPrefix = "Bearer "

func (s *Server) authUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.checkToken(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) authStream(req any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.checkToken(ss.Context()); err != nil {
		return err
	}
	return handler(req, ss)
}

func (s *Server) checkToken(ctx context.Context) error {
	if s.Opts.Token == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.PermissionDenied, "gitaly: missing metadata")
	}
	for _, v := range md.Get("authorization") {
		token, ok := strings.CutPrefix(v, bearerPrefix)
		if !ok {
			continue
		}
		// Two accepted forms. Real GitLab (Rails GitalyClient, since 16.x)
		// signs a fresh token per call — v2.<hex HMAC>.<unix-seconds>, HMAC
		// over the timestamp keyed by the shared secret, valid for ±30s
		// (upstream auth/token.go at the vendored tag). The bare constant
		// compare is the legacy form gitaly's older clients — and banyan's
		// own Dial — still present.
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.Opts.Token)) == 1 {
			return nil
		}
		if err := checkV2Token(token, s.Opts.Token, time.Now()); err == nil {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "gitaly: invalid bearer token")
}

// v2TokenValidity matches upstream's window (30s both directions — the
// client signs per call, the window only absorbs clock skew).
const v2TokenValidity = 30 * time.Second

// checkV2Token verifies the HMAC bearer form GitLab Rails sends:
// "v2.<hex hmac-sha256(secret, issuedAt)>.<issuedAt>". Malformed tokens
// fail closed; the error text mirrors upstream's reasons for diagnostics.
func checkV2Token(token, secret string, now time.Time) error {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return fmt.Errorf("invalid token format")
	}
	sigHex, msg := parts[1], parts[2]
	if parts[0] != "v2" {
		return fmt.Errorf("invalid token version")
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("invalid token signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(msg))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return fmt.Errorf("wrong hmac signature")
	}
	ts, err := strconv.ParseInt(msg, 10, 64)
	if err != nil {
		return fmt.Errorf("cannot parse timestamp")
	}
	issued := time.Unix(ts, 0)
	if issued.Before(now.Add(-v2TokenValidity)) {
		return fmt.Errorf("token has expired")
	}
	if issued.After(now.Add(v2TokenValidity)) {
		return fmt.Errorf("token's validity window is in future")
	}
	return nil
}

// Dial is the client counterpart for tests: a plain insecure connection
// with the bearer token attached.
func Dial(addr string, token string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tokenInterceptor(token)),
		grpc.WithStreamInterceptor(tokenStreamInterceptor(token)))
}

func tokenInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", bearerPrefix+token), method, req, reply, cc, opts...)
	}
}

func tokenStreamInterceptor(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(metadata.AppendToOutgoingContext(ctx, "authorization", bearerPrefix+token), desc, cc, method, opts...)
	}
}
