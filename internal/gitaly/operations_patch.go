package gitaly

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// UserApplyPatch applies an mbox patch series to a branch (the web
// editor's "apply patch" flow): each mail becomes one commit — author from
// the mail headers, message subject+body, tree from applying the mail's
// unified diff onto the running tree. The applier is pure Go
// (patchapply.go): context matching with forward/backward offset search;
// binary and rename/copy patches are rejected rather than misapplied.
func (os *operationService) UserApplyPatch(stream gitalypb.OperationService_UserApplyPatchServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "empty UserApplyPatch_Header")
	}
	name, err := os.s.existingRepo(ctx, header.GetRepository())
	if err != nil {
		return err
	}
	branch := string(header.GetTargetBranch())
	if !validBranchName(branch) {
		return status.Error(codes.InvalidArgument, "empty target branch")
	}
	committer, err := requestIdentity(header.GetUser(), header.GetTimestamp())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	// The mbox rides the stream after the header.
	var mbox []byte
	mbox = append(mbox, first.GetPatches()...)
	for {
		req, err := stream.Recv()
		if err != nil {
			break
		}
		mbox = append(mbox, req.GetPatches()...)
	}
	patches, err := splitMailbox(mbox)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	// Parent: the target branch tip (or the default branch when the target
	// is unborn — GitLab's create-branch-via-patch flow).
	branchCreated := false
	parent := model.OID("")
	if adv, raw, hasBranch := os.branchState(ctx, name, branch); hasBranch {
		if !adv.IsZero() {
			parent = adv
		} else {
			parent = raw
		}
	} else if def, derr := os.s.defaultBranch(ctx, name); derr == nil {
		if rev, rerr := os.s.resolveRevision(ctx, name, "refs/heads/"+def); rerr == nil {
			parent, err = os.peelToCommit(ctx, name, rev)
			if err != nil {
				return status.Error(codes.InvalidArgument, "default branch is not a commit")
			}
			branchCreated = true
		}
	}
	if expected := header.GetExpectedOldOid(); expected != "" {
		if !isHexOID(expected) || model.OID(expected) != parent {
			return status.Error(codes.FailedPrecondition, "cannot update branch: expected old object ID mismatch")
		}
	}

	os.s.Objects.BeginSynthBatch(name)
	defer os.s.Objects.EndSynthBatch(name)

	current := parent
	for i, patch := range patches {
		changes, blobs, perr := os.applyPatchToCommit(ctx, name, current, patch)
		if perr != nil {
			if errIsConflict(perr) {
				return status.Errorf(codes.FailedPrecondition, "Patch failed at %04d %s: %v", i+1, patch.subject, perr)
			}
			return status.Errorf(codes.InvalidArgument, "applying patch %d: %v", i+1, perr)
		}
		if len(blobs) > 0 {
			if uerr := os.s.Svc.PutObjects(ctx, name, blobs); uerr != nil {
				return status.Errorf(codes.Internal, "staging patch blobs: %v", uerr)
			}
		}
		if len(changes) == 0 {
			continue // empty patch: no commit
		}
		author := model.Identity{
			Name: patch.authorName, Email: patch.authorEmail,
			When: committer.When,
		}
		if !patch.authorDate.IsZero() {
			author.When = patch.authorDate
		}
		if author.Name == "" {
			author.Name = committer.Name
		}
		if author.Email == "" {
			author.Email = committer.Email
		}
		msg := patch.subject + "\n"
		if body := strings.Trim(patch.body, "\n"); body != "" {
			msg += "\n" + body + "\n"
		}
		var base model.OID
		if !current.IsZero() {
			base = current
		}
		res, cerr := os.s.Svc.CreateCommit(ctx, service.CommitRequest{
			Repo: name, Branch: branch, Base: base,
			Message: msg, Author: &service.Identity{Name: author.Name, Email: author.Email},
			Committer: &service.Identity{Name: committer.Name, Email: committer.Email},
			When:      author.When.Unix(),
			Changes:   changes,
		})
		if cerr != nil {
			return translateCommitError(cerr, branch)
		}
		current = res.Head
	}
	if current == parent {
		return status.Error(codes.InvalidArgument, "patch series produced no changes")
	}
	return stream.SendAndClose(&gitalypb.UserApplyPatchResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      current.String(),
			BranchCreated: branchCreated,
		},
	})
}

// applyPatchToCommit applies one parsed patch's file changes onto a commit
// tree, answering the service-layer change list plus the blobs to stage.
// Context mismatches are conflicts; structurally unsupported patches are
// invalid.
func (os *operationService) applyPatchToCommit(ctx context.Context, repo string, commit model.OID, patch parsedPatch) ([]service.Change, []service.PutObject, error) {
	files, err := parseUnifiedDiff(patch.diff)
	if err != nil {
		return nil, nil, err
	}
	var tree model.OID
	if !commit.IsZero() {
		rec, err := os.s.commitRecord(ctx, repo, commit)
		if err != nil {
			return nil, nil, err
		}
		tree = rec.Tree
	}
	var changes []service.Change
	var uploads []service.PutObject
	for _, f := range files {
		path := f.oldPath
		if path == "" || path == "/dev/null" {
			path = f.newPath
		}
		if path == "" || strings.HasPrefix(path, "/dev/null") {
			return nil, nil, fmt.Errorf("patch without a usable path")
		}
		entry, err := os.entryAtTree(ctx, repo, tree, path)
		if err != nil {
			return nil, nil, err
		}
		var oldLines []string
		if entry != nil && !entry.IsDir {
			_, raw, rerr := os.s.Objects.Raw(ctx, repo, entry.Target)
			if rerr != nil {
				return nil, nil, rerr
			}
			oldLines = splitPatchLines(raw)
		} else if !f.newFile {
			return nil, nil, errPatchConflict{path: path, reason: "target file missing"}
		}

		// Hunks reference original positions; track the running offset.
		offset := 0
		for _, h := range f.hunks {
			at := h.oldStart - 1 + offset
			effective := h
			effective.oldStart = at + 1
			if effective.oldStart < 1 {
				effective.oldStart = 1
			}
			newLines, ok := applyToLines(oldLines, effective)
			if !ok {
				return nil, nil, errPatchConflict{path: path, reason: "context mismatch"}
			}
			offset += len(newLines) - len(oldLines)
			oldLines = newLines
		}
		if f.deleteFile && len(oldLines) != 0 {
			return nil, nil, errPatchConflict{path: path, reason: "delete patch leaves lines"}
		}
		content := joinPatchLines(oldLines)
		if f.deleteFile {
			changes = append(changes, service.Change{Path: path, Delete: true})
			continue
		}
		mode := ""
		if f.newFile && f.newMode == "100755" {
			mode = "100755"
		}
		blobOID := model.ObjectOID(model.BlobType, content, os.s.Algo)
		uploads = append(uploads, service.PutObject{OID: blobOID, Type: string(model.BlobType), Data: content})
		changes = append(changes, service.Change{Path: path, Blob: blobOID, Mode: mode})
	}
	return changes, uploads, nil
}

// errPatchConflict marks context-mismatch failures that answer
// FailedPrecondition ("Patch failed at …"), git am's conflict shape.
type errPatchConflict struct {
	path, reason string
}

func (e errPatchConflict) Error() string { return e.path + ": " + e.reason }

func errIsConflict(err error) bool {
	_, ok := err.(errPatchConflict)
	return ok
}

// splitPatchLines splits content into lines keeping no trailing empty.
func splitPatchLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
