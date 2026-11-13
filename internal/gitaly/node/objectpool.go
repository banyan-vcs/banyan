package node

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Object pools (docs/integration/gitaly-compat.md). Upstream pools dedup fork objects through git's
// alternates mechanism: the member repository reads through to the pool's
// object directory. banyan's packs are per-repository, so the honest
// equivalent is materialization — linking copies any pool object the member
// lacks, and the membership registry (repo_pools) is the durable record.
// Physical dedup stays out of scope: the pool is the designated shared-base
// source, not a storage-saving device in this deployment shape.

// poolPrefix is the relative-path namespace pool repositories live in —
// upstream's IsPoolRepository contract. kit.MapRelativePath folds
// "@pools/<h2>/<h2>/<hex64>" onto the "pools-<hex64>" storage key.
const poolPrefix = "@pools/"

// poolStorageName maps a pool's gitaly relative path onto the banyan
// storage key (the same fold every per-repo RPC applies).
func (os *objectPoolService) poolStorageName(rel string) (string, error) {
	if !strings.HasPrefix(rel, poolPrefix) {
		return "", status.Errorf(codes.InvalidArgument, "gitaly: object pool path %q must live under %s", rel, poolPrefix)
	}
	return kit.MapRelativePath(rel), nil
}

// CreateObjectPool creates the pool repository seeded with the origin's
// full object closure and records the origin as the pool's upstream.
func (os *objectPoolService) CreateObjectPool(ctx context.Context, in *gitalypb.CreateObjectPoolRequest) (*gitalypb.CreateObjectPoolResponse, error) {
	originName, err := os.ExistingRepo(ctx, in.GetOrigin())
	if err != nil {
		return nil, err
	}
	poolRepo := in.GetObjectPool().GetRepository()
	if poolRepo == nil {
		return nil, status.Error(codes.InvalidArgument, "gitaly: object pool is required")
	}
	poolName, err := os.poolStorageName(poolRepo.GetRelativePath())
	if err != nil {
		return nil, err
	}
	if _, err := os.ExistingRepo(ctx, poolRepo); err == nil {
		return nil, status.Error(codes.FailedPrecondition, "gitaly: target path exists already")
	}
	if err := os.Meta.CreateRepo(ctx, poolName, os.Algo); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.copyRepoObjects(ctx, originName, poolName); err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: %v", err)
	}
	// The registry records the pool's gitaly relative path (the
	// pool_disk_path the internal metadata RPCs speak); storage ops map it
	// through kit.MapRelativePath like every other RPC.
	if err := os.Meta.LinkPool(ctx, originName, poolRepo.GetRelativePath(), true); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.CreateObjectPoolResponse{}, nil
}

// DeleteObjectPool removes the pool repository and unlinks every member.
func (os *objectPoolService) DeleteObjectPool(ctx context.Context, in *gitalypb.DeleteObjectPoolRequest) (*gitalypb.DeleteObjectPoolResponse, error) {
	poolRepo := in.GetObjectPool().GetRepository()
	if poolRepo == nil {
		return nil, status.Error(codes.InvalidArgument, "gitaly: object pool is required")
	}
	name, err := os.poolStorageName(poolRepo.GetRelativePath())
	if err != nil {
		return nil, err
	}
	if _, gerr := os.Meta.GetRepo(ctx, name); gerr != nil {
		return nil, status.Errorf(codes.NotFound, "gitaly: repository %q not found", name)
	}
	if err := os.Meta.DeletePoolRecords(ctx, poolRepo.GetRelativePath()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Meta.DeleteRepo(ctx, name); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Objects.DeleteRepo(ctx, name); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.DeleteObjectPoolResponse{}, nil
}

// LinkRepositoryToObjectPool links the member: registry record plus the
// materialization pass (the alternates equivalent — the member ends up
// holding every pool object).
func (os *objectPoolService) LinkRepositoryToObjectPool(ctx context.Context, in *gitalypb.LinkRepositoryToObjectPoolRequest) (*gitalypb.LinkRepositoryToObjectPoolResponse, error) {
	member, err := os.ExistingRepo(ctx, in.GetRepository())
	if err != nil {
		return nil, err
	}
	poolRepo := in.GetObjectPool().GetRepository()
	if poolRepo == nil {
		return nil, status.Error(codes.InvalidArgument, "gitaly: object pool is required")
	}
	poolName, err := os.poolStorageName(poolRepo.GetRelativePath())
	if err != nil {
		return nil, err
	}
	if _, gerr := os.Meta.GetRepo(ctx, poolName); gerr != nil {
		return nil, status.Errorf(codes.NotFound, "gitaly: repository %q not found", poolName)
	}
	if err := os.copyAllObjects(ctx, poolName, member); err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: %v", err)
	}
	if err := os.Meta.LinkPool(ctx, member, poolRepo.GetRelativePath(), false); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.LinkRepositoryToObjectPoolResponse{}, nil
}

