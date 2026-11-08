package operations

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

const testToken = "s3cr3t"

// startServer boots the operations family over an in-memory banyan stack
// (the root package serves every family; these tests construct the family
// struct directly, so a family-local endpoint suffices).
func startServer(t *testing.T) (*kit.Base, *storage.ObjectService, *service.Service, string) {
	t.Helper()
	ms := meta.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(t.TempDir())
	objects := storage.NewObjectService(ms, local, []storage.Node{{ID: "local-1", Local: true}}, model.SHA1)
	svc := service.New(ms, objects, model.SHA1)
	b := &kit.Base{Meta: ms, Objects: objects, Algo: model.SHA1, Svc: svc,
		Opts: kit.Options{StorageName: "default", ReportedVersion: kit.ProtoVersion},
		Git:  gitwire.NewServer(ms, objects, model.SHA1)}
	gs := grpc.NewServer()
	Register(gs, b)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return b, objects, svc, lis.Addr().String()
}

// dial connects with the bearer token attached (mirrors gitaly.Dial without
// importing the root package — that would cycle).
func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testToken), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testToken), desc, cc, method, opts...)
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func repoRef(name string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: "default", RelativePath: name}
}
