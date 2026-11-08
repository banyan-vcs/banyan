package ref

import (
	"context"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// refService implements the ref read/write slice of gitaly.RefService:
// branch/tag discovery, existence probes, reverse lookup and ref updates.
// Signature-bearing RPCs (GetTagSignatures) are later slices.
type refService struct {
	gitalypb.UnimplementedRefServiceServer
	*kit.Base
}

func (rf *refService) FindDefaultBranchName(ctx context.Context, req *gitalypb.FindDefaultBranchNameRequest) (*gitalypb.FindDefaultBranchNameResponse, error) {
	name, err := rf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	def, err := rf.DefaultBranch(ctx, name)
	if err != nil {
		return nil, err
	}
	// head_only: the HEAD symref target, full name — even when the ref is
	// unborn (upstream answers the dangling target for an empty repo).
	if req.GetHeadOnly() {
		return &gitalypb.FindDefaultBranchNameResponse{Name: []byte("refs/heads/" + def)}, nil
	}
	// Heuristic (upstream's test table): the fully-qualified default branch
	// when it exists — the contract is the full name: Rails feeds it into
	// Gitlab::Git.branch_name, which nils anything without the refs/heads/
	// prefix; when it doesn't, the first existing branch; no branches at
	// all → empty.
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	want := "refs/heads/" + def
	first := ""
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, "refs/heads/") {
			continue
		}
		if r.Name == want {
			return &gitalypb.FindDefaultBranchNameResponse{Name: []byte(want)}, nil
		}
		if first == "" || r.Name < first {
			first = r.Name
		}
	}
	if first != "" {
		return &gitalypb.FindDefaultBranchNameResponse{Name: []byte(first)}, nil
	}
	return &gitalypb.FindDefaultBranchNameResponse{}, nil
}

func (rf *refService) FindBranch(ctx context.Context, req *gitalypb.FindBranchRequest) (*gitalypb.FindBranchResponse, error) {
	name, err := rf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	want := string(req.GetName())
	if !strings.HasPrefix(want, "refs/") {
		want = "refs/heads/" + want
	}
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if r.Name != want {
			continue
		}
		commit, err := rf.GitCommit(ctx, name, r.Head)
		if err != nil {
			return nil, err
		}
		return &gitalypb.FindBranchResponse{Branch: &gitalypb.Branch{Name: []byte(want), TargetCommit: commit}}, nil
	}
	return nil, status.Errorf(codes.NotFound, "gitaly: branch %q not found", string(req.GetName()))
}

func (rf *refService) RefExists(ctx context.Context, req *gitalypb.RefExistsRequest) (*gitalypb.RefExistsResponse, error) {
	name, err := rf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(string(req.GetRef()))
	if !strings.HasPrefix(want, "refs/") {
		return nil, status.Error(codes.InvalidArgument, "gitaly: ref must be fully qualified (refs/…)")
	}
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if r.Name == want {
			return &gitalypb.RefExistsResponse{Value: true}, nil
		}
	}
	return &gitalypb.RefExistsResponse{Value: false}, nil
}

func (rf *refService) FindAllBranches(req *gitalypb.FindAllBranchesRequest, stream gitalypb.RefService_FindAllBranchesServer) error {
	ctx := stream.Context()
	name, err := rf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	var mergedInto []model.OID
	if req.GetMergedOnly() {
		for _, b := range req.GetMergedBranches() {
			if oid, err := rf.ResolveRevision(ctx, name, string(b)); err == nil {
				mergedInto = append(mergedInto, oid)
			}
		}
	}
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, "refs/heads/") {
			continue
		}
		if len(mergedInto) > 0 {
			merged := false
			for _, tip := range mergedInto {
				if ok, _ := rf.IsAncestor(ctx, name, r.Head, tip); ok {
					merged = true
					break
				}
			}
			if !merged {
				continue
			}
		}
		commit, err := rf.GitCommit(ctx, name, r.Head)
		if err != nil {
			continue
		}
		if err := stream.Send(&gitalypb.FindAllBranchesResponse{
			Branches: []*gitalypb.FindAllBranchesResponse_Branch{{Name: []byte(r.Name), Target: commit}},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (rf *refService) FindAllTags(req *gitalypb.FindAllTagsRequest, stream gitalypb.RefService_FindAllTagsServer) error {
	tags, err := rf.tagsOf(stream.Context(), req.GetRepository())
	if err != nil {
		return err
	}
	for _, t := range tags {
		if err := stream.Send(&gitalypb.FindAllTagsResponse{Tags: []*gitalypb.Tag{t}}); err != nil {
			return err
		}
	}
	return nil
}

// tagsOf lists refs/tags/* in the upstream shape: lightweight tags carry the
// commit as Id; annotated tags carry the tag object as Id.
func (rf *refService) tagsOf(ctx context.Context, repo *gitalypb.Repository) ([]*gitalypb.Tag, error) {
	name, err := rf.ExistingRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	var out []*gitalypb.Tag
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, "refs/tags/") {
			continue
		}
		short := strings.TrimPrefix(r.Name, "refs/tags/")
		target := r.Head
		if typ, _, err := rf.Objects.Raw(ctx, name, r.Head); err == nil && typ == model.TagType {
			if target, err = rf.PeelToCommit(ctx, name, r.Head); err != nil {
				continue
			}
		}
		commit, err := rf.GitCommit(ctx, name, target)
		if err != nil {
			continue
		}
		out = append(out, &gitalypb.Tag{Name: []byte(short), Id: r.Head.String(), TargetCommit: commit})
	}
	return out, nil
}

