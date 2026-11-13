package kit

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// repoName maps a gitaly Repository onto the banyan repo name via
// MapRelativePath — hashed relative paths collapse to their globally-unique
// storage oid, so every RPC agrees on one identity for one GitLab repo.
func (s *Base) RepoName(r *gitalypb.Repository) (string, error) {
	if r == nil || r.StorageName == "" {
		return "", status.Error(codes.InvalidArgument, "gitaly: repository.storage_name is required")
	}
	if r.RelativePath == "" {
		return "", status.Error(codes.InvalidArgument, "gitaly: repository.relative_path is required")
	}
	if r.StorageName != s.Opts.StorageName {
		return "", status.Errorf(codes.NotFound,
			"gitaly: storage %q not found (this node answers as %q)", r.StorageName, s.Opts.StorageName)
	}
	if strings.Contains(r.RelativePath, "..") || strings.ContainsAny(r.RelativePath, "\x00") {
		return "", status.Error(codes.InvalidArgument, "gitaly: repository.relative_path is malformed")
	}
	return MapRelativePath(r.RelativePath), nil
}

// MapRelativePath translates a gitaly relative_path into a banyan repo name.
// Banyan names cannot contain "/" (the /r/<repo>/tree/ URL grammar reserves
// it), while GitLab relative paths are slash-bearing: "@hashed/<h2>/<h2>/
// <sha256(project id)>.git". That trailing oid is globally unique and
// hex-only, so hashed repos collapse to it verbatim — zero ambiguity, and
// hashed storage addressing then derives the same opaque directory key
// upstream would. Pool paths get a pools- prefix (ObjectPool slice); legacy
// named paths fall back to reversible tilde escaping ("~0"=~, "~1"=/).
func MapRelativePath(rel string) string {
	rel = strings.TrimSuffix(rel, ".git")
	if p, ok := strings.CutPrefix(rel, "@hashed/"); ok {
		if segs := strings.Split(p, "/"); len(segs) == 3 && isHex64(segs[2]) {
			return segs[2]
		}
	}
	if p, ok := strings.CutPrefix(rel, "@pools/"); ok {
		if segs := strings.Split(p, "/"); len(segs) == 3 && isHex64(segs[2]) {
			return "pools-" + segs[2]
		}
	}
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(rel)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// existingRepo resolves and requires an existing repository; the canonical
// prelude of every per-repo RPC.
func (s *Base) ExistingRepo(ctx context.Context, r *gitalypb.Repository) (string, error) {
	name, err := s.RepoName(r)
	if err != nil {
		return "", err
	}
	if _, err := s.Meta.GetRepo(ctx, name); err != nil {
		return "", status.Errorf(codes.NotFound, "gitaly: repository %q not found", name)
	}
	return name, nil
}

// ListedRef is one advertised ref of a repo, in the git view clients see.
type ListedRef struct {
	Name string // HEAD | refs/heads/<b> | refs/tags/<t> | refs/<ns>/<name>
	Head model.OID
}

// listRefs returns the repo's advertised refs (the same view gitwire serves:
// woven branch tips plus verbatim auxiliary refs). Results are sorted by name
// with HEAD first removed — callers filter namespaces themselves.
func (s *Base) AdvertisedRefs(ctx context.Context, repo string) ([]ListedRef, error) {
	refs, err := s.Svc.ListRefs(ctx, repo, "")
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "gitaly: repository %q not found", repo)
	}
	out := make([]ListedRef, 0, len(refs))
	have := map[string]bool{}
	for _, r := range refs {
		out = append(out, ListedRef{Name: r.Name, Head: r.Head})
		have[r.Name] = true
	}
	// The subtree projections ride the same advertisement the git wire
	// serves (workhorse bridges onto it) — the two faces must agree on the
	// ref space (T60 pins ls-remote == ListRefs).
	def, derr := s.DefaultBranch(ctx, repo)
	if derr != nil {
		return nil, derr
	}
	for _, r := range s.Git.ProjectionRefs(ctx, repo, def) {
		if !have[r.Name] {
			out = append(out, ListedRef{Name: r.Name, Head: r.OID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// defaultBranch resolves the repo's default branch (RepoInfo, "main" fall-
// back — the same resolution gitwire's HEAD symref uses).
func (s *Base) DefaultBranch(ctx context.Context, repo string) (string, error) {
	info, err := s.Meta.GetRepo(ctx, repo)
	if err != nil {
		return "", status.Errorf(codes.NotFound, "gitaly: repository %q not found", repo)
	}
	if info.DefaultBranch == "" {
		return "main", nil
	}
	return info.DefaultBranch, nil
}

// resolveRevision maps a gitaly revision (full ref, short branch/tag, HEAD,
// or a hex OID) onto an OID in the repo's advertised view. A tilde/caret
// suffix walks parents (^, ~N).
func (s *Base) ResolveRevision(ctx context.Context, repo string, rev string) (model.OID, error) {
	refs, err := s.AdvertisedRefs(ctx, repo)
	if err != nil {
		return "", err
	}
	byName := make(map[string]model.OID, len(refs)+2)
	def, _ := s.DefaultBranch(ctx, repo)
	for _, r := range refs {
		byName[r.Name] = r.Head
	}
	resolve := func(name string) (model.OID, bool) {
		if oid, ok := byName[name]; ok {
			return oid, true
		}
		if !strings.HasPrefix(name, "refs/") {
			for _, p := range []string{"refs/heads/", "refs/tags/"} {
				if oid, ok := byName[p+name]; ok {
					return oid, true
				}
			}
		}
		return "", false
	}
	tail := ""
	if i := strings.IndexAny(rev, "^~"); i >= 0 {
		rev, tail = rev[:i], rev[i:]
	}
	var oid model.OID
	switch {
	case rev == "" || rev == "HEAD":
		oid = byName["HEAD"]
		if oid.IsZero() {
			if o, ok := resolve(def); ok {
				oid = o
			}
		}
	default:
		if o, ok := resolve(rev); ok {
			oid = o
		} else if IsHexOID(rev) {
			oid = model.OID(rev)
		}
	}
	if oid.IsZero() {
		return "", status.Errorf(codes.NotFound, "gitaly: revision %q not found", rev+tail)
	}
	return walkAncestors(ctx, s, repo, oid, tail)
}

// walkAncestors applies a ^/~ suffix to a resolved OID.
func walkAncestors(ctx context.Context, s *Base, repo string, oid model.OID, tail string) (model.OID, error) {
	for i := 0; i < len(tail); {
		c := tail[i]
		i++
		switch c {
		case '^':
			n := 1
			if i < len(tail) && tail[i] >= '0' && tail[i] <= '9' {
				n = 0
				for i < len(tail) && tail[i] >= '0' && tail[i] <= '9' {
					n = n*10 + int(tail[i]-'0')
					i++
				}
			}
			if n == 0 {
				return "", status.Error(codes.InvalidArgument, "gitaly: ^0 resolves to the commit itself — not a valid ancestor")
			}
			var err error
			oid, err = nthParent(ctx, s, repo, oid, n-1)
			if err != nil {
				return "", err
			}
		case '~':
			n := 1
			if i < len(tail) && tail[i] >= '0' && tail[i] <= '9' {
				n = 0
				for i < len(tail) && tail[i] >= '0' && tail[i] <= '9' {
					n = n*10 + int(tail[i]-'0')
					i++
				}
			}
			for k := 0; k < n; k++ {
				var err error
				oid, err = nthParent(ctx, s, repo, oid, 0)
				if err != nil {
					return "", err
				}
			}
		default:
			return "", status.Errorf(codes.InvalidArgument, "gitaly: unsupported revision suffix %q", tail)
		}
	}
	return oid, nil
}

func nthParent(ctx context.Context, s *Base, repo string, oid model.OID, n int) (model.OID, error) {
	ci, err := s.Objects.CommitInfo(repo, oid)
	if err != nil {
		return "", status.Errorf(codes.NotFound, "gitaly: object %s not found", oid)
	}
	if n >= len(ci.Parents) {
		return "", status.Errorf(codes.NotFound, "gitaly: commit %s has no parent %d", oid, n+1)
	}
	return ci.Parents[n], nil
}

func IsHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// gitCommit loads and converts a commit object into the wire shape.
func (s *Base) GitCommit(ctx context.Context, repo string, oid model.OID) (*gitalypb.GitCommit, error) {
	typ, raw, err := s.Objects.Raw(ctx, repo, oid)
	if err != nil || typ != model.CommitType {
		return nil, status.Errorf(codes.NotFound, "gitaly: commit %s not found", oid)
	}
	c, err := model.DecodeCommit(raw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: commit %s undecodable: %v", oid, err)
	}
	subject, body := splitMessage(c.Message)
	return &gitalypb.GitCommit{
		Id:        oid.String(),
		Subject:   subject,
		Body:      body,
		Author:    commitAuthor(c.Author),
		Committer: commitAuthor(c.Committer),
		ParentIds: oidsToStrings(c.Parents),
	}, nil
}

// splitMessage cuts subject (first paragraph) from body per git conventions.
func splitMessage(msg string) (subject, body []byte) {
	msg = strings.TrimSuffix(msg, "\n")
	if i := strings.Index(msg, "\n\n"); i >= 0 {
		return []byte(msg[:i]), []byte(strings.TrimPrefix(msg[i+2:], "\n"))
	}
	return []byte(msg), nil
}

func commitAuthor(id model.Identity) *gitalypb.CommitAuthor {
	return &gitalypb.CommitAuthor{
		Name:  []byte(id.Name),
		Email: []byte(id.Email),
		Date:  timestamppb.New(id.When),
	}
}

func oidsToStrings(oids []model.OID) []string {
	out := make([]string, len(oids))
	for i, o := range oids {
		out[i] = o.String()
	}
	return out
}

// walkHistory yields commits reachable from heads in commit-time-first order
// (walkParents, commit_service.go); isAncestor is its boolean specialization.
func (s *Base) WalkHistory(ctx context.Context, repo string, heads []model.OID, visit func(oid model.OID, ci *CommitRecord) bool) error {
	return s.WalkParents(ctx, repo, heads, &WalkFilter{}, visit)
}

type CommitRecord struct {
	Tree    model.OID
	Parents []model.OID
	When    int64
}

func (s *Base) CommitRecord(ctx context.Context, repo string, oid model.OID) (*CommitRecord, error) {
	ci, err := s.Objects.CommitInfo(repo, oid)
	if err != nil {
		// Lazy metadata may not cover imported histories yet; fall back to
		// the raw object.
		typ, raw, rerr := s.Objects.Raw(ctx, repo, oid)
		if rerr != nil || typ != model.CommitType {
			return nil, fmt.Errorf("commit %s: %w", oid, err)
		}
		c, derr := model.DecodeCommit(raw)
		if derr != nil {
			return nil, fmt.Errorf("commit %s: %w", oid, derr)
		}
		return &CommitRecord{Tree: c.TreeOID, Parents: c.Parents, When: c.Committer.When.Unix()}, nil
	}
	return &CommitRecord{Tree: ci.Tree, Parents: ci.Parents, When: ci.When}, nil
}

// isAncestor answers whether anc is reachable from desc.
func (s *Base) IsAncestor(ctx context.Context, repo string, anc, desc model.OID) (bool, error) {
	found := false
	err := s.WalkHistory(ctx, repo, []model.OID{desc}, func(oid model.OID, _ *CommitRecord) bool {
		if oid == anc {
			found = true
			return false
		}
		return true
	})
	return found, err
}

// BundleRef is one advertised reference line.
type BundleRef struct {
	OID model.OID
	Ref string
}

// BundleClosure walks the union closure of the refs' objects (commits,
// trees, blobs and tag objects) in a stable order.
func (s *Base) BundleClosure(ctx context.Context, repo string, refs []BundleRef) ([]model.OID, error) {
	lookup := s.Objects.Lookup(repo)
	var order []model.OID
	seen := map[model.OID]bool{}
	add := func(oid model.OID) {
		if !seen[oid] {
			seen[oid] = true
			order = append(order, oid)
		}
	}
	var addTree func(tree model.OID)
	addTree = func(tree model.OID) {
		t, err := lookup.Tree(tree)
		if err != nil {
			return
		}
		add(tree)
		for _, e := range t.Entries {
			if e.IsDir {
				addTree(e.Target)
				continue
			}
			if e.Mode != "160000" {
				add(e.Target)
			}
		}
	}
	var heads []model.OID
	for _, r := range refs {
		typ, _, err := s.Objects.Raw(ctx, repo, r.OID)
		if err != nil {
			return nil, fmt.Errorf("bundle ref %s: %w", r.Ref, err)
		}
		switch typ {
		case model.TagType:
			add(r.OID)
			target, perr := s.PeelToCommit(ctx, repo, r.OID)
			if perr == nil {
				heads = append(heads, target)
			}
		case model.CommitType:
			heads = append(heads, r.OID)
		}
	}
	if err := s.WalkParents(ctx, repo, heads, &WalkFilter{}, func(oid model.OID, rec *CommitRecord) bool {
		add(oid)
		addTree(rec.Tree)
		return true
	}); err != nil {
		return nil, err
	}
	return order, nil
}

// WalkFilter carries the ListCommits/CountCommits query knobs the pure-Go
// walk understands; unsupported knobs are ignored where upstream treats them
// as optional, and rejected where ignoring would lie (docs/integration/gitaly-compat.md matrix).
type WalkFilter struct {
	FirstParent bool
	MaxParents  uint32
	Author      string
	After       int64
	Before      int64
	Exclude     map[model.OID]bool
}

// walkParents is walkHistory with exclusion/first-parent semantics.
func (s *Base) WalkParents(ctx context.Context, repo string, heads []model.OID, f *WalkFilter, visit func(model.OID, *CommitRecord) bool) error {
	type item struct {
		oid model.OID
		rec *CommitRecord
		seq int
	}
	var q []item
	seen := map[model.OID]bool{}
	seq := 0
	enqueue := func(oid model.OID) bool {
		if seen[oid] {
			return true
		}
		rec, err := s.CommitRecord(ctx, repo, oid)
		if err != nil {
			return false
		}
		seen[oid] = true
		seq++
		q = append(q, item{oid, rec, seq})
		return true
	}
	for _, h := range heads {
		if f.Exclude[h] {
			continue
		}
		enqueue(h)
	}
	for len(q) > 0 {
		// Commit-time descending; same-second commits surface tips before
		// their parents — earlier enqueue wins (rev-list's tie order).
		best := 0
		for i := range q {
			if q[i].rec.When > q[best].rec.When || (q[i].rec.When == q[best].rec.When && q[i].seq < q[best].seq) {
				best = i
			}
		}
		cur := q[best]
		q = append(q[:best], q[best+1:]...)
		if !visit(cur.oid, cur.rec) {
			return nil
		}
		parents := cur.rec.Parents
		if f.FirstParent && len(parents) > 0 {
			parents = parents[:1]
		}
		for _, p := range parents {
			if !f.Exclude[p] {
				enqueue(p)
			}
		}
	}
	return nil
}

// peelToCommit follows annotated-tag chains down to the commit.
func (s *Base) PeelToCommit(ctx context.Context, repo string, oid model.OID) (model.OID, error) {
	for i := 0; i < 8; i++ {
		typ, raw, err := s.Objects.Raw(ctx, repo, oid)
		if err != nil {
			return "", status.Errorf(codes.NotFound, "gitaly: object %s not found", oid)
		}
		switch typ {
		case model.CommitType:
			return oid, nil
		case model.TagType:
			tag, err := model.DecodeTag(raw)
			if err != nil {
				return "", status.Error(codes.Internal, err.Error())
			}
			oid = tag.Object
		default:
			return "", status.Errorf(codes.InvalidArgument, "gitaly: object %s is a %s, not a commit", oid, typ)
		}
	}
	return "", status.Error(codes.Internal, "gitaly: tag chain too deep")
}

func (f *WalkFilter) Matches(oid model.OID, rec *CommitRecord, raw func(model.OID) (*model.AnchoredCommit, bool)) bool {
	if f.MaxParents > 0 && len(rec.Parents) > int(f.MaxParents) {
		return false
	}
	if f.After != 0 && rec.When <= f.After {
		return false
	}
	if f.Before != 0 && rec.When >= f.Before {
		return false
	}
	if f.Author != "" && raw != nil {
		if c, ok := raw(oid); ok {
			if !strings.Contains(c.Author.Name, f.Author) && !strings.Contains(c.Author.Email, f.Author) {
				return false
			}
		}
	}
	return true
}

// mergeBaseOf is the two-revision single-base walk behind FindMergeBase and
// the diverging count: A's ancestor set, then B's newest walk hit.
func (s *Base) MergeBaseOf(ctx context.Context, repo string, a, b model.OID) (model.OID, error) {
	ancestors := map[model.OID]bool{}
	if err := s.WalkParents(ctx, repo, []model.OID{a}, &WalkFilter{}, func(oid model.OID, _ *CommitRecord) bool {
		ancestors[oid] = true
		return true
	}); err != nil {
		return "", err
	}
	var found model.OID
	if err := s.WalkParents(ctx, repo, []model.OID{b}, &WalkFilter{}, func(oid model.OID, _ *CommitRecord) bool {
		if !found.IsZero() {
			return false
		}
		if ancestors[oid] {
			found = oid
			return false
		}
		return true
	}); err != nil {
		return "", err
	}
	return found, nil
}

// RawAuth injects the verbatim Authorization header value GitLab hands
// the RPC ("Bearer …", "Basic …") into every smart-HTTP request.
type RawAuth struct{ Value string }

func (a RawAuth) SetAuth(r *http.Request) {
	if a.Value != "" {
		r.Header.Set("Authorization", a.Value)
	}
}
func (a RawAuth) Name() string   { return "raw-authorization" }
func (a RawAuth) String() string { return "raw-authorization" }
