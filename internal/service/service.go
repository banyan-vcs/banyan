// Package service is banyan's protocol-agnostic core (docs/10): the single
// place where domain operations live. Frontends — httpd's JSON RPC (rpc),
// Gitaly gRPC (planned) and gitwire's smart HTTP — stay thin (parse, auth,
// serialize) and depend only on this layer; protocol types never appear here.
//
// Write-path invariant (docs/10 §2): "publish" (append a commit to a
// directory timeline via head CAS) has exactly one implementation —
// CreateCommit — shared by the CLI commit RPC and (after the Gitaly
// frontend lands) GitLab's receive path.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/view"
)

// Service wires the storage layer, the view engine and the object service
// into the domain operations exposed by every frontend.
type Service struct {
	Meta    storage.MetaStore
	Objects *storage.ObjectService
	Views   *view.Service
	Algo    model.HashAlgo
}

func New(meta storage.MetaStore, objects *storage.ObjectService, algo model.HashAlgo) *Service {
	return &Service{Meta: meta, Objects: objects, Views: view.New(meta, objects, algo), Algo: algo}
}

// ErrBaseMoved is the changeset conflict (docs/05 §4.2): the client's base
// commit is no longer the timeline head — pull --rebase and retry.
type ErrBaseMoved struct {
	Current model.OID
}

func (e *ErrBaseMoved) Error() string {
	return fmt.Sprintf("base moved: current head is %s", e.Current)
}

// ErrInvalidArg marks client-side contract violations (bad path, hash
// mismatch, missing object) — mapped to HTTP 400 by the rpc frontend.
type ErrInvalidArg struct{ Reason string }

func (e *ErrInvalidArg) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return &ErrInvalidArg{Reason: fmt.Sprintf(format, args...)}
}

// Translate maps a service error to the stable error-envelope code
// (docs/10 §3.2). Frontends translate codes to transport status / exit codes.
func Translate(err error) (code string, data any) {
	var base *ErrBaseMoved
	switch {
	case err == nil:
		return "", nil
	case errors.As(err, &base):
		return "CONFLICT_BASE_MOVED", map[string]string{"head": string(base.Current)}
	case errors.Is(err, storage.ErrNonFastForward):
		return "CONFLICT_REF_CAS", nil
	case errors.Is(err, storage.ErrRepoNotFound),
		errors.Is(err, model.ErrObjectNotFound),
		errors.Is(err, storage.ErrPackNotFound),
		errors.Is(err, storage.ErrTimelineUnknown):
		return "NOT_FOUND", nil
	default:
		var bad *ErrInvalidArg
		if errors.As(err, &bad) {
			return "INVALID_ARG", nil
		}
		return "INTERNAL", nil
	}
}

// --- common helpers ---

// parseSub canonicalizes the optional sub argument ("" = root repo).
func parseSub(sub string) (model.AnchorPath, error) {
	if sub == "" {
		return model.RootAnchor, nil
	}
	p, err := model.ParseAnchorPath(sub)
	if err != nil {
		return "", invalid("sub: %v", err)
	}
	return p, nil
}

// defaultBranchOf resolves the repo's default branch ("main" fallback,
// mirroring gitwire's advertisement behavior).
func (s *Service) defaultBranchOf(ctx context.Context, repo string) (string, error) {
	info, err := s.Meta.GetRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	if info.DefaultBranch == "" {
		return "main", nil
	}
	return info.DefaultBranch, nil
}

// viewHead resolves the (virtual) repo's current view head and its tree
// (docs/03 §6): root repo → woven tip; subtree repo → subtree view head.
// rootLocal carries the root-local head for subtree timelines (EnsureTimeline
// base). A zero head means "no commits yet" (root repo only).
func (s *Service) viewHead(ctx context.Context, repo string, sub model.AnchorPath, branch string) (head, tree, rootLocal model.OID, err error) {
	if sub.IsRoot() {
		head, err = s.Views.RootTip(ctx, repo, branch)
		if err != nil {
			return "", "", "", err
		}
		if head.IsZero() {
			return "", "", "", nil // empty repo: first-commit semantics
		}
	} else {
		head, rootLocal, err = s.Views.SubtreeHead(ctx, repo, sub, branch)
		if err != nil {
			return "", "", "", err
		}
		if head.IsZero() {
			return "", "", "", fmt.Errorf("subtree view %s%s unavailable", repo, sub)
		}
	}
	ci, err := s.Objects.CommitInfo(repo, head)
	if err != nil {
		return "", "", "", err
	}
	return head, ci.Tree, rootLocal, nil
}

// --- repo registry (CLI repo ls; create stays M5 with auth) ---

type RepoSummary struct {
	ID            string `json:"id"`
	DefaultBranch string `json:"defaultBranch"`
	Algorithm     string `json:"algorithm"`
}

func (s *Service) ListRepos(ctx context.Context) ([]RepoSummary, error) {
	repos, err := s.Meta.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RepoSummary, 0, len(repos))
	for _, r := range repos {
		algo := "sha1"
		if r.Algo == model.SHA256 {
			algo = "sha256"
		}
		out = append(out, RepoSummary{ID: r.ID, DefaultBranch: r.DefaultBranch, Algorithm: algo})
	}
	return out, nil
}

// --- object transport (docs/05 §3.1: content-addressed direct fetch) ---

