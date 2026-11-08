package ref

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	gittransport "github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Remote mirror surface: existence probes and the push-side mirror, the
// counterpart of FetchRemote on the same go-git transport client.

// FindRemoteRepository reports whether a smart-HTTP remote answers with a
// non-empty ref advertisement.
func (rm *remoteService) FindRemoteRepository(ctx context.Context, req *gitalypb.FindRemoteRepositoryRequest) (*gitalypb.FindRemoteRepositoryResponse, error) {
	remoteURL := req.GetRemote()
	if remoteURL == "" {
		return nil, status.Error(codes.InvalidArgument, "gitaly: remote is required")
	}
	refs, err := advertisedRefs(ctx, remoteURL, "")
	if err != nil {
		return &gitalypb.FindRemoteRepositoryResponse{Exists: false}, nil
	}
	return &gitalypb.FindRemoteRepositoryResponse{Exists: len(refs.References) > 0}, nil
}

// FindRemoteRootRef answers the ref the remote HEAD points at (the clone
// default branch), via the symref capability with a single-head fallback.
func (rm *remoteService) FindRemoteRootRef(ctx context.Context, req *gitalypb.FindRemoteRootRefRequest) (*gitalypb.FindRemoteRootRefResponse, error) {
	if req.GetRemoteUrl() == "" {
		return nil, status.Error(codes.InvalidArgument, "gitaly: remote_url is required")
	}
	refs, err := advertisedRefs(ctx, req.GetRemoteUrl(), req.GetHttpAuthorizationHeader())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "gitaly: remote refs: %v", err)
	}
	for _, symref := range refs.Capabilities.Get(capability.SymRef) {
		if head, target, ok := strings.Cut(symref, ":"); ok && head == "HEAD" {
			if _, exists := refs.References[target]; exists {
				return &gitalypb.FindRemoteRootRefResponse{Ref: target}, nil
			}
		}
	}
	// No symref: fall back to the only head if there is exactly one.
	var onlyHead string
	for name := range refs.References {
		if strings.HasPrefix(name, "refs/heads/") {
			if onlyHead != "" {
				return nil, status.Error(codes.FailedPrecondition, "gitaly: ambiguous remote HEAD")
			}
			onlyHead = name
		}
	}
	if onlyHead == "" {
		return nil, status.Error(codes.NotFound, "gitaly: remote has no branches")
	}
	return &gitalypb.FindRemoteRootRefResponse{Ref: onlyHead}, nil
}

// advertisedRefs opens one upload-pack session and answers its ref
// advertisement.
func advertisedRefs(ctx context.Context, url, authHeader string) (*packp.AdvRefs, error) {
	ep, err := gittransport.NewEndpoint(url)
	if err != nil {
		return nil, err
	}
	cl, err := transportclient.NewClient(ep)
	if err != nil {
		return nil, err
	}
	sess, err := cl.NewUploadPackSession(ep, kit.RawAuth{Value: authHeader})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.AdvertisedReferencesContext(ctx)
}

// UpdateRemoteMirror pushes every local ref to the remote (mirror refmap):
// one receive-pack exchange carrying each branch/tag tip's closure as a
// pack, divergent refs force-updated unless keep_divergent_refs holds
// (then they are skipped instead of deleted).
func (rm *remoteService) UpdateRemoteMirror(stream gitalypb.RemoteService_UpdateRemoteMirrorServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: first request is required")
	}
	name, err := rm.ExistingRepo(ctx, first.GetRepository())
	if err != nil {
		return err
	}
	remote := first.GetRemote()
	if remote == nil || remote.GetUrl() == "" {
		return status.Error(codes.InvalidArgument, "gitaly: remote_params.url is required")
	}
	if strings.HasPrefix(remote.GetUrl(), "ssh://") || strings.HasPrefix(remote.GetUrl(), "git@") {
		return status.Error(codes.Unimplemented, "gitaly: ssh remotes are not served")
	}

	matched := first.GetOnlyBranchesMatching()
	refs, err := rm.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	type update struct {
		name string
		oid  model.OID
	}
	var updates []update
	for _, r := range refs {
		if r.Name == "HEAD" || !strings.HasPrefix(r.Name, "refs/heads/") {
			continue // mirror refmaps cover branches (tags ride the pack)
		}
		if len(matched) > 0 && !matchAnyGlob(matched, r.Name) {
			continue
		}
		updates = append(updates, update{name: r.Name, oid: r.Head})
	}
	if len(updates) == 0 {
		return stream.SendAndClose(&gitalypb.UpdateRemoteMirrorResponse{})
	}

	ep, err := gittransport.NewEndpoint(remote.GetUrl())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: invalid remote url: %v", err)
	}
	cl, err := transportclient.NewClient(ep)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: unsupported remote protocol")
	}
	sess, err := cl.NewReceivePackSession(ep, kit.RawAuth{Value: remote.GetHttpAuthorizationHeader()})
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote session: %v", err)
	}
	defer sess.Close()
	remoteRefs, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote refs: %v", err)
	}

	req := packp.NewReferenceUpdateRequest()
	var closureHeads []model.OID
	for _, u := range updates {
		old := plumbing.ZeroHash
		if remoteRefs != nil && remoteRefs.References != nil {
			if h, ok := remoteRefs.References[u.name]; ok {
				if first.GetKeepDivergentRefs() && h.String() != u.oid.String() {
					continue // never force over a diverged remote ref
				}
				old = h
			}
		}
		cmd := &packp.Command{
			Name: plumbing.ReferenceName(u.name),
			Old:  old,
			New:  plumbing.NewHash(u.oid.String()),
		}
		req.Commands = append(req.Commands, cmd)
		closureHeads = append(closureHeads, u.oid)
	}
	if len(req.Commands) == 0 {
		return stream.SendAndClose(&gitalypb.UpdateRemoteMirrorResponse{})
	}

	// One pack with every tip's closure (receive-pack carries no
	// negotiation; a self-contained pack is always correct).
	order, err := rm.BundleClosure(ctx, name, refsOf(refs, closureHeads))
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	var packBuf bytes.Buffer
	i := 0
	if err := storage.StreamEncodePack(&packBuf, rm.Algo, len(order), func(int) (model.ObjectType, []byte, error) {
		oid := order[i]
		i++
		typ, raw, err := rm.Objects.Raw(ctx, name, oid)
		return typ, raw, err
	}); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	req.Packfile = io.NopCloser(bytes.NewReader(packBuf.Bytes()))

	if _, err := sess.ReceivePack(ctx, req); err != nil {
		return status.Errorf(codes.FailedPrecondition, "gitaly: remote receive-pack: %v", err)
	}
	return stream.SendAndClose(&gitalypb.UpdateRemoteMirrorResponse{})
}

// refsOf adapts closure heads into kit.BundleRef entries.
func refsOf(refs []kit.ListedRef, heads []model.OID) []kit.BundleRef {
	byOID := map[model.OID]string{}
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "refs/heads/") {
			byOID[r.Head] = r.Name
		}
	}
	var out []kit.BundleRef
	for _, h := range heads {
		if n, ok := byOID[h]; ok {
			out = append(out, kit.BundleRef{OID: h, Ref: n})
		}
	}
	return out
}

type remoteService struct {
	gitalypb.UnimplementedRemoteServiceServer
	*kit.Base
}