// DisconnectGitAlternates unlinks the member. banyan members hold their
// objects locally (materialized at link time), so no data moves — the
// registry record is all there is to drop.
func (os *objectPoolService) DisconnectGitAlternates(ctx context.Context, in *gitalypb.DisconnectGitAlternatesRequest) (*gitalypb.DisconnectGitAlternatesResponse, error) {
	member, err := os.ExistingRepo(ctx, in.GetRepository())
	if err != nil {
		return nil, err
	}
	if err := os.Meta.UnlinkPool(ctx, member); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.DisconnectGitAlternatesResponse{}, nil
}

// FetchIntoObjectPool extends the pool with the origin's current closure
// (upstream's periodic re-fetch, which keeps the pool a superset of the
// fork network's shared base).
func (os *objectPoolService) FetchIntoObjectPool(ctx context.Context, in *gitalypb.FetchIntoObjectPoolRequest) (*gitalypb.FetchIntoObjectPoolResponse, error) {
	originName, err := os.ExistingRepo(ctx, in.GetOrigin())
	if err != nil {
		return nil, err
	}
	poolRepo := in.GetObjectPool().GetRepository()
	if poolRepo == nil {
		return nil, status.Error(codes.InvalidArgument, "gitaly: object pool is required")
	}
	poolName, err := os.poolStorageName(poolRepo.GetRelativePath())
	if err != nil {
		return nil, err
	}
	if _, gerr := os.Meta.GetRepo(ctx, poolName); gerr != nil {
		return nil, status.Errorf(codes.NotFound, "gitaly: repository %q not found", poolName)
	}
	if err := os.copyRepoObjects(ctx, originName, poolName); err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: %v", err)
	}
	return &gitalypb.FetchIntoObjectPoolResponse{}, nil
}

// GetObjectPool answers the pool the repository is linked to (nil when
// unlinked — upstream's shape for "no pool").
func (os *objectPoolService) GetObjectPool(ctx context.Context, in *gitalypb.GetObjectPoolRequest) (*gitalypb.GetObjectPoolResponse, error) {
	member, err := os.ExistingRepo(ctx, in.GetRepository())
	if err != nil {
		return nil, err
	}
	pool, ok, err := os.Meta.PoolOf(ctx, member)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return &gitalypb.GetObjectPoolResponse{}, nil
	}
	return &gitalypb.GetObjectPoolResponse{
		ObjectPool: &gitalypb.ObjectPool{
			Repository: &gitalypb.Repository{
				StorageName:  os.Opts.StorageName,
				RelativePath: pool,
			},
		},
	}, nil
}

// copyRepoObjects copies the objects reachable from the source's refs into
// the target (pool seeding — the ref-driven closure pass).
func (os *objectPoolService) copyRepoObjects(ctx context.Context, source, target string) error {
	refs, err := os.AdvertisedRefs(ctx, source)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		tip := r.Head
		if typ, _, terr := os.Objects.Raw(ctx, source, tip); terr == nil && typ == model.TagType {
			peeled, perr := os.PeelToCommit(ctx, source, tip)
			if perr != nil {
				continue
			}
			tip = peeled
		}
		if err := os.CopyClosure(ctx, source, target, tip); err != nil {
			return fmt.Errorf("pool copy: %w", err)
		}
	}
	return nil
}

