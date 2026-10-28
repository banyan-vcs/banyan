package gitaly

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// Repository maintenance surface: deletion, integrity probes, checksums,
// housekeeping no-ops and the raw-change/config readers. GetArchive lives in
// archive.go.

// checksumRefsAllowed mirrors upstream's git-show-ref filter: only HEAD and
// the well-known ref namespaces feed the checksum.
var checksumRefsAllowed = regexp.MustCompile(`\AHEAD|(refs/(heads|tags|keep-around|merge-requests|environments|notes)/)`)

// CalculateChecksum reproduces gitaly's XOR-of-SHA1 checksum over the
// `git show-ref --head` line set (order-independent by construction). An
// empty reference set answers the all-zero checksum.
func (rs *repositoryService) CalculateChecksum(ctx context.Context, req *gitalypb.CalculateChecksumRequest) (*gitalypb.CalculateChecksumResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	refs, err := rs.s.listRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	var sum big.Int
	for _, r := range refs {
		if !checksumRefsAllowed.MatchString(r.Name) {
			continue
		}
		h := sha1.Sum([]byte(string(r.Head) + " " + r.Name))
		var one big.Int
		one.SetBytes(h[:])
		if sum.BitLen() == 0 {
			sum.Set(&one)
		} else {
			sum.Xor(&sum, &one)
		}
	}
	out := "0000000000000000000000000000000000000000"
	if sum.BitLen() != 0 {
		out = hex.EncodeToString(sum.Bytes())
	}
	return &gitalypb.CalculateChecksumResponse{Checksum: out}, nil
}

// Fsck walks every reference's reachable object closure and reports the
// first corruption signals it finds: objects that will not read or parse.
// The error text lands in the response (the RPC itself succeeds), mirroring
// how upstream surfaces `git fsck` output.
func (rs *repositoryService) Fsck(ctx context.Context, req *gitalypb.FsckRequest) (*gitalypb.FsckResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	refs, err := rs.s.listRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	var problems []string
	seen := map[model.OID]bool{}
	lookup := rs.s.Objects.Lookup(name)

	var checkTree func(oid model.OID, path string)
	checkTree = func(oid model.OID, path string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		t, err := lookup.Tree(oid)
		if err != nil {
			problems = append(problems, fmt.Sprintf("error in tree %s: %v", oid, err))
			return
		}
		for _, e := range t.Entries {
			if e.IsDir {
				checkTree(e.Target, path+e.Name+"/")
				continue
			}
			if e.Mode == "160000" || seen[e.Target] {
				continue // gitlinks point into the submodule's repository
			}
			seen[e.Target] = true
			typ, _, err := rs.s.Objects.Raw(ctx, name, e.Target)
			if err != nil || typ != model.BlobType {
				problems = append(problems, fmt.Sprintf("error in blob %s (%s%s): %v", e.Target, path, e.Name, err))
			}
		}
	}
	for _, r := range refs {
		oid := r.Head
		typ, raw, err := rs.s.Objects.Raw(ctx, name, oid)
		if err != nil {
			problems = append(problems, fmt.Sprintf("error in ref %s: %s unreadable: %v", r.Name, oid, err))
			continue
		}
		if typ == model.TagType {
			if _, err := model.DecodeTag(raw); err != nil {
				problems = append(problems, fmt.Sprintf("error in tag %s: %v", oid, err))
			}
			continue // the peel closure surfaces through the branch heads
		}
		if typ != model.CommitType {
			problems = append(problems, fmt.Sprintf("error in ref %s: %s is a %s, want commit", r.Name, oid, typ))
			continue
		}
		seen[oid] = true
		if _, err := model.DecodeCommit(raw); err != nil {
			problems = append(problems, fmt.Sprintf("error in commit %s: %v", oid, err))
			continue
		}
		if rec, err := rs.s.commitRecord(ctx, name, oid); err == nil {
			checkTree(rec.Tree, "")
		}
	}
	if len(problems) == 0 {
		return &gitalypb.FsckResponse{}, nil
	}
	return &gitalypb.FsckResponse{Error: []byte(strings.Join(problems, "\n") + "\n")}, nil
}

// RemoveRepository drops the repository: storage bytes first (named
// addressing), then every registry row. Missing repositories succeed —
// deletion is idempotent, matching upstream's remove semantics.
func (rs *repositoryService) RemoveRepository(ctx context.Context, req *gitalypb.RemoveRepositoryRequest) (*gitalypb.RemoveRepositoryResponse, error) {
	name, err := rs.s.repoName(req.GetRepository())
	if err != nil {
		return nil, err
	}
	if err := rs.s.Objects.DeleteRepo(ctx, name); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.RemoveRepositoryResponse{}, nil
}

