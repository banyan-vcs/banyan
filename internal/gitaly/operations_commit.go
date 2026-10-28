package gitaly

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// UserCommitFiles is the web-editor write path: a bidi stream of one header
// followed by actions (header + content chunks each), assembled server-side
// into a commit via service.CreateCommit — the same publish path as a git
// push. Action semantics mirror upstream v19.5.2 commit_files.go, including
// CREATE_DIR materializing a .gitkeep (git tracks no empty directories).
func (os *operationService) UserCommitFiles(stream gitalypb.OperationService_UserCommitFilesServer) error {
	ctx := stream.Context()

	var header *gitalypb.UserCommitFilesRequestHeader
	type opAction struct {
		hdr     *gitalypb.UserCommitFilesActionHeader
		content []byte
	}
	var actions []opAction
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header == nil {
			if h := req.GetHeader(); h != nil {
				header = h
				continue
			}
			return status.Error(codes.InvalidArgument, "no header provided")
		}
		act := req.GetAction()
		if act == nil {
			return status.Error(codes.InvalidArgument, "unrecognized request payload")
		}
		switch payload := act.GetUserCommitFilesActionPayload().(type) {
		case *gitalypb.UserCommitFilesAction_Header:
			actions = append(actions, opAction{hdr: payload.Header})
		case *gitalypb.UserCommitFilesAction_Content:
			if len(actions) == 0 {
				return status.Error(codes.InvalidArgument, "content sent before action")
			}
			actions[len(actions)-1].content = append(actions[len(actions)-1].content, payload.Content...)
		default:
			return status.Errorf(codes.InvalidArgument, "unhandled action payload type: %T", payload)
		}
	}
	if header == nil {
		return status.Error(codes.InvalidArgument, "no header provided")
	}

	name, err := os.s.existingRepo(ctx, header.GetRepository())
	if err != nil {
		return err
	}
	branch := string(header.GetBranchName())
	if !validBranchName(branch) {
		return status.Error(codes.InvalidArgument, "branch name invalid")
	}
	if startRepo := header.GetStartRepository(); startRepo != nil {
		return status.Error(codes.InvalidArgument,
			"start_repository: cross-repository commit files is not supported on this node")
	}
	hasBranches, err := os.repoHasBranches(ctx, name)
	if err != nil {
		return status.Errorf(codes.Internal, "checking branches: %v", err)
	}

	// Parent: the target branch's advertised tip (the view CreateCommit and
	// every read share), or the same-repo start branch. When the target
	// branch is unborn but a start branch is given, the branch is created at
	// the start tip first — mirroring git's implicit branch-on-commit.
	advertised, raw, hasBranch := os.branchState(ctx, name, branch)
	branchWasUnborn := !hasBranch
	parent := advertised
	if parent.IsZero() {
		parent = raw
	}
	if start := string(header.GetStartBranchName()); start != "" {
		rev, err := os.s.resolveRevision(ctx, name, "refs/heads/"+start)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "could not resolve start branch %q", start)
		}
		parent, err = os.peelToCommit(ctx, name, rev)
		if err != nil {
			return status.Error(codes.InvalidArgument, "start branch does not point at a commit")
		}
		if !hasBranch && !parent.IsZero() {
			if err := os.casRef(ctx, name, branchTimeline(name, branch), model.OID(""), parent); err != nil {
				return err
			}
		}
	}
	if old := header.GetExpectedOldOid(); old != "" {
		if !isHexOID(old) {
			return status.Errorf(codes.InvalidArgument, "invalid expected old object ID: %q", old)
		}
		if model.OID(old) != parent {
			return status.Error(codes.FailedPrecondition, "cannot update branch: expected old object ID mismatch")
		}
	}

	// Actions → change list + blob batch.
	var uploads []service.PutObject
	var changes []service.Change
	parentTree := model.OID("")
	if rec, err := os.s.commitRecord(ctx, name, parent); err == nil {
		parentTree = rec.Tree
	}
	blobOf := func(content []byte) (model.OID, error) {
		oid := model.ObjectOID(model.BlobType, content, os.s.Algo)
		uploads = append(uploads, service.PutObject{OID: oid, Type: string(model.BlobType), Data: content})
		return oid, nil
	}
	decodeContent := func(a opAction) ([]byte, error) {
		if a.hdr.GetBase64Content() {
			return base64.StdEncoding.DecodeString(string(a.content))
		}
		return a.content, nil
	}
	for _, a := range actions {
		path := strings.TrimPrefix(string(a.hdr.GetFilePath()), "/")
		if path == "" || strings.Contains(path, "..") {
			return status.Errorf(codes.InvalidArgument, "invalid path %q", path)
		}
		switch a.hdr.GetAction() {
		case gitalypb.UserCommitFilesActionHeader_CREATE, gitalypb.UserCommitFilesActionHeader_UPDATE:
			content, err := decodeContent(a)
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "path %q: content: %v", path, err)
			}
			oid, err := blobOf(content)
			if err != nil {
				return err
			}
			mode := ""
			if a.hdr.GetAction() == gitalypb.UserCommitFilesActionHeader_CREATE && a.hdr.GetExecuteFilemode() {
				mode = "100755"
			}
			changes = append(changes, service.Change{Path: path, Blob: oid, Mode: mode})
		case gitalypb.UserCommitFilesActionHeader_DELETE:
			changes = append(changes, service.Change{Path: path, Delete: true})
		case gitalypb.UserCommitFilesActionHeader_MOVE:
			prev := strings.TrimPrefix(string(a.hdr.GetPreviousPath()), "/")
			if prev == "" {
				return status.Errorf(codes.InvalidArgument, "move: previous path empty")
			}
			var blobOID model.OID
			var mode string
			if a.hdr.GetInferContent() {
				entry, err := os.entryAtTree(ctx, name, parentTree, prev)
				if err != nil || entry == nil || entry.IsDir {
					return status.Errorf(codes.InvalidArgument, "move: path %q not found", prev)
				}
				blobOID, mode = entry.Target, entry.Mode
			} else {
				content, err := decodeContent(a)
				if err != nil {
					return status.Errorf(codes.InvalidArgument, "path %q: content: %v", path, err)
				}
				blobOID, err = blobOf(content)
				if err != nil {
					return err
				}
			}
			changes = append(changes,
				service.Change{Path: prev, Delete: true},
				service.Change{Path: path, Blob: blobOID, Mode: mode})
		case gitalypb.UserCommitFilesActionHeader_CHMOD:
			entry, err := os.entryAtTree(ctx, name, parentTree, path)
			if err != nil || entry == nil || entry.IsDir {
				return status.Errorf(codes.InvalidArgument, "chmod: path %q not found", path)
			}
			mode := "100644"
			if a.hdr.GetExecuteFilemode() {
				mode = "100755"
			}
			changes = append(changes, service.Change{Path: path, Blob: entry.Target, Mode: mode})
		case gitalypb.UserCommitFilesActionHeader_CREATE_DIR:
			content := []byte{}
			oid, err := blobOf(content)
			if err != nil {
				return err
			}
			changes = append(changes, service.Change{Path: path + "/.gitkeep", Blob: oid})
		default:
			return status.Errorf(codes.InvalidArgument, "unknown action %d", a.hdr.GetAction())
		}
	}
	if parent.IsZero() && len(changes) == 0 {
		return status.Error(codes.InvalidArgument, "no actions on an unborn branch")
	}

	if len(uploads) > 0 {
		if err := os.s.Svc.PutObjects(ctx, name, uploads); err != nil {
			return status.Errorf(codes.Internal, "staging blobs: %v", err)
		}
	}

	// Identities: author = header (fallback committer), committer = acting
	// user — both share the header timestamp.
	user := header.GetUser()
	committerName, committerEmail := "banyan", "banyan@local"
	if user != nil && len(user.GetName()) > 0 {
		committerName = string(user.GetName())
		if len(user.GetEmail()) > 0 {
			committerEmail = string(user.GetEmail())
		}
	}
	authorName := strings.TrimSpace(string(header.GetCommitAuthorName()))
	authorEmail := strings.TrimSpace(string(header.GetCommitAuthorEmail()))
	if authorName == "" {
		authorName = committerName
	}
	if authorEmail == "" {
		authorEmail = committerEmail
	}
	var when int64
	if ts := header.GetTimestamp(); ts != nil {
		when = ts.AsTime().Unix()
	}

	res, err := os.s.Svc.CreateCommit(ctx, service.CommitRequest{
		Repo:    name,
		Branch:  branch,
		Base:    parent,
		Message: string(header.GetCommitMessage()),
		Author:  &service.Identity{Name: authorName, Email: authorEmail},
		Committer: &service.Identity{
			Name:  committerName,
			Email: committerEmail,
		},
		When:    when,
		Changes: changes,
	})
	if err != nil {
		return translateCommitError(err, branch)
	}
	return stream.SendAndClose(&gitalypb.UserCommitFilesResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      res.Head.String(),
			RepoCreated:   !hasBranches,
			BranchCreated: branchWasUnborn,
		},
	})
}

