package gitaly

import (
	"strings"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// Ref read extras: paginated local-branch listing and the annotated-tag
// signature/message readers.

// FindLocalBranches streams refs/heads/* with name sorting, pagination and
// exclude patterns (`for-each-ref refs/heads` shape).
func (rf *refService) FindLocalBranches(req *gitalypb.FindLocalBranchesRequest, stream gitalypb.RefService_FindLocalBranchesServer) error {
	ctx := stream.Context()
	name, err := rf.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	refs, err := rf.s.listRefs(ctx, name)
	if err != nil {
		return err
	}

	skip, limit := paginationWindow(req.GetPaginationParams())
	sent := uint32(0)
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, "refs/heads/") {
			continue
		}
		if matchAnyGlob(req.GetExcludePatterns(), r.Name) {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		if limit > 0 && sent >= limit {
			break
		}
		commit, err := rf.s.gitCommit(ctx, name, r.Head)
		if err != nil {
			continue
		}
		if err := stream.Send(&gitalypb.FindLocalBranchesResponse{
			LocalBranches: []*gitalypb.Branch{{
				Name:            []byte(strings.TrimPrefix(r.Name, "refs/heads/")),
				TargetCommit:    commit,
			}},
		}); err != nil {
			return err
		}
		sent++
	}
	return nil
}

// tagMessage extracts the body below a tag object's header block.
func tagMessage(raw []byte) string {
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] == 10 && raw[i+1] == 10 {
			return string(raw[i+2:])
		}
	}
	return ""
}

// GetTagSignatures streams the PGP block embedded in annotated tag objects
// (the tail of the message), plus the signed content above it.
func (rf *refService) GetTagSignatures(req *gitalypb.GetTagSignaturesRequest, stream gitalypb.RefService_GetTagSignaturesServer) error {
	ctx := stream.Context()
	name, err := rf.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	for _, rev := range req.GetTagRevisions() {
		oid, rerr := rf.s.resolveRevision(ctx, name, rev)
		if rerr != nil {
			continue
		}
		typ, raw, terr := rf.s.Objects.Raw(ctx, name, oid)
		if terr != nil || typ != model.TagType {
			continue
		}
		sig, content, ok := splitTagSignature(tagMessage(raw))
		if !ok {
			continue
		}
		if serr := stream.Send(&gitalypb.GetTagSignaturesResponse{
			Signatures: []*gitalypb.GetTagSignaturesResponse_TagSignature{{
				TagId: oid.String(), Signature: sig, Content: content,
			}}}); serr != nil {
			return serr
		}
	}
	return nil
}

// splitTagSignature separates the trailing PGP armor block of a tag
// message from the signed content above it.
func splitTagSignature(message string) (signature, content []byte, ok bool) {
	start := strings.Index(message, "-----BEGIN PGP SIGNATURE-----")
	if start < 0 {
		return nil, nil, false
	}
	end := strings.Index(message, "-----END PGP SIGNATURE-----")
	if end < 0 {
		return nil, nil, false
	}
	sig := strings.TrimSpace(message[start : end+len("-----END PGP SIGNATURE-----")])
	above := strings.TrimSuffix(message[:start], "\n")
	return []byte(sig), []byte(above), true
}

// GetTagMessages streams annotated-tag messages by tag object id.
func (rf *refService) GetTagMessages(req *gitalypb.GetTagMessagesRequest, stream gitalypb.RefService_GetTagMessagesServer) error {
	ctx := stream.Context()
	name, err := rf.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	for _, tid := range req.GetTagIds() {
		typ, raw, terr := rf.s.Objects.Raw(ctx, name, model.OID(strings.ToLower(tid)))
		if terr != nil || typ != model.TagType {
			continue
		}
		if serr := streamBytes([]byte(tagMessage(raw)), func(p []byte) error {
			return stream.Send(&gitalypb.GetTagMessagesResponse{Message: p, TagId: tid})
		}); serr != nil {
			return serr
		}
	}
	return nil
}