// RepositoryInfo answers the stats GitLab's housekeeping view needs. Banyan
// maps onto the single object plane: everything is packed, references live
// in the metadata registry (files-backend semantics), and the sizes come
// from the same source as RepositorySize.
func (rs *repositoryService) RepositoryInfo(ctx context.Context, req *gitalypb.RepositoryInfoRequest) (*gitalypb.RepositoryInfoResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	size, _, err := rs.s.Svc.RepoSize(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	refs, err := rs.s.listRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	packs, err := rs.s.Meta.ListPacks(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.RepositoryInfoResponse{
		Size: uint64(size),
		References: &gitalypb.RepositoryInfoResponse_ReferencesInfo{
			LooseCount:       uint64(len(refs)),
			ReferenceBackend: gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES,
		},
		Objects: &gitalypb.RepositoryInfoResponse_ObjectsInfo{
			Size:          uint64(size),
			RecentSize:    uint64(size),
			PackfileCount: uint64(len(packs)),
		},
	}, nil
}

// OptimizeRepository: banyan's object plane is always compact (immutable
// packs, no loose-object drift), so every on-line strategy is a successful
// no-op. The offloading pair matches upstream: Unimplemented unless wired.
func (rs *repositoryService) OptimizeRepository(ctx context.Context, req *gitalypb.OptimizeRepositoryRequest) (*gitalypb.OptimizeRepositoryResponse, error) {
	if _, err := rs.s.existingRepo(ctx, req.GetRepository()); err != nil {
		return nil, err
	}
	switch req.GetStrategy() {
	case gitalypb.OptimizeRepositoryRequest_STRATEGY_UNSPECIFIED,
		gitalypb.OptimizeRepositoryRequest_STRATEGY_HEURISTICAL,
		gitalypb.OptimizeRepositoryRequest_STRATEGY_EAGER:
		return &gitalypb.OptimizeRepositoryResponse{}, nil
	case gitalypb.OptimizeRepositoryRequest_STRATEGY_OFFLOADING,
		gitalypb.OptimizeRepositoryRequest_STRATEGY_REHYDRATION:
		return nil, status.Error(codes.Unimplemented, "gitaly: offloading feature not enabled")
	default:
		return nil, status.Error(codes.InvalidArgument, "gitaly: unknown strategy")
	}
}

// PruneUnreachableObjects: with immutable packs and registry-driven
// lifetime, unreachable objects never outlive their repository — nothing to
// prune, and the call succeeds.
func (rs *repositoryService) PruneUnreachableObjects(ctx context.Context, req *gitalypb.PruneUnreachableObjectsRequest) (*gitalypb.PruneUnreachableObjectsResponse, error) {
	if _, err := rs.s.existingRepo(ctx, req.GetRepository()); err != nil {
		return nil, err
	}
	return &gitalypb.PruneUnreachableObjectsResponse{}, nil
}

// GetConfig: banyan holds no git config file. An empty stream is the honest
// answer (callers treat it as "no keys set"), rather than Unimplemented —
// Rails probes it for optional knobs.
func (rs *repositoryService) GetConfig(req *gitalypb.GetConfigRequest, stream gitalypb.RepositoryService_GetConfigServer) error {
	if _, err := rs.s.existingRepo(stream.Context(), req.GetRepository()); err != nil {
		return err
	}
	return nil
}

// GetInfoAttributes serves .gitattributes from the tip tree when present.
func (rs *repositoryService) GetInfoAttributes(req *gitalypb.GetInfoAttributesRequest, stream gitalypb.RepositoryService_GetInfoAttributesServer) error {
	ctx := stream.Context()
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	oid, err := rs.s.resolveRevision(ctx, name, "HEAD")
	if err != nil {
		return err
	}
	rec, err := rs.s.commitRecord(ctx, name, oid)
	if err != nil {
		return err
	}
	entry, err := rs.s.entryAtPath(ctx, name, rec.Tree, ".gitattributes")
	if err != nil || entry == nil || entry.IsDir {
		return nil // no attributes file: empty stream
	}
	_, raw, err := rs.s.Objects.Raw(ctx, name, entry.Target)
	if err != nil {
		return err
	}
	return streamBytes(raw, func(b []byte) error {
		return stream.Send(&gitalypb.GetInfoAttributesResponse{Attributes: b})
	})
}

// ObjectsSize sums the object bytes reachable from the streamed revisions
// (rev-list --objects closure). banyan reports uncompressed object sizes —
// the pack layout makes per-object on-disk bytes an implementation detail.
func (rs *repositoryService) ObjectsSize(stream gitalypb.RepositoryService_ObjectsSizeServer) error {
	ctx := stream.Context()
	var name string
	var roots []model.OID
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			if name, err = rs.s.existingRepo(ctx, req.GetRepository()); err != nil {
				return err
			}
		} else if req.GetRepository() != nil {
			return status.Error(codes.InvalidArgument, "gitaly: subsequent requests must not contain repository")
		}
		if len(req.GetRevisions()) == 0 {
			return status.Error(codes.InvalidArgument, "gitaly: no revisions specified")
		}
		for _, rev := range req.GetRevisions() {
			oid, err := rs.s.resolveRevision(ctx, name, string(rev))
			if err != nil {
				return status.Errorf(codes.NotFound, "gitaly: revision not found: %s", string(rev))
			}
			roots = append(roots, oid)
		}
	}
	total, err := rs.s.reachableObjectBytes(ctx, name, roots)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendAndClose(&gitalypb.ObjectsSizeResponse{Size: total})
}

