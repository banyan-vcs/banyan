package gitaly

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// operationService implements gitaly.OperationService's user-facing write
// slice: branch/tag lifecycle, fast-forward and the web-edit commit flow
// (operations_commit.go). Merge/rebase/revert/squash land with the 3-way
// engine (docs/19 §5). Hooks are a no-op: banyan's gitaly face inherits the
// upstream trust model — the token-bearing caller is the frontend, receive
// hooks evaluate at the banyan gateway (docs/04 §7), not here.
type operationService struct {
	gitalypb.UnimplementedOperationServiceServer
	s *Server
}

// branchTimeline is the root-anchor timeline backing refs/heads/<name>.
func branchTimeline(repo, branch string) model.Timeline {
	return model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: branch}
}

// fullRefTimeline is the aux timeline backing a full refs/<ns>/<name>.
func fullRefTimeline(repo, ref string) model.Timeline {
	return model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: ref}
}

// timelineHead reads one timeline's head; a missing timeline answers (zero, false, nil).
func (os *operationService) timelineHead(ctx context.Context, repo string, tl model.Timeline) (model.OID, bool, error) {
	st, err := os.s.Meta.Timeline(ctx, tl)
	if err != nil {
		return "", false, nil // absent timeline: treated as an empty ref
	}
	return st.Head, !st.Head.IsZero(), nil
}

// branchState reads a branch's dual identity: advertised (the woven view
// every read and advertisement shows) and raw (the timeline head writes
// CAS against). Linear chains make them equal; merge commits linearize on
// the woven side (banyan's root-view model), so client expectations arrive
// as advertised OIDs while the CAS runs on the raw head.
func (os *operationService) branchState(ctx context.Context, repo, branch string) (advertised, raw model.OID, ok bool) {
	if tip, err := os.s.Svc.Views.RootTip(ctx, repo, branch); err == nil && !tip.IsZero() {
		advertised = tip
	}
	raw, hasRaw, _ := os.timelineHead(ctx, repo, branchTimeline(repo, branch))
	ok = !advertised.IsZero() || hasRaw
	return advertised, raw, ok
}