func (rf *refService) FindTag(ctx context.Context, req *gitalypb.FindTagRequest) (*gitalypb.FindTagResponse, error) {
	tags, err := rf.tagsOf(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		if string(t.Name) == string(req.GetTagName()) {
			return &gitalypb.FindTagResponse{Tag: t}, nil
		}
	}
	// Missing tags answer NotFound carrying the structured FindTagError
	// detail — Rails' wraps_gitaly_errors maps the detail to ReferenceNotFoundError,
	// which find_tag rescues into nil; a bare NotFound would surface as
	// NoRepository and 500 the caller (the demo's archive bug).
	st := status.New(codes.NotFound, "tag does not exist")
	st, _ = st.WithDetails(&gitalypb.FindTagError{
		Error: &gitalypb.FindTagError_TagNotFound{
			TagNotFound: &gitalypb.ReferenceNotFoundError{
				ReferenceName: append([]byte("refs/tags/"), req.GetTagName()...),
			},
		},
	})
	return nil, st.Err()
}

func (rf *refService) FindAllRemoteBranches(req *gitalypb.FindAllRemoteBranchesRequest, stream gitalypb.RefService_FindAllRemoteBranchesServer) error {
	// Banyan stores no remote-tracking refs — remote branches surface through
	// the fetch path instead. An empty stream is the honest answer.
	_, err := rf.ExistingRepo(stream.Context(), req.GetRepository())
	return err
}

func (rf *refService) ListRefs(req *gitalypb.ListRefsRequest, stream gitalypb.RefService_ListRefsServer) error {
	ctx := stream.Context()
	name, err := rf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	pointAt := map[string]bool{}
	for _, o := range req.GetPointingAtOids() {
		pointAt[string(o)] = true
	}
	skip := uint32(0)
	limit := uint32(0) // 0 = unbounded
	if p := req.GetPaginationParams(); p != nil {
		if tok := p.GetPageToken(); tok != "" {
			if !isAllDigits(tok) {
				return status.Error(codes.InvalidArgument, "gitaly: malformed pagination token")
			}
			for _, c := range tok {
				skip = skip*10 + uint32(c-'0')
			}
		}
		if l := p.GetLimit(); l > 0 {
			limit = uint32(l)
		}
	}
	sent := uint32(0)
	for _, r := range refs {
		if r.Name == "HEAD" {
			if !req.GetHead() {
				continue
			}
		} else if pats := req.GetPatterns(); len(pats) > 0 && !matchAnyGlob(pats, r.Name) {
			continue
		}
		target := r.Head.String()
		peeled := ""
		if req.GetPeelTags() && strings.HasPrefix(r.Name, "refs/tags/") {
			if typ, _, err := rf.Objects.Raw(ctx, name, r.Head); err == nil && typ == model.TagType {
				if commit, err := rf.PeelToCommit(ctx, name, r.Head); err == nil {
					peeled = commit.String()
				}
			}
		}
		if len(pointAt) > 0 && !pointAt[target] && (peeled == "" || !pointAt[peeled]) {
			continue
		}
		if sent >= skip {
			ref := &gitalypb.ListRefsResponse_Reference{Name: []byte(r.Name), Target: target, PeeledTarget: peeled}
			if err := stream.Send(&gitalypb.ListRefsResponse{References: []*gitalypb.ListRefsResponse_Reference{ref}}); err != nil {
				return err
			}
		}
		sent++
		if limit > 0 && sent-skip >= limit && sent < uint32(len(refs)) {
			return stream.Send(&gitalypb.ListRefsResponse{
				PaginationCursor: &gitalypb.PaginationCursor{NextCursor: kit.Itoa(sent)},
			})
		}
	}
	return nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// matchAnyGlob applies for-each-ref argument semantics (git ref-filter's
// match_name_as_path): a pattern matches when it is a literal prefix of
// the ref name ending at a path boundary — "refs/heads" and "refs/heads/"
// both match "refs/heads/main"; this is the form Rails' list_refs sends —
// or a glob where '*' spans separators.
func matchAnyGlob(patterns [][]byte, name string) bool {
	for _, p := range patterns {
		pat := string(p)
		if n := len(pat); n <= len(name) && strings.HasPrefix(name, pat) &&
			(len(name) == n || name[n] == '/' || (n > 0 && pat[n-1] == '/')) {
			return true
		}
		if kit.GlobMatch(pat, name) {
			return true
		}
	}
	return false
}

func (rf *refService) FindRefsByOID(ctx context.Context, req *gitalypb.FindRefsByOIDRequest) (*gitalypb.FindRefsByOIDResponse, error) {
	name, err := rf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return nil, err
	}
	var out []string
	oid := strings.ToLower(req.GetOid())
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		if strings.EqualFold(r.Head.String(), oid) {
			out = append(out, r.Name)
			continue
		}
		if typ, _, err := rf.Objects.Raw(ctx, name, r.Head); err == nil && typ == model.TagType {
			if commit, err := rf.PeelToCommit(ctx, name, r.Head); err == nil && strings.EqualFold(commit.String(), oid) {
				out = append(out, r.Name)
			}
		}
	}
	if len(out) == 0 {
		return nil, status.Errorf(codes.NotFound, "gitaly: no ref points at %s", req.GetOid())
	}
	return &gitalypb.FindRefsByOIDResponse{Refs: out}, nil
}