// GetRawChanges streams the file-level changes of to_revision's first
// parent (or the empty tree for a root commit), `git diff --raw` semantics
// without rename detection.
func (rs *repositoryService) GetRawChanges(req *gitalypb.GetRawChangesRequest, stream gitalypb.RepositoryService_GetRawChangesServer) error {
	ctx := stream.Context()
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	toOID, err := rs.s.resolveRevision(ctx, name, req.GetToRevision())
	if err != nil {
		return err
	}
	toRec, err := rs.s.commitRecord(ctx, name, toOID)
	if err != nil {
		return err
	}
	fromTree := model.OID("") // zero = empty tree (root-commit shape)
	if req.GetFromRevision() != "" {
		fromOID, err := rs.s.resolveRevision(ctx, name, req.GetFromRevision())
		if err != nil {
			return err
		}
		fromRec, err := rs.s.commitRecord(ctx, name, fromOID)
		if err != nil {
			return err
		}
		fromTree = fromRec.Tree
	}
	changes, err := rs.s.treeDiff(ctx, name, fromTree, toRec.Tree)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	var batch []*gitalypb.GetRawChangesResponse_RawChange
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out := batch
		batch = nil
		return stream.Send(&gitalypb.GetRawChangesResponse{RawChanges: out})
	}
	for _, c := range changes {
		rc := &gitalypb.GetRawChangesResponse_RawChange{
			OldPathBytes: []byte(c.path), NewPathBytes: []byte(c.path),
		}
		rc.OldMode = int32(parseMode(c.oldMode))
		rc.NewMode = int32(parseMode(c.newMode))
		switch c.status() {
		case 'A':
			rc.Operation = gitalypb.GetRawChangesResponse_RawChange_ADDED
		case 'D':
			rc.Operation = gitalypb.GetRawChangesResponse_RawChange_DELETED
		default:
			rc.Operation = gitalypb.GetRawChangesResponse_RawChange_MODIFIED
		}
		blobOID, blobMode := c.newOID, c.newMode
		if blobOID.IsZero() {
			blobOID, blobMode = c.oldOID, c.oldMode
		}
		if blobMode == "160000" {
			rc.BlobId = "" // submodule pointer: no blob to size
		} else if typ, raw, rerr := rs.s.Objects.Raw(ctx, name, blobOID); rerr == nil && typ == model.BlobType {
			rc.BlobId = blobOID.String()
			rc.Size = int64(len(raw))
		}
		batch = append(batch, rc)
		if len(batch) >= 100 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// parseMode turns a tree-entry mode ("100644") into its integer form.
func parseMode(mode string) int64 {
	v, _ := strconv.ParseInt(mode, 8, 64)
	return v
}

// reachableObjectBytes walks the commit closure of roots and sums the
// uncompressed bytes of every distinct commit, tree and blob reached.
func (s *Server) reachableObjectBytes(ctx context.Context, repo string, roots []model.OID) (uint64, error) {
	lookup := s.Objects.Lookup(repo)
	var total uint64
	seen := map[model.OID]bool{}
	var addTree func(oid model.OID) error
	addTree = func(oid model.OID) error {
		if seen[oid] {
			return nil
		}
		seen[oid] = true
		t, err := lookup.Tree(oid)
		if err != nil {
			return nil // unreadable subtree: count what is readable
		}
		if _, raw, err := s.Objects.Raw(ctx, repo, oid); err == nil {
			total += uint64(len(raw))
		}
		for _, e := range t.Entries {
			if e.IsDir {
				if err := addTree(e.Target); err != nil {
					return err
				}
				continue
			}
			if e.Mode == "160000" || seen[e.Target] {
				continue
			}
			seen[e.Target] = true
			if _, raw, err := s.Objects.Raw(ctx, repo, e.Target); err == nil {
				total += uint64(len(raw))
			}
		}
		return nil
	}
	if err := s.walkParents(ctx, repo, roots, &walkFilter{}, func(oid model.OID, rec *commitRecord) bool {
		if !seen[oid] {
			seen[oid] = true
			if _, raw, err := s.Objects.Raw(ctx, repo, oid); err == nil {
				total += uint64(len(raw))
			}
		}
		_ = addTree(rec.Tree)
		return true
	}); err != nil {
		return 0, err
	}
	return total, nil
}