// copyAllObjects copies every stored object of the source into the target
// (the materialization pass — pools carry objects without refs, so the
// pack index is the enumeration, not the ref list).
func (os *objectPoolService) copyAllObjects(ctx context.Context, source, target string) error {
	packs, err := os.Meta.ListPacks(ctx, source)
	if err != nil {
		return err
	}
	for _, pack := range packs {
		oids, err := os.Meta.PackObjects(ctx, source, pack.PackOID)
		if err != nil {
			return err
		}
		batch := make([]service.PutObject, 0, len(oids))
		for _, oid := range oids {
			typ, raw, err := os.Objects.Raw(ctx, source, oid)
			if err != nil {
				return fmt.Errorf("pool object %s: %w", oid, err)
			}
			batch = append(batch, service.PutObject{OID: oid, Type: string(typ), Data: raw})
		}
		if len(batch) > 0 {
			if err := os.Svc.PutObjects(ctx, target, batch); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- InternalGitaly: repo walking and the pool-metadata registry ---

// WalkRepos streams every repository on the storage (praefect's inventory
// walk; banyan answers from the metadata plane).
func (ig *internalGitalyService) WalkRepos(in *gitalypb.WalkReposRequest, stream gitalypb.InternalGitaly_WalkReposServer) error {
	if in.GetStorageName() != "" && in.GetStorageName() != ig.Opts.StorageName {
		return status.Errorf(codes.NotFound, "gitaly: unknown storage %q", in.GetStorageName())
	}
	repos, err := ig.Meta.ListRepos(stream.Context())
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	for _, repo := range repos {
		if err := stream.Send(&gitalypb.WalkReposResponse{
			RelativePath: repo.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// StorePoolMetadata records one repository↔pool relationship — the same
// registry the ObjectPoolService maintains, answering praefect's metadata
// store contract (client-streamed: the first message carries the record).
func (ig *internalGitalyService) StorePoolMetadata(stream gitalypb.InternalGitaly_StorePoolMetadataServer) error {
	ctx := stream.Context()
	in, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving request: %v", err)
	}
	if in.GetRelativePath() == "" || in.GetPoolDiskPath() == "" {
		return status.Error(codes.InvalidArgument, "gitaly: relative path and pool disk path are required")
	}
	if err := ig.Meta.LinkPool(ctx, in.GetRelativePath(), in.GetPoolDiskPath(), in.GetIsUpstream()); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendAndClose(&gitalypb.StorePoolMetadataResponse{})
}

// ScanPoolMetadata streams every membership record (member, pool, upstream).
func (ig *internalGitalyService) ScanPoolMetadata(in *gitalypb.ScanPoolMetadataRequest, stream gitalypb.InternalGitaly_ScanPoolMetadataServer) error {
	if in.GetStorageName() != "" && in.GetStorageName() != ig.Opts.StorageName {
		return status.Errorf(codes.NotFound, "gitaly: unknown storage %q", in.GetStorageName())
	}
	members, err := ig.Meta.ListPoolMembers(stream.Context())
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	for _, m := range members {
		if err := stream.Send(&gitalypb.ScanPoolMetadataResponse{
			RelativePath: m.Member,
			PoolDiskPath: m.Pool,
			IsUpstream:   m.Upstream,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ListPoolMetadata streams the distinct pool repositories on the storage.
func (ig *internalGitalyService) ListPoolMetadata(in *gitalypb.ListPoolMetadataRequest, stream gitalypb.InternalGitaly_ListPoolMetadataServer) error {
	if in.GetStorageName() != "" && in.GetStorageName() != ig.Opts.StorageName {
		return status.Errorf(codes.NotFound, "gitaly: unknown storage %q", in.GetStorageName())
	}
	members, err := ig.Meta.ListPoolMembers(stream.Context())
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	seen := map[string]bool{}
	var pools []string
	for _, m := range members {
		if !seen[m.Pool] {
			seen[m.Pool] = true
			pools = append(pools, m.Pool)
		}
	}
	sort.Strings(pools)
	for _, p := range pools {
		if err := stream.Send(&gitalypb.ListPoolMetadataResponse{PoolDiskPath: p}); err != nil {
			return err
		}
	}
	return nil
}

// ListPoolUpstreams answers pool → upstream-repository for the asked pools
// (a bidi stream: the first request names the pools, one response carries
// the map; pools without an upstream are omitted, upstream's shape).
func (ig *internalGitalyService) ListPoolUpstreams(stream gitalypb.InternalGitaly_ListPoolUpstreamsServer) error {
	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving request: %v", err)
	}
	if req.GetStorageName() != "" && req.GetStorageName() != ig.Opts.StorageName {
		return status.Errorf(codes.NotFound, "gitaly: unknown storage %q", req.GetStorageName())
	}
	members, err := ig.Meta.ListPoolMembers(stream.Context())
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	upstreams := map[string]string{}
	for _, m := range members {
		if m.Upstream {
			upstreams[m.Pool] = m.Member
		}
	}
	asked := map[string]bool{}
	for _, p := range req.GetPoolDiskPaths() {
		asked[p] = true
	}
	out := map[string]string{}
	for pool, upstream := range upstreams {
		if len(asked) == 0 || asked[pool] {
			out[pool] = upstream
		}
	}
	return stream.Send(&gitalypb.ListPoolUpstreamsResponse{Upstreams: out})
}

type objectPoolService struct {
	gitalypb.UnimplementedObjectPoolServiceServer
	*kit.Base
}

type internalGitalyService struct {
	gitalypb.UnimplementedInternalGitalyServer
	*kit.Base
}
