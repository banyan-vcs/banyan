package repository

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/banyan-vcs/banyan/internal/gitaly/backup"
	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Snapshots, replication and the bundle-URI generator (docs/19).
//
// GetSnapshot answers a bare-repository TAR — HEAD, packed-refs and a
// pack + idx pair — the same layout git itself works with, so the snapshot
// is consumable by plain git (unpacked, cloned from) and by
// CreateRepositoryFromSnapshot on the way back in. ReplicateRepository is
// the object-plane mirror of the same state. GenerateBundleURI writes a
// fresh bundle into the backup sink's bundle-uri area (upstream's sink key
// scheme); the smart-HTTP advertisement stays config-driven.

// GetSnapshot streams the repository state as a bare-repo TAR.
func (rs *repositoryService) GetSnapshot(req *gitalypb.GetSnapshotRequest, stream gitalypb.RepositoryService_GetSnapshotServer) error {
	ctx := stream.Context()
	name, err := rs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	return rs.WriteSnapshot(ctx, name, &chunkWriter{send: func(p []byte) error {
		return stream.Send(&gitalypb.GetSnapshotResponse{Data: p})
	}})
}

func sha1Sum(b []byte) []byte {
	s := sha1.Sum(b)
	return s[:]
}

type chunkWriter struct{ send func([]byte) error }

func (c *chunkWriter) Write(p []byte) (int, error) {
	if err := c.send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// snapshotFile is one extracted member of a snapshot TAR.
type snapshotFile struct {
	name string
	data []byte
}

// extractSnapshotTar pulls the members of a snapshot TAR into memory (repo
// snapshots are pack-shaped; the few control files ride along).
func extractSnapshotTar(data []byte) (map[string][]byte, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[hdr.Name] = body
	}
	return out, nil
}