// validBranchName enforces git's refname rules (check-ref-format subset).
func validBranchName(name string) bool {
	if name == "" || name == "HEAD" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return false
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, bad := range []string{"..", "//", "@{", `\`, "~", "^", ":", "?", "[", "*"} {
		if strings.Contains(name, bad) {
			return false
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return false
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// peelToCommit follows tag chains down to the underlying commit.
func (os *operationService) peelToCommit(ctx context.Context, repo string, oid model.OID) (model.OID, error) {
	for i := 0; i < 8; i++ {
		typ, _, err := os.s.Objects.Raw(ctx, repo, oid)
		if err != nil {
			return "", status.Errorf(codes.NotFound, "gitaly: object %s not found", oid)
		}
		switch typ {
		case model.CommitType:
			return oid, nil
		case model.TagType:
			_, raw, err := os.s.Objects.Raw(ctx, repo, oid)
			if err != nil {
				return "", status.Errorf(codes.NotFound, "gitaly: tag %s unreadable", oid)
			}
			tag, err := model.DecodeTag(raw)
			if err != nil {
				return "", status.Errorf(codes.InvalidArgument, "gitaly: tag %s malformed: %v", oid, err)
			}
			oid = tag.Object
		default:
			return "", status.Errorf(codes.InvalidArgument, "gitaly: object %s is a %s, want commit", oid, typ)
		}
	}
	return "", status.Error(codes.InvalidArgument, "gitaly: tag chain too deep")
}

// resolveExpectedOld parses and resolves an expected-old OID for CAS checks.
func (os *operationService) resolveExpectedOld(ctx context.Context, repo, hex string) (model.OID, error) {
	if !isHexOID(hex) {
		return "", status.Errorf(codes.InvalidArgument, "invalid expected old object ID: %q", hex)
	}
	return model.OID(hex), nil
}

// casRef performs one compare-and-swap on the timeline behind a ref.
func (os *operationService) casRef(ctx context.Context, repo string, tl model.Timeline, expected, next model.OID) error {
	if !next.IsZero() {
		if _, _, err := os.s.Objects.Raw(ctx, repo, next); err != nil {
			return status.Errorf(codes.FailedPrecondition, "reference update failed: target %s missing", next)
		}
	}
	if err := os.s.Meta.CASHead(ctx, tl, expected, next); err != nil {
		return status.Error(codes.FailedPrecondition, "reference update failed")
	}
	return nil
}

// UserCreateBranch creates refs/heads/<branch> at a start point.
func (os *operationService) UserCreateBranch(ctx context.Context, req *gitalypb.UserCreateBranchRequest) (*gitalypb.UserCreateBranchResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	branch := string(req.GetBranchName())
	if !validBranchName(branch) {
		return nil, status.Error(codes.InvalidArgument, "branch name invalid")
	}
	start := string(req.GetStartPoint())
	if start == "" {
		start = "HEAD"
	}
	rev, err := os.s.resolveRevision(ctx, name, start)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "revspec '%s' not found", start)
	}
	rev, err = os.peelToCommit(ctx, name, rev)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "revspec '%s' not found", start)
	}
	tl := branchTimeline(name, branch)
	if _, ok, _ := os.timelineHead(ctx, name, tl); ok {
		return nil, status.Error(codes.FailedPrecondition, "reference already exists")
	}
	if err := os.casRef(ctx, name, tl, model.OID(""), rev); err != nil {
		return nil, err
	}
	commit, err := os.s.gitCommit(ctx, name, rev)
	if err != nil {
		return nil, err
	}
	return &gitalypb.UserCreateBranchResponse{
		Branch: &gitalypb.Branch{Name: []byte(branch), TargetCommit: commit},
	}, nil
}

// UserDeleteBranch removes refs/heads/<branch> with an optional CAS guard.
func (os *operationService) UserDeleteBranch(ctx context.Context, req *gitalypb.UserDeleteBranchRequest) (*gitalypb.UserDeleteBranchResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	branch := string(req.GetBranchName())
	if !validBranchName(branch) {
		return nil, status.Error(codes.InvalidArgument, "branch name invalid")
	}
	tl := branchTimeline(name, branch)
	raw, ok, _ := os.timelineHead(ctx, name, tl)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "branch not found: %q", branch)
	}
	if old := req.GetExpectedOldOid(); old != "" {
		want, err := os.resolveExpectedOld(ctx, name, old)
		if err != nil {
			return nil, err
		}
		if want != raw {
			advertised, _, _ := os.branchState(ctx, name, branch)
			if want != advertised {
				return nil, status.Error(codes.FailedPrecondition, "reference update failed")
			}
		}
	}
	if err := os.casRef(ctx, name, tl, raw, model.OID("")); err != nil {
		return nil, err
	}
	return &gitalypb.UserDeleteBranchResponse{}, nil
}

// UserUpdateBranch moves a branch to newrev (no hooks on this face).
func (os *operationService) UserUpdateBranch(ctx context.Context, req *gitalypb.UserUpdateBranchRequest) (*gitalypb.UserUpdateBranchResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	branch := string(req.GetBranchName())
	if !validBranchName(branch) {
		return nil, status.Error(codes.InvalidArgument, "branch name invalid")
	}
	newrev := string(req.GetNewrev())
	if !isHexOID(newrev) {
		return nil, status.Error(codes.Internal, "could not parse newrev")
	}
	tl := branchTimeline(name, branch)
	raw, ok, _ := os.timelineHead(ctx, name, tl)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "Could not update %s. Please refresh and try again.", branch)
	}
	if old := string(req.GetOldrev()); old != "" {
		if !isHexOID(old) {
			return nil, status.Error(codes.Internal, "could not parse oldrev")
		}
		if model.OID(old) != raw {
			advertised, _, _ := os.branchState(ctx, name, branch)
			if model.OID(old) != advertised {
				return nil, status.Errorf(codes.FailedPrecondition, "Could not update %s. Please refresh and try again.", branch)
			}
		}
	}
	if err := os.casRef(ctx, name, tl, raw, model.OID(newrev)); err != nil {
		return nil, err
	}
	return &gitalypb.UserUpdateBranchResponse{}, nil
}

// UserCreateTag creates refs/tags/<tag> — lightweight or annotated.
func (os *operationService) UserCreateTag(ctx context.Context, req *gitalypb.UserCreateTagRequest) (*gitalypb.UserCreateTagResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	tagName := string(req.GetTagName())
	if !validBranchName(tagName) {
		return nil, status.Error(codes.InvalidArgument, "tag name invalid")
	}
	target := string(req.GetTargetRevision())
	rev, err := os.s.resolveRevision(ctx, name, target)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "revspec '%s' not found", target)
	}
	commitOID, err := os.peelToCommit(ctx, name, rev)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "revspec '%s' not found", target)
	}
	tl := fullRefTimeline(name, "refs/tags/"+tagName)
	if _, ok, _ := os.timelineHead(ctx, name, tl); ok {
		return nil, status.Error(codes.AlreadyExists, "tag reference exists already")
	}

	tagOID := commitOID
	var message []byte
	var tagger *gitalypb.CommitAuthor
	if msg := req.GetMessage(); len(msg) > 0 {
		user := req.GetUser()
		if user == nil || len(user.GetName()) == 0 {
			return nil, status.Error(codes.InvalidArgument, "annotated tag requires a tagger identity")
		}
		when := time.Now()
		if ts := req.GetTimestamp(); ts != nil {
			when = ts.AsTime()
		}
		email := string(user.GetEmail())
		if email == "" {
			email = "banyan@local"
		}
		ident := model.Identity{Name: string(user.GetName()), Email: email, When: when}
		message = append(message, msg...)
		if len(message) > 0 && message[len(message)-1] != '\n' {
			message = append(message, '\n')
		}
		raw := []byte(fmt.Sprintf("object %s\ntype commit\ntag %s\ntagger %s\n\n%s",
			commitOID, tagName, ident.Format(), message))
		tagOID = model.ObjectOID(model.TagType, raw, os.s.Algo)
		if err := os.s.Svc.PutObjects(ctx, name, []service.PutObject{{OID: tagOID, Type: string(model.TagType), Data: raw}}); err != nil {
			return nil, status.Errorf(codes.Internal, "writing tag: %v", err)
		}
		tagger = &gitalypb.CommitAuthor{
			Name:  []byte(ident.Name),
			Email: []byte(ident.Email),
			Date:  timestamppb.New(when),
		}
	}
	if err := os.casRef(ctx, name, tl, model.OID(""), tagOID); err != nil {
		return nil, err
	}
	commit, err := os.s.gitCommit(ctx, name, commitOID)
	if err != nil {
		return nil, err
	}
	return &gitalypb.UserCreateTagResponse{
		Tag: &gitalypb.Tag{
			Id:            tagOID.String(),
			Name:          []byte(tagName),
			TargetCommit:  commit,
			Message:       message,
			MessageSize:   int64(len(message)),
			Tagger:        tagger,
			SignatureType: gitalypb.SignatureType_NONE,
		},
	}, nil
}

// UserDeleteTag removes refs/tags/<tag> with an optional CAS guard.
func (os *operationService) UserDeleteTag(ctx context.Context, req *gitalypb.UserDeleteTagRequest) (*gitalypb.UserDeleteTagResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	tagName := string(req.GetTagName())
	if !validBranchName(tagName) {
		return nil, status.Error(codes.InvalidArgument, "tag name invalid")
	}
	tl := fullRefTimeline(name, "refs/tags/"+tagName)
	head, ok, _ := os.timelineHead(ctx, name, tl)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "tag not found: %s", tagName)
	}
	if old := req.GetExpectedOldOid(); old != "" {
		want, err := os.resolveExpectedOld(ctx, name, old)
		if err != nil {
			return nil, err
		}
		if want != head {
			return nil, status.Error(codes.FailedPrecondition, "reference update failed")
		}
	}
	if err := os.casRef(ctx, name, tl, head, model.OID("")); err != nil {
		return nil, err
	}
	return &gitalypb.UserDeleteTagResponse{}, nil
}

// UserFFBranch fast-forwards a branch to commit_id.
func (os *operationService) UserFFBranch(ctx context.Context, req *gitalypb.UserFFBranchRequest) (*gitalypb.UserFFBranchResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	branch := string(req.GetBranch())
	if !validBranchName(branch) {
		return nil, status.Error(codes.InvalidArgument, "branch name invalid")
	}
	commitID := req.GetCommitId()
	if !isHexOID(commitID) {
		return nil, status.Error(codes.InvalidArgument, "cannot parse commit ID")
	}
	advertised, raw, ok := os.branchState(ctx, name, branch)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "reference not found: refs/heads/%s", branch)
	}
	if old := req.GetExpectedOldOid(); old != "" {
		want, err := os.resolveExpectedOld(ctx, name, old)
		if err != nil {
			return nil, err
		}
		if want != raw && want != advertised {
			return nil, status.Error(codes.FailedPrecondition, "reference update failed")
		}
	}
	if _, err := os.s.commitRecord(ctx, name, model.OID(commitID)); err != nil {
		return nil, status.Errorf(codes.NotFound, "gitaly: commit %s not found", commitID)
	}
	// Fast-forward against either identity: the raw head (the chain the CAS
	// moves) or the advertised tip (what the client last read — a previous
	// merge's linearized twin is a valid ancestor view too).
	ancFromRaw, err := os.s.isAncestor(ctx, name, raw, model.OID(commitID))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking for ancestry: %v", err)
	}
	anc := ancFromRaw
	if !anc && advertised != raw {
		anc, err = os.s.isAncestor(ctx, name, advertised, model.OID(commitID))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "checking for ancestry: %v", err)
		}
	}
	if !anc {
		return nil, status.Error(codes.FailedPrecondition, "not fast forward")
	}
	if err := os.casRef(ctx, name, branchTimeline(name, branch), raw, model.OID(commitID)); err != nil {
		return nil, err
	}
	return &gitalypb.UserFFBranchResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{CommitId: commitID},
	}, nil
}