// GetObject returns an object's type and raw bytes (cat / lazy backfill).
func (s *Service) GetObject(ctx context.Context, repo string, oid model.OID) (model.ObjectType, []byte, error) {
	return s.Objects.Raw(ctx, repo, oid)
}

// PutObject is one uploaded object (CLI blob upload before a commit; the
// hash is verified against the claimed type — content addressing is the
// integrity contract, docs/10 §3.3).
type PutObject struct {
	OID  model.OID `json:"oid"`
	Type string    `json:"type"`
	Data []byte    `json:"data"`
}

// PutObjects verifies and persists a batch of objects as one pack.
func (s *Service) PutObjects(ctx context.Context, repo string, objs []PutObject) error {
	if _, err := s.Meta.GetRepo(ctx, repo); err != nil {
		return err
	}
	if len(objs) == 0 {
		return nil
	}
	blobs := map[model.OID][]byte{}
	trees := map[model.OID][]byte{}
	commits := map[model.OID][]byte{}
	tags := map[model.OID][]byte{}
	for _, o := range objs {
		t := model.ObjectType(o.Type)
		switch t {
		case model.BlobType, model.TreeType, model.CommitType, model.TagType:
		default:
			return invalid("object %s: unknown type %q", o.OID, o.Type)
		}
		if got := model.ObjectOID(t, o.Data, s.Algo); got != o.OID {
			return invalid("object %s: hash mismatch (computed %s as %s)", o.OID, got, t)
		}
		switch t {
		case model.BlobType:
			blobs[o.OID] = o.Data
		case model.TreeType:
			trees[o.OID] = o.Data
		case model.CommitType:
			commits[o.OID] = o.Data
		case model.TagType:
			tags[o.OID] = o.Data
		}
	}
	var buf bytes.Buffer
	if err := storage.EncodePack(&buf, blobs, trees, commits, tags); err != nil {
		return err
	}
	_, err := s.Objects.IngestPack(ctx, repo, buf.Bytes())
	return err
}

// --- resolve (CLI cat: rev:path → object pointer) ---

type ResolveRequest struct {
	Repo string `json:"repo"`
	Sub  string `json:"sub,omitempty"`
	Rev  string `json:"rev,omitempty"` // branch; "" = default
	Path string `json:"path"`
}

type ResolveResult struct {
	Type string    `json:"type"` // blob | tree
	OID  model.OID `json:"oid"`
	Size int       `json:"size"`
}

func (s *Service) Resolve(ctx context.Context, req ResolveRequest) (*ResolveResult, error) {
	sub, err := parseSub(req.Sub)
	if err != nil {
		return nil, err
	}
	branch := req.Rev
	if branch == "" {
		if branch, err = s.defaultBranchOf(ctx, req.Repo); err != nil {
			return nil, err
		}
	}
	_, tree, _, err := s.viewHead(ctx, req.Repo, sub, branch)
	if err != nil {
		return nil, err
	}
	if tree.IsZero() {
		return nil, fmt.Errorf("%w: repo %s is empty", model.ErrObjectNotFound, req.Repo)
	}
	segs, err := splitRelPath(req.Path)
	if err != nil {
		return nil, err
	}
	lookup := s.Objects.Lookup(req.Repo)
	cur := tree
	for i, seg := range segs {
		t, err := lookup.Tree(cur)
		if err != nil {
			return nil, err
		}
		var entry *model.TreeEntry
		for j := range t.Entries {
			if t.Entries[j].Name == seg {
				entry = &t.Entries[j]
				break
			}
		}
		if entry == nil {
			return nil, fmt.Errorf("%w: %s", model.ErrObjectNotFound, req.Path)
		}
		if i == len(segs)-1 {
			typ := "blob"
			if entry.IsDir {
				typ = "tree"
			}
			size := 0
			if !entry.IsDir {
				if t, data, rerr := s.Objects.Raw(ctx, req.Repo, entry.Target); rerr == nil && t == model.BlobType {
					size = len(data)
				}
			}
			return &ResolveResult{Type: typ, OID: entry.Target, Size: size}, nil
		}
		if !entry.IsDir {
			return nil, fmt.Errorf("%w: %s is not a directory", model.ErrObjectNotFound, seg)
		}
		cur = entry.Target
	}
	return &ResolveResult{Type: "tree", OID: cur}, nil
}

// --- timelines / repack (diagnostics and admin passthrough) ---

type TimelineSummary struct {
	Anchor string    `json:"anchor"`
	Name   string    `json:"name"`
	Head   model.OID `json:"head"`
	Ref    string    `json:"ref,omitempty"`
}

func (s *Service) Timelines(ctx context.Context, repo string) ([]TimelineSummary, error) {
	tls, err := s.Meta.ListAllTimelines(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := make([]TimelineSummary, 0, len(tls))
	for _, tl := range tls {
		sum := TimelineSummary{Anchor: string(tl.Anchor), Name: tl.Name}
		if st, err := s.Meta.Timeline(ctx, tl); err == nil {
			sum.Head = st.Head
		}
		if tl.Anchor.IsRoot() && !isAuxRefName(tl.Name) {
			sum.Ref = tl.RefName()
		} else if isAuxRefName(tl.Name) {
			sum.Ref = tl.Name
		}
		out = append(out, sum)
	}
	return out, nil
}

func (s *Service) Repack(ctx context.Context, repo string, threshold int) (storage.RepackResult, error) {
	return s.Objects.Repack(ctx, repo, threshold)
}
