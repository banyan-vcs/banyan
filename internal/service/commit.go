package service

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// CreateCommit: server-side changeset assembly (docs/06 §4.2, docs/05 §2).
// The client uploads only NEW blobs (PutObjects) plus a change list relative
// to its base; the server rebuilds trees along the touched paths, creates a
// standard git commit and publishes it with the single directory-timeline
// CAS — the same write path as a git push (v5: zero cross-directory locks).
//
// Scion's changeset semantics ("list of files changed relative to the first
// parent") on git's object model: git clients and mirror topologies see a
// perfectly ordinary commit.
type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Change struct {
	Path   string    `json:"path"`
	Blob   model.OID `json:"blob,omitempty"`
	Delete bool      `json:"delete,omitempty"`
	Mode   string    `json:"mode,omitempty"` // empty: preserved from the base entry, else 100644
}

type CommitRequest struct {
	Repo    string    `json:"repo"`
	Sub     string    `json:"sub,omitempty"`
	Branch  string    `json:"branch,omitempty"` // "" = repo default
	Base    model.OID `json:"base"`             // the client's manifest commit; must equal the current head
	Message string    `json:"message"`
	Author  *Identity `json:"author,omitempty"`
	// Committer overrides the commit's committer identity (gitaly web-edit
	// flow: author = the editor, committer = the acting user). Defaults to Author.
	Committer *Identity `json:"committer,omitempty"`
	// When fixes the commit timestamp (unix seconds); 0 = now with the
	// monotonic same-second bump below.
	When    int64    `json:"when,omitempty"`
	Changes []Change `json:"changes"`
}

type CommitResult struct {
	Head     model.OID `json:"head"`     // new timeline head (the created commit)
	RootView model.OID `json:"rootView"` // woven root view (CI/webhook anchor, docs/01 §3.2)
	Tree     model.OID `json:"tree"`     // new view tree (client manifest anchor)
}

func (s *Service) CreateCommit(ctx context.Context, req CommitRequest) (*CommitResult, error) {
	sub, err := parseSub(req.Sub)
	if err != nil {
		return nil, err
	}
	branch := req.Branch
	if branch == "" {
		if branch, err = s.defaultBranchOf(ctx, req.Repo); err != nil {
			return nil, err
		}
	}
	head, baseTree, rootLocal, err := s.viewHead(ctx, req.Repo, sub, branch)
	if err != nil {
		return nil, err
	}
	if req.Base != head {
		return nil, &ErrBaseMoved{Current: head}
	}

	// Referenced blobs must already be uploaded (PutObjects) and be blobs.
	// Gitlinks (mode 160000) are exempt: they point at commits that live
	// in the submodule's repository, not this one.
	for _, ch := range req.Changes {
		if ch.Delete || ch.Mode == "160000" {
			continue
		}
		t, _, rerr := s.Objects.Raw(ctx, req.Repo, ch.Blob)
		if rerr != nil {
			return nil, invalid("change %s: blob %s not uploaded (%v)", ch.Path, ch.Blob, rerr)
		}
		if t != model.BlobType {
			return nil, invalid("change %s: %s is a %s, want blob", ch.Path, ch.Blob, t)
		}
	}

	newTree, newTrees, err := s.applyChanges(ctx, req.Repo, baseTree, req.Changes)
	if err != nil {
		return nil, err
	}

	id := Identity{Name: "banyan", Email: "banyan@local"}
	if req.Author != nil && req.Author.Name != "" {
		if req.Author.Email == "" {
			req.Author.Email = "banyan@local"
		}
		id = *req.Author
	}
	committer := id
	if req.Committer != nil && req.Committer.Name != "" {
		committer = *req.Committer
		if committer.Email == "" {
			committer.Email = "banyan@local"
		}
	}
	when := time.Now()
	if req.When > 0 {
		when = time.Unix(req.When, 0)
	} else if !head.IsZero() {
		if ci, cierr := s.Objects.CommitInfo(req.Repo, head); cierr == nil && when.Unix() <= ci.When {
			// Monotonic timestamps keep the weave's (time, …) ordering stable
			// even for same-second commit bursts (pitfall: timestamp ties).
			when = time.Unix(ci.When+1, 0)
		}
	}
	msg := req.Message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	commit := &model.AnchoredCommit{
		Anchor:    model.RootAnchor, // plain git commit: no anchor header, verbatim like a pushed chain
		TreeOID:   newTree,
		Author:    model.Identity{Name: id.Name, Email: id.Email, When: when},
		Committer: model.Identity{Name: committer.Name, Email: committer.Email, When: when},
		Message:   msg,
	}
	if !head.IsZero() {
		commit.Parents = []model.OID{head}
	}
	commitOID := commit.OID(s.Algo)
	commitRaw := commit.Encode()

	// Persist all new trees + the commit as one pack (one fragment per commit;
	// repack consolidates, docs/09 §3.2).
	var buf bytes.Buffer
	if err := storage.EncodePack(&buf, nil, newTrees, map[model.OID][]byte{commitOID: commitRaw}, nil); err != nil {
		return nil, err
	}
	if _, err := s.Objects.IngestPack(ctx, req.Repo, buf.Bytes()); err != nil {
		return nil, err
	}

	// Publish: the single directory-timeline CAS (v5 write path).
	if sub.IsRoot() {
		tl := model.Timeline{Repo: req.Repo, Anchor: model.RootAnchor, Name: branch}
		expected := req.Base
		if st, terr := s.Meta.Timeline(ctx, tl); terr == nil {
			expected = st.Head
		}
		if err := s.Meta.CASHead(ctx, tl, expected, commitOID); err != nil {
			return nil, err
		}
		return &CommitResult{Head: commitOID, RootView: commitOID, Tree: newTree}, nil
	}
	localTL := model.Timeline{Repo: req.Repo, Anchor: sub, Name: branch}
	expected := head
	if st, terr := s.Meta.Timeline(ctx, localTL); terr == nil {
		expected = st.Head
	} else if terr := s.Meta.EnsureTimeline(ctx, localTL, head, rootLocal); terr != nil {
		return nil, terr
	}
	if err := s.Meta.CASHead(ctx, localTL, expected, commitOID); err != nil {
		return nil, err
	}
	rootView, _ := s.Views.RootTip(ctx, req.Repo, branch) // best-effort: weave materializes on read
	return &CommitResult{Head: commitOID, RootView: rootView, Tree: newTree}, nil
}