func (rf *refService) ListBranchNamesContainingCommit(req *gitalypb.ListBranchNamesContainingCommitRequest, stream gitalypb.RefService_ListBranchNamesContainingCommitServer) error {
	return rf.namesContainingCommit(stream.Context(), req.GetRepository(), req.GetCommitId(), "refs/heads/",
		func(names [][]byte) error {
			return stream.Send(&gitalypb.ListBranchNamesContainingCommitResponse{BranchNames: names})
		})
}

func (rf *refService) ListTagNamesContainingCommit(req *gitalypb.ListTagNamesContainingCommitRequest, stream gitalypb.RefService_ListTagNamesContainingCommitServer) error {
	return rf.namesContainingCommit(stream.Context(), req.GetRepository(), req.GetCommitId(), "refs/tags/",
		func(names [][]byte) error {
			return stream.Send(&gitalypb.ListTagNamesContainingCommitResponse{TagNames: names})
		})
}

func (rf *refService) namesContainingCommit(ctx context.Context, repo *gitalypb.Repository, commitID string, prefix string, send func([][]byte) error) error {
	name, err := rf.ExistingRepo(ctx, repo)
	if err != nil {
		return err
	}
	target := model.OID(strings.ToLower(commitID))
	refs, err := rf.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	var batch [][]byte
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		tip := r.Head
		if typ, _, err := rf.Objects.Raw(ctx, name, tip); err == nil && typ == model.TagType {
			if peeled, err := rf.PeelToCommit(ctx, name, tip); err == nil {
				tip = peeled
			}
		}
		if ok, _ := rf.IsAncestor(ctx, name, target, tip); ok {
			batch = append(batch, []byte(strings.TrimPrefix(r.Name, prefix)))
		}
	}
	if len(batch) > 0 {
		return send(batch)
	}
	return nil
}

// UpdateReferences applies ref writes (client-streamed batch): non-empty new
// OID creates/moves a ref (branches via root timelines, everything else as
// verbatim aux refs — the same dual storage gitwire's receive-pack uses),
// empty new OID deletes. Old OIDs are advisory; banyan's ref plane serializes
// writers, so races resolve last-write-wins rather than failing the batch.
func (rf *refService) UpdateReferences(stream gitalypb.RefService_UpdateReferencesServer) error {
	ctx := stream.Context()
	var name string
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			name, err = rf.ExistingRepo(ctx, req.GetRepository())
			if err != nil {
				return err
			}
		}
		for _, u := range req.GetUpdates() {
			if err := rf.applyUpdate(ctx, name, u); err != nil {
				return err
			}
		}
	}
	return stream.SendAndClose(&gitalypb.UpdateReferencesResponse{})
}

func (rf *refService) applyUpdate(ctx context.Context, name string, u *gitalypb.UpdateReferencesRequest_Update) error {
	refName := string(u.GetReference())
	if !strings.HasPrefix(refName, "refs/") {
		return status.Errorf(codes.InvalidArgument, "gitaly: ref %q must be fully qualified", refName)
	}
	newOID := model.OID(strings.ToLower(string(u.GetNewObjectId())))
	if err := rf.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: refName, New: newOID}); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return status.Errorf(codes.NotFound, "gitaly: %v", err)
		}
		return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
	}
	return nil
}

func (rf *refService) DeleteRefs(ctx context.Context, req *gitalypb.DeleteRefsRequest) (*gitalypb.DeleteRefsResponse, error) {
	name, err := rf.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	for _, refBytes := range req.GetRefs() {
		refName := string(refBytes)
		if !strings.HasPrefix(refName, "refs/") {
			return nil, status.Errorf(codes.InvalidArgument, "gitaly: ref %q must be fully qualified", refName)
		}
		if err := rf.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: refName}); err != nil {
			if strings.Contains(err.Error(), "not found") {
				continue // already gone
			}
			return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	return &gitalypb.DeleteRefsResponse{}, nil
}