// repoHasBranches answers whether any branch timeline holds a head.
func (os *operationService) repoHasBranches(ctx context.Context, repo string) (bool, error) {
	tls, err := os.s.Meta.ListTimelines(ctx, repo, model.RootAnchor)
	if err != nil {
		return false, err
	}
	for _, tl := range tls {
		if strings.HasPrefix(tl.Name, "refs/") {
			continue
		}
		if st, terr := os.s.Meta.Timeline(ctx, tl); terr == nil && !st.Head.IsZero() {
			return true, nil
		}
	}
	return false, nil
}

// entryAtTree resolves a tree entry below a tree OID (nil = missing).
func (os *operationService) entryAtTree(ctx context.Context, repo string, tree model.OID, path string) (*model.TreeEntry, error) {
	if tree.IsZero() {
		return nil, nil
	}
	return os.s.entryAtPath(ctx, repo, tree, path)
}

// translateCommitError maps service-layer commit failures onto wire codes.
func translateCommitError(err error, branch string) error {
	var moved *service.ErrBaseMoved
	if errors.As(err, &moved) {
		return status.Errorf(codes.FailedPrecondition,
			"Could not update %s. Please refresh and try again.", branch)
	}
	var bad *service.ErrInvalidArg
	if errors.As(err, &bad) {
		return status.Error(codes.InvalidArgument, bad.Reason)
	}
	return status.Errorf(codes.Internal, "commit files: %v", err)
}
