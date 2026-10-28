package gitaly

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// UserUpdateSubmodule moves one submodule pointer in a branch tree and
// commits the result — the web editor's "update submodule" action. The
// tree rewrite rides service.CreateCommit (the same publish path as a
// git push); author = committer = the acting user.
func (os *operationService) UserUpdateSubmodule(ctx context.Context, req *gitalypb.UserUpdateSubmoduleRequest) (*gitalypb.UserUpdateSubmoduleResponse, error) {
	name, err := os.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	branch := string(req.GetBranch())
	if !validBranchName(branch) {
		return nil, status.Error(codes.InvalidArgument, "empty Branch")
	}
	submodule := string(req.GetSubmodule())
	if submodule == "" {
		return nil, status.Error(codes.InvalidArgument, "empty submodule path")
	}
	if req.GetCommitSha() == "" || !isHexOID(req.GetCommitSha()) {
		return nil, status.Error(codes.InvalidArgument, "invalid commit sha")
	}
	ident, err := requestIdentity(req.GetUser(), req.GetTimestamp())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	hasBranches, err := os.repoHasBranches(ctx, name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "has branches: %v", err)
	}
	if !hasBranches {
		return nil, status.Error(codes.FailedPrecondition, "Repository is empty")
	}
	advertised, raw, hasBranch := os.branchState(ctx, name, branch)
	if !hasBranch {
		return nil, status.Error(codes.InvalidArgument, "Cannot find branch")
	}
	parent := advertised
	if parent.IsZero() {
		parent = raw
	}
	if expected := req.GetExpectedOldOid(); expected != "" {
		if !isHexOID(expected) || (model.OID(expected) != parent && model.OID(expected) != raw) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid expected old object ID: %q", expected)
		}
	}

	// The entry must be a gitlink (160000) today.
	rec, err := os.s.commitRecord(ctx, name, parent)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read branch tip: %v", err)
	}
	entry, err := os.entryAtTree(ctx, name, rec.Tree, submodule)
	if err != nil || entry == nil || entry.IsDir || entry.Mode != "160000" {
		return nil, withDetail(codes.InvalidArgument, "Invalid submodule path",
			&gitalypb.UserUpdateSubmoduleError{
				Error: &gitalypb.UserUpdateSubmoduleError_PathError{
					PathError: &gitalypb.PathError{
						Path:      []byte(submodule),
						ErrorType: gitalypb.PathError_ERROR_TYPE_INVALID_PATH,
					},
				},
			})
	}
	if entry.Target.String() == req.GetCommitSha() {
		return nil, status.Errorf(codes.InvalidArgument,
			"Submodule: %s is already at %s", submodule, req.GetCommitSha())
	}

	res, err := os.s.Svc.CreateCommit(ctx, service.CommitRequest{
		Repo:    name,
		Branch:  branch,
		Base:    parent,
		Message: string(req.GetCommitMessage()),
		Author:  &service.Identity{Name: ident.Name, Email: ident.Email},
		Committer: &service.Identity{
			Name:  ident.Name,
			Email: ident.Email,
		},
		When: ident.When.Unix(),
		Changes: []service.Change{{
			Path: submodule,
			Blob: model.OID(req.GetCommitSha()),
			Mode: "160000",
		}},
	})
	if err != nil {
		return nil, translateCommitError(err, branch)
	}
	return &gitalypb.UserUpdateSubmoduleResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      res.Head.String(),
			BranchCreated: false,
			RepoCreated:   false,
		},
	}, nil
}