// --- tree rebuild (git index semantics: touched paths allocate new trees) ---

type treeChange struct {
	segs []string
	ch   Change
}

// splitRelPath validates a workspace-relative path ("a/b/c") and returns its
// segments. Empty input yields no segments (the root itself).
func splitRelPath(p string) ([]string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, nil
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return nil, invalid("path %q: invalid segment %q", p, seg)
		}
	}
	return segs, nil
}

// applyChanges returns the new root tree and every new tree object to persist.
func (s *Service) applyChanges(ctx context.Context, repo string, base model.OID, changes []Change) (model.OID, map[model.OID][]byte, error) {
	out := map[model.OID][]byte{}
	ops := make([]treeChange, 0, len(changes))
	for _, ch := range changes {
		segs, err := splitRelPath(ch.Path)
		if err != nil {
			return "", nil, err
		}
		if len(segs) == 0 {
			return "", nil, invalid("change path is empty")
		}
		ops = append(ops, treeChange{segs: segs, ch: ch})
	}
	root, _, err := s.rebuildTree(ctx, repo, base, 0, ops, out)
	return root, out, err
}

// rebuildTree rewrites one tree level. removed=true tells the caller (a
// parent level) to drop this subtree entry — deleting the last file of a
// directory removes the directory, as git's index does.
func (s *Service) rebuildTree(ctx context.Context, repo string, base model.OID, depth int, ops []treeChange, out map[model.OID][]byte) (model.OID, bool, error) {
	if depth > 64 {
		return "", false, invalid("change path too deep")
	}
	lookup := s.Objects.Lookup(repo)
	var entries []model.TreeEntry
	if !base.IsZero() {
		t, err := lookup.Tree(base)
		if err != nil {
			return "", false, err
		}
		entries = append(entries, t.Entries...)
	}
	idx := map[string]int{}
	for i, e := range entries {
		idx[e.Name] = i
	}

	// Non-terminal ops grouped by their first segment (one recursion per child dir).
	children := map[string][]treeChange{}
	for _, op := range ops {
		if len(op.segs) > 1 {
			children[op.segs[0]] = append(children[op.segs[0]], treeChange{segs: op.segs[1:], ch: op.ch})
		}
	}
	// Terminal ops applied directly at this level.
	for _, op := range ops {
		if len(op.segs) != 1 {
			continue
		}
		name := op.segs[0]
		if op.ch.Delete {
			if i, ok := idx[name]; ok {
				entries = append(entries[:i], entries[i+1:]...)
				reindex(entries, idx)
			}
			continue
		}
		mode := op.ch.Mode
		if i, ok := idx[name]; ok && mode == "" {
			mode = entries[i].Mode // preserve the base entry's mode on rewrite
		}
		if mode == "" {
			mode = "100644"
		}
		e := model.TreeEntry{Name: name, Mode: mode, Target: op.ch.Blob}
		if i, ok := idx[name]; ok {
			entries[i] = e
		} else {
			entries = append(entries, e)
			idx[name] = len(entries) - 1
		}
	}
	// Recurse into child directories.
	for name, cops := range children {
		var childBase model.OID
		if i, ok := idx[name]; ok {
			if !entries[i].IsDir {
				// A file becomes a directory (the path grew deeper): start from empty.
				entries = append(entries[:i], entries[i+1:]...)
				reindex(entries, idx)
			} else {
				childBase = entries[i].Target
			}
		}
		childOID, removed, err := s.rebuildTree(ctx, repo, childBase, depth+1, cops, out)
		if err != nil {
			return "", false, err
		}
		if removed {
			if i, ok := idx[name]; ok {
				entries = append(entries[:i], entries[i+1:]...)
				reindex(entries, idx)
			}
			continue
		}
		e := model.TreeEntry{Name: name, Mode: "40000", Target: childOID, IsDir: true}
		if i, ok := idx[name]; ok {
			entries[i] = e
		} else {
			entries = append(entries, e)
			idx[name] = len(entries) - 1
		}
	}

	tree := &model.Tree{Entries: entries}
	raw, err := tree.Encode()
	if err != nil {
		return "", false, err
	}
	oid := tree.OID(s.Algo)
	out[oid] = raw
	return oid, len(entries) == 0, nil
}

func reindex(entries []model.TreeEntry, idx map[string]int) {
	clear(idx)
	for i, e := range entries {
		idx[e.Name] = i
	}
}