// CreateRepositoryFromSnapshot downloads a snapshot TAR over HTTP and
// seeds the new repository from it. A failed ingest rolls the repository
// back (the CreateRepositoryFrom* family contract).
func (rs *repositoryService) CreateRepositoryFromSnapshot(ctx context.Context, req *gitalypb.CreateRepositoryFromSnapshotRequest) (*gitalypb.CreateRepositoryFromSnapshotResponse, error) {
	url := req.GetHttpUrl()
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, status.Error(codes.InvalidArgument, "gitaly: snapshot URL must be http(s)")
	}
	if err := rs.createRepositoryChecked(ctx, req.GetRepository()); err != nil {
		return nil, err
	}
	name := req.GetRepository().GetRelativePath()
	rollback := func() {
		_ = rs.Meta.DeleteRepo(ctx, name)
		_ = rs.Objects.DeleteRepo(ctx, name)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		rollback()
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if auth := req.GetHttpAuth(); auth != "" {
		httpReq.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		rollback()
		return nil, status.Errorf(codes.Unavailable, "gitaly: fetch snapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rollback()
		return nil, status.Errorf(codes.Unavailable, "gitaly: snapshot fetch: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, kit.MaxWireMsg))
	if err != nil {
		rollback()
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	files, err := extractSnapshotTar(raw)
	if err != nil {
		rollback()
		return nil, status.Error(codes.InvalidArgument, "gitaly: snapshot tar: "+err.Error())
	}

	// Ingest every pack member.
	for name_, data := range files {
		if !strings.HasPrefix(name_, "objects/pack/") || !strings.HasSuffix(name_, ".pack") {
			continue
		}
		dir, err := os.MkdirTemp("", "banyan-snapshot-*")
		if err != nil {
			rollback()
			return nil, status.Error(codes.Internal, err.Error())
		}
		packPath := filepath.Join(dir, "snapshot.pack")
		if err := os.WriteFile(packPath, data, 0o644); err != nil {
			os.RemoveAll(dir)
			rollback()
			return nil, status.Error(codes.Internal, err.Error())
		}
		_, err = rs.Objects.IngestPackFile(ctx, req.GetRepository().GetRelativePath(), packPath, int64(len(data)))
		os.RemoveAll(dir)
		if err != nil {
			rollback()
			return nil, status.Errorf(codes.InvalidArgument, "gitaly: snapshot pack rejected: %v", err)
		}
	}

	// Refs from packed-refs; HEAD picks the default branch back up.
	if pr, ok := files["packed-refs"]; ok {
		for _, line := range strings.Split(string(pr), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || strings.HasSuffix(fields[1], "^{}") {
				continue
			}
			if err := rs.Svc.UpdateRef(ctx, service.RefUpdate{
				Repo: name, Name: fields[1], New: model.OID(fields[0])}); err != nil {
				rollback()
				return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
			}
		}
	}
	if head, ok := files["HEAD"]; ok {
		if target := strings.TrimSpace(strings.TrimPrefix(string(head), "ref: ")); strings.HasPrefix(target, "refs/heads/") {
			_ = rs.Meta.SetDefaultBranch(ctx, name, strings.TrimPrefix(target, "refs/heads/"))
		}
	}
	return &gitalypb.CreateRepositoryFromSnapshotResponse{}, nil
}

// ReplicateRepository mirrors one repository's complete state onto another
// (the praefect replication surface, served by the object plane): every
// ref's closure, the refs themselves, HEAD's default branch and the
// custom-hooks tarball.
func (rs *repositoryService) ReplicateRepository(ctx context.Context, req *gitalypb.ReplicateRepositoryRequest) (*gitalypb.ReplicateRepositoryResponse, error) {
	source, err := rs.ExistingRepo(ctx, req.GetSource())
	if err != nil {
		return nil, err
	}
	target := req.GetRepository().GetRelativePath()

	// A replication replaces the destination wholesale.
	if _, terr := rs.ExistingRepo(ctx, req.GetRepository()); terr == nil {
		if derr := rs.Meta.DeleteRepo(ctx, target); derr != nil {
			return nil, status.Error(codes.Internal, derr.Error())
		}
		if derr := rs.Objects.DeleteRepo(ctx, target); derr != nil {
			return nil, status.Error(codes.Internal, derr.Error())
		}
	}
	if err := rs.Meta.CreateRepo(ctx, target, rs.Algo); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	refs, err := rs.AdvertisedRefs(ctx, source)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		tip := r.Head
		if typ, _, terr := rs.Objects.Raw(ctx, source, tip); terr == nil && typ == model.TagType {
			if peeled, perr := rs.PeelToCommit(ctx, source, tip); perr == nil {
				// The tag object itself plus its commit closure.
				typ2, raw, _ := rs.Objects.Raw(ctx, source, tip)
				if typ2 != "" {
					_ = rs.Svc.PutObjects(ctx, target, []service.PutObject{{
						OID: tip, Type: string(typ2), Data: raw}})
				}
				tip = peeled
			}
		}
		if err := rs.CopyClosure(ctx, source, target, tip); err != nil {
			return nil, status.Errorf(codes.Internal, "gitaly: %v", err)
		}
		if err := rs.Svc.UpdateRef(ctx, service.RefUpdate{
			Repo: target, Name: r.Name, New: r.Head}); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	if def, derr := rs.DefaultBranch(ctx, source); derr == nil {
		_ = rs.Meta.SetDefaultBranch(ctx, target, def)
	}
	if hooks, ok, herr := rs.Meta.GetRepoBlob(ctx, source, kit.CustomHooksKey); herr == nil && ok {
		_ = rs.Meta.SetRepoBlob(ctx, target, kit.CustomHooksKey, hooks)
	}
	return &gitalypb.ReplicateRepositoryResponse{}, nil
}

// GenerateBundleURI writes a fresh full bundle for the repository into the
// backup sink's bundle-uri area (upstream's sink key scheme:
// <storage>/<relative_path>/uri/default.bundle — each run overwrites).
// The response is empty — upstream's callers learn the URI from the
// sink/CDN, not the RPC.
func (rs *repositoryService) GenerateBundleURI(ctx context.Context, req *gitalypb.GenerateBundleURIRequest) (*gitalypb.GenerateBundleURIResponse, error) {
	run := backup.Runner{Base: rs.Base}
	sink, err := run.SinkForNode()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "server-side backups are not configured")
	}
	name, err := rs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	bundle, err := run.FullBundle(ctx, name, mustRefs(ctx, rs, name))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: %v", err)
	}
	rel := fmt.Sprintf("%s/%s/uri/default.bundle", req.GetRepository().GetStorageName(),
		req.GetRepository().GetRelativePath())
	if err := sink.Write(rel, bundle); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.GenerateBundleURIResponse{}, nil
}

func mustRefs(ctx context.Context, rs *repositoryService, name string) []kit.ListedRef {
	refs, err := rs.AdvertisedRefs(ctx, name)
	if err != nil {
		return nil
	}
	return refs
}
