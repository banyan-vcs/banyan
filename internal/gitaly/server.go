// Package gitaly serves the Gitaly gRPC protocol (v19.5.2, vendored pb in
// internal/gitalypb) on top of banyan's object plane, so a GitLab Rails/workhorse
// stack can point at banyan instead of a stock gitaly node (docs/19).
//
// Trust model: like upstream gitaly, the wire is trusted after a shared
// bearer token — authorization happens in the frontend (Rails/console), not
// here. The banyan auth plane (docs/07) is bypassed by design.
//
// Coverage grows in vertical slices; every service is registered from the
// start and unimplemented RPCs fail with codes.Unimplemented so clients see
// a stable contract, never a connection error.
package gitaly

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"net"
	"strings"


	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// ProtoVersion is the vendored gitaly protocol (internal/gitalypb) this
// server speaks. ServerInfo reports it as server_version; Rails requires a
// minimum gitaly version, so the compat layer pins to the vendored minor.
const ProtoVersion = "19.5.2"

// GitVersionReported is what ServerInfo reports as git_version. Banyan has
// no git binary — this states the git feature baseline its pure-Go protocol
// surface implements, for frontends that gate on it.
const GitVersionReported = "2.45.2"

// Options configures the compat server.
type Options struct {
	// Token is the bearer token clients must present (authorization:
	// Bearer <token>); empty disables authentication (development only).
	Token string
	// StorageName is the virtual storage this node answers as — must match
	// the storage name the frontend is configured with. Default "default".
	StorageName string
	// ReportedVersion overrides ProtoVersion in ServerInfo (rare; e.g.
	// impersonating an older node).
	ReportedVersion string
}

// Server is the gitaly-protocol frontend over banyan storage.
type Server struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo
	Svc     *service.Service
	Opts    Options

	signingKey ed25519.PrivateKey
	grpcSrv    *grpc.Server
	lis        net.Listener
}

// NewServer builds a server registering every gitaly service. Wire it with
// ListenAndServe; absent listen config keeps it off.
func NewServer(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo, opts Options) *Server {
	if opts.StorageName == "" {
		opts.StorageName = "default"
	}
	if opts.ReportedVersion == "" {
		opts.ReportedVersion = ProtoVersion
	}
	_, priv, _ := ed25519.GenerateKey(nil) // sign(); rotation = restart, Rails refetches
	s := &Server{
		Meta:       meta,
		Objects:    objects,
		Algo:       algo,
		Svc:        service.New(meta, objects, algo),
		Opts:       opts,
		signingKey: priv,
	}
	s.grpcSrv = grpc.NewServer(
		grpc.ChainUnaryInterceptor(s.authUnary),
		grpc.ChainStreamInterceptor(s.authStream),
	)
	gitalypb.RegisterServerServiceServer(s.grpcSrv, &serverService{s: s})
	gitalypb.RegisterRepositoryServiceServer(s.grpcSrv, &repositoryService{s: s})
	gitalypb.RegisterRefServiceServer(s.grpcSrv, &refService{s: s})
	gitalypb.RegisterCommitServiceServer(s.grpcSrv, &commitService{s: s})
	gitalypb.RegisterBlobServiceServer(s.grpcSrv, &blobService{s: s})
	gitalypb.RegisterDiffServiceServer(s.grpcSrv, &diffService{s: s})
	gitalypb.RegisterSmartHTTPServiceServer(s.grpcSrv, &smartHTTPService{s: s})
	gitalypb.RegisterSSHServiceServer(s.grpcSrv, &sshService{s: s})
	gitalypb.RegisterOperationServiceServer(s.grpcSrv, &operationService{s: s})
	gitalypb.RegisterHookServiceServer(s.grpcSrv, &hookService{s: s})
	gitalypb.RegisterConflictsServiceServer(s.grpcSrv, &conflictsService{s: s})
	gitalypb.RegisterRemoteServiceServer(s.grpcSrv, &remoteService{s: s})
	gitalypb.RegisterCleanupServiceServer(s.grpcSrv, &cleanupService{s: s})
	gitalypb.RegisterObjectPoolServiceServer(s.grpcSrv, &objectPoolService{s: s})
	gitalypb.RegisterInternalGitalyServer(s.grpcSrv, &internalGitalyService{s: s})
	gitalypb.RegisterBackupServiceServer(s.grpcSrv, &backupService{s: s})
	gitalypb.RegisterPartitionServiceServer(s.grpcSrv, &partitionService{s: s})
	gitalypb.RegisterAnalysisServiceServer(s.grpcSrv, &analysisService{s: s})
	gitalypb.RegisterPraefectInfoServiceServer(s.grpcSrv, &praefectInfoService{s: s})
	gitalypb.RegisterRefTransactionServer(s.grpcSrv, &refTransactionService{s: s})
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
		if token, ok := strings.CutPrefix(v, bearerPrefix); ok &&
			subtle.ConstantTimeCompare([]byte(token), []byte(s.Opts.Token)) == 1 {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "gitaly: invalid bearer token")
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

// openSSHPublicKey renders an ed25519 public key in authorized_keys form —
// the format callers of ServerSignature expect.
func openSSHPublicKey(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: der}
	var out strings.Builder
	if err := pem.Encode(&out, block); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(out.String())), nil
}
