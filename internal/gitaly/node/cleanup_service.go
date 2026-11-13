package node

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CleanupService: history rewrite in pure Go — no git-filter-repo(1)
// subprocess (docs/integration/gitaly-compat.md). RewriteHistory rebuilds every commit whose tree or
// parents change: blobs named by OID are stripped, redaction patterns are
// replaced with "***REMOVED***" (literal and regex forms, the subset that
// covers GitLab's redaction payloads), and every ref is re-pointed at the
// rewritten history. Like upstream, the removed objects stay in storage
// until garbage collection removes them.
type cleanupService struct {
	gitalypb.UnimplementedCleanupServiceServer
	*kit.Base
}

// bfgEntryLimit mirrors upstream's chunker batch size for map entries.
const bfgEntryLimit = 500

// internalRefPrefixes are GitLab's internal reference namespaces
// (upstream git.InternalRefPrefixes): refs the cleaner removes when the
// commit they anchor is rewritten out of history.
var internalRefPrefixes = []string{
	"refs/environments/",
	"refs/keep-around/",
	"refs/merge-requests/",
	"refs/pipelines/",
	"refs/remotes/",
}

// redactor is one compiled --replace-text rule (git-filter-repo form:
// "literal:x==>y", "regex:x==>y", or a bare literal; missing replacement
// defaults to ***REMOVED***).
type redactor struct {
	literal []byte
	re      *regexp.Regexp
	replace []byte
}

func compileRedactors(patterns [][]byte) ([]redactor, error) {
	out := make([]redactor, 0, len(patterns))
	for _, p := range patterns {
		if bytes.ContainsAny(p, "\n\r") {
			return nil, status.Error(codes.InvalidArgument, "redaction pattern contains newline")
		}
		r := redactor{replace: []byte("***REMOVED***")}
		pat := p
		kind := "literal"
		if i := bytes.Index(pat, []byte(":")); i >= 0 {
			switch string(pat[:i]) {
			case "literal", "regex":
				kind = string(pat[:i])
				pat = pat[i+1:]
			}
		}
		if i := bytes.Index(pat, []byte("==>")); i >= 0 {
			r.replace = append([]byte(nil), pat[i+3:]...)
			pat = pat[:i]
		}
		switch kind {
		case "literal":
			if len(pat) == 0 {
				return nil, status.Error(codes.InvalidArgument, "redaction pattern is empty")
			}
			r.literal = append([]byte(nil), pat...)
		default:
			re, err := regexp.Compile(string(pat))
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "redaction regex: %v", err)
			}
			r.re = re
		}
		out = append(out, r)
	}
	return out, nil
}

func applyRedactors(rs []redactor, content []byte) ([]byte, bool) {
	changed := false
	for _, r := range rs {
		if r.literal != nil {
			if bytes.Contains(content, r.literal) {
				content = bytes.ReplaceAll(content, r.literal, r.replace)
				changed = true
			}
			continue
		}
		if r.re.Match(content) {
			content = r.re.ReplaceAll(content, r.replace)
			changed = true
		}
	}
	return content, changed
}

// RewriteHistory rewrites the repository's history (client-streamed rules,
// empty response on success — the object map rides ApplyBfgObjectMapStream
// for tools that want it).
func (cs *cleanupService) RewriteHistory(stream gitalypb.CleanupService_RewriteHistoryServer) error {
	ctx := stream.Context()
	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving initial request: %v", err)
	}
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	req.Repository = nil

	var blobs []model.OID
	var patterns [][]byte
	for {
		if req.GetRepository() != nil {
			return status.Error(codes.InvalidArgument, "subsequent requests must not contain repository")
		}
		for _, oid := range req.GetBlobs() {
			o := model.OID(strings.ToLower(oid))
			if !kit.IsHexOID(string(o)) {
				return status.Errorf(codes.InvalidArgument, "validating object ID: %s", oid)
			}
			blobs = append(blobs, o)
		}
		for _, p := range req.GetRedactions() {
			if strings.Contains(string(p), "\n") {
				return status.Error(codes.InvalidArgument, "redaction pattern contains newline")
			}
			patterns = append(patterns, p)
		}
		req, err = stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if len(blobs) == 0 && len(patterns) == 0 {
		return status.Error(codes.InvalidArgument, "no object IDs or text replacements specified")
	}
	redactors, err := compileRedactors(patterns)
	if err != nil {
		return err
	}

	if err := cs.rewriteRepo(ctx, name, blobs, redactors); err != nil {
		return err
	}
	return stream.SendAndClose(&gitalypb.RewriteHistoryResponse{})
}

// rewriteRepo rebuilds the reachable history and re-points every ref.
func (cs *cleanupService) rewriteRepo(ctx context.Context, repo string, blobs []model.OID, redactors []redactor) error {
	refs, err := cs.AdvertisedRefs(ctx, repo)
	if err != nil {
		return err
	}
	strip := make(map[model.OID]bool, len(blobs))
	for _, b := range blobs {
		strip[b] = true
	}

	engine := &rewriteEngine{
		b: cs.Base, repo: repo, strip: strip, redactors: redactors,
		trees: map[model.OID]model.OID{}, commits: map[model.OID]model.OID{},
		pending: map[model.OID]bool{},
	}
	// Peel every ref to commits: heads and aux refs point at commits
	// directly; annotated tags point through a tag object.
	tips := make([]model.OID, 0, len(refs))
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		c, err := cs.PeelToCommit(ctx, repo, r.Head)
		if err == nil {
			tips = append(tips, c)
		}
	}
	for _, tip := range tips {
		if _, err := engine.commit(ctx, tip); err != nil {
			return status.Errorf(codes.Internal, "rewrite: %v", err)
		}
	}

	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		newOID, err := engine.refTarget(ctx, repo, r.Name, r.Head)
		if err != nil {
			return status.Errorf(codes.Internal, "rewrite ref %s: %v", r.Name, err)
		}
		if newOID == r.Head {
			continue
		}
		// The rewritten history is disjoint from the old one. Force-updating
		// the ref alone would keep weaving the old chain in as ancestry
		// (docs/gateway/git-compat.md §6a); the timeline is dropped and recreated at the new
		// tip and the branch's woven family reset, so the advertised
		// history contains rewritten commits only. Old objects stay in
		// storage for garbage collection — like upstream.
		if err := cs.Svc.UpdateRef(ctx, service.RefUpdate{Repo: repo, Name: r.Name}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
		if err := cs.Svc.UpdateRef(ctx, service.RefUpdate{Repo: repo, Name: r.Name, New: newOID}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	// Re-derive every affected branch's woven view from the new source
	// chains (the force-push rebuild, docs/gateway/git-compat.md §6a).
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		if err := cs.Svc.Views.ResetWoven(ctx, repo, strings.TrimPrefix(r.Name, "refs/heads/")); err != nil {
			return status.Errorf(codes.Internal, "gitaly: %v", err)
		}
	}
	return nil
}

// rewriteEngine memoizes the old→new mapping while rebuilding history
// parents-first (recursion with a pending set breaks DAG cycles).
type rewriteEngine struct {
	b         *kit.Base
	repo      string
	strip     map[model.OID]bool
	redactors []redactor

	trees   map[model.OID]model.OID
	commits map[model.OID]model.OID
	pending map[model.OID]bool
}

func (e *rewriteEngine) commit(ctx context.Context, oid model.OID) (model.OID, error) {
	if n, ok := e.commits[oid]; ok {
		return n, nil
	}
	if e.pending[oid] {
		return "", fmt.Errorf("commit cycle at %s", oid)
	}
	e.pending[oid] = true
	defer delete(e.pending, oid)

	typ, raw, err := e.b.Objects.Raw(ctx, e.repo, oid)
	if err != nil || typ != model.CommitType {
		return "", fmt.Errorf("commit %s: %w", oid, err)
	}
	c, err := model.DecodeCommit(raw)
	if err != nil {
		return "", fmt.Errorf("decode commit %s: %w", oid, err)
	}
	newTree, err := e.tree(ctx, c.TreeOID)
	if err != nil {
		return "", err
	}
	newParents := make([]model.OID, len(c.Parents))
	for i, p := range c.Parents {
		np, err := e.commit(ctx, p)
		if err != nil {
			return "", err
		}
		newParents[i] = np
	}
	c.TreeOID = newTree
	c.Parents = newParents
	enc := c.Encode()
	newOID := model.ObjectOID(model.CommitType, enc, e.b.Algo)
	if newOID != oid {
		if err := e.b.Svc.PutObjects(ctx, e.repo, []service.PutObject{{
			OID: newOID, Type: "commit", Data: enc}}); err != nil {
			return "", err
		}
	}
	e.commits[oid] = newOID
	return newOID, nil
}

func (e *rewriteEngine) tree(ctx context.Context, oid model.OID) (model.OID, error) {
	if n, ok := e.trees[oid]; ok {
		return n, nil
	}
	t, err := e.b.Objects.Lookup(e.repo).Tree(oid)
	if err != nil {
		return "", fmt.Errorf("tree %s: %w", oid, err)
	}
	changed := false
	entries := make([]model.TreeEntry, 0, len(t.Entries))
	for _, entry := range t.Entries {
		switch {
		case entry.IsDir:
			sub, err := e.tree(ctx, entry.Target)
			if err != nil {
				return "", err
			}
			if sub != entry.Target {
				entry.Target = sub
				changed = true
			}
			entries = append(entries, entry)
		case entry.Mode == "160000":
			entries = append(entries, entry) // gitlink: opaque commit reference
		case e.strip[entry.Target]:
			changed = true // stripped: entry drops out entirely
		default:
			newOID, did, err := e.blob(ctx, entry.Target)
			if err != nil {
				return "", err
			}
			if did {
				entry.Target = newOID
				changed = true
			}
			entries = append(entries, entry)
		}
	}
	if !changed {
		e.trees[oid] = oid
		return oid, nil
	}
	nt := &model.Tree{Entries: entries}
	enc, err := nt.Encode()
	if err != nil {
		return "", err
	}
	newOID := model.ObjectOID(model.TreeType, enc, e.b.Algo)
	if err := e.b.Svc.PutObjects(ctx, e.repo, []service.PutObject{{
		OID: newOID, Type: "tree", Data: enc}}); err != nil {
		return "", err
	}
	e.trees[oid] = newOID
	return newOID, nil
}

func (e *rewriteEngine) blob(ctx context.Context, oid model.OID) (model.OID, bool, error) {
	typ, raw, err := e.b.Objects.Raw(ctx, e.repo, oid)
	if err != nil || typ != model.BlobType {
		return oid, false, nil // unknown targets ride along untouched
	}
	content, changed := applyRedactors(e.redactors, raw)
	if !changed {
		return oid, false, nil
	}
	newOID := model.ObjectOID(model.BlobType, content, e.b.Algo)
	if err := e.b.Svc.PutObjects(ctx, e.repo, []service.PutObject{{
		OID: newOID, Type: "blob", Data: content}}); err != nil {
		return "", false, err
	}
	return newOID, true, nil
}

// refTarget maps one ref's advertised target through the rewrite: annotated
// tag objects are re-encoded against the mapped commit; direct commit refs
// follow the commit map. Unrewritten targets answer unchanged.
func (e *rewriteEngine) refTarget(ctx context.Context, repo, ref string, target model.OID) (model.OID, error) {
	typ, raw, err := e.b.Objects.Raw(ctx, repo, target)
	if err != nil {
		return target, nil
	}
	switch typ {
	case model.CommitType:
		if n, ok := e.commits[target]; ok {
			return n, nil
		}
		return target, nil
	case model.TagType:
		// Rewrite the "object <oid>" header line when the tagged commit
		// moved; the rest of the tag object rides along verbatim.
		peeled, err := e.b.PeelToCommit(ctx, repo, target)
		if err != nil {
			return target, nil
		}
		mapped, ok := e.commits[peeled]
		if !ok || mapped == peeled {
			return target, nil
		}
		oldLine := []byte("object " + string(peeled))
		newLine := []byte("object " + string(mapped))
		if !bytes.Contains(raw, oldLine) {
			return target, nil
		}
		nraw := bytes.Replace(raw, oldLine, newLine, 1)
		newOID := model.ObjectOID(model.TagType, nraw, e.b.Algo)
		if err := e.b.Svc.PutObjects(ctx, repo, []service.PutObject{{
			OID: newOID, Type: "tag", Data: nraw}}); err != nil {
			return "", err
		}
		return newOID, nil
	default:
		return target, nil
	}
}

// ApplyBfgObjectMapStream applies a BFG / filter-repo object map
// ("old-sha new-sha" lines): internal references anchored at rewritten
// commits are removed, and each mapping streams back as an entry.
// Non-internal refs are left alone — the map's producer rewrites those.
func (cs *cleanupService) ApplyBfgObjectMapStream(stream gitalypb.CleanupService_ApplyBfgObjectMapStreamServer) error {
	ctx := stream.Context()
	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving initial request: %v", err)
	}
	name, err := cs.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}

	// object-map bytes arrive across messages; lines may split on chunk
	// boundaries, so buffer until the stream ends.
	var buf []byte
	for {
		buf = append(buf, req.GetObjectMap()...)
		req, err = stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	internalRefs, err := cs.internalRefTable(ctx, name)
	if err != nil {
		return err
	}
	removed := map[string]bool{}

	var entries []*gitalypb.ApplyBfgObjectMapStreamResponse_Entry
	flush := func() error {
		if len(entries) == 0 {
			return nil
		}
		if err := stream.Send(&gitalypb.ApplyBfgObjectMapStreamResponse{Entries: entries}); err != nil {
			return err
		}
		entries = nil
		return nil
	}

	lines := strings.Split(string(buf), "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "old") {
			continue // filter-repo's header row
		}
		oldOID, newOID, ok := strings.Cut(line, " ")
		if !ok {
			return status.Errorf(codes.InvalidArgument, "object map invalid at line %d", i)
		}
		o, n := model.OID(strings.ToLower(oldOID)), model.OID(strings.ToLower(newOID))
		if !kit.IsHexOID(string(o)) {
			return status.Errorf(codes.InvalidArgument, "invalid old object ID at line %d", i)
		}
		if !kit.IsHexOID(string(n)) {
			return status.Errorf(codes.InvalidArgument, "invalid new object ID at line %d", i)
		}
		if o == n {
			continue // considered but not modified
		}
		entries = append(entries, &gitalypb.ApplyBfgObjectMapStreamResponse_Entry{
			Type:   cs.objectTypeOf(ctx, name, n),
			OldOid: oldOID,
			NewOid: newOID,
		})
		if len(entries) >= bfgEntryLimit {
			if err := flush(); err != nil {
				return err
			}
		}
		for _, ref := range internalRefs[string(o)] {
			if removed[ref] {
				continue
			}
			removed[ref] = true
			if err := cs.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: ref}); err != nil {
				return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
			}
		}
	}
	return flush()
}

// internalRefTable indexes OID → internal ref names pointing at it.
func (cs *cleanupService) internalRefTable(ctx context.Context, repo string) (map[string][]string, error) {
	refs, err := cs.AdvertisedRefs(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, r := range refs {
		internal := false
		for _, p := range internalRefPrefixes {
			if strings.HasPrefix(r.Name, p) {
				internal = true
				break
			}
		}
		if !internal {
			continue
		}
		c, err := cs.PeelToCommit(ctx, repo, r.Head)
		if err != nil {
			continue
		}
		out[string(c)] = append(out[string(c)], r.Name)
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out, nil
}

func (cs *cleanupService) objectTypeOf(ctx context.Context, repo string, oid model.OID) gitalypb.ObjectType {
	typ, _, err := cs.Objects.Raw(ctx, repo, oid)
	if err != nil {
		return gitalypb.ObjectType_UNKNOWN
	}
	switch typ {
	case model.CommitType:
		return gitalypb.ObjectType_COMMIT
	case model.BlobType:
		return gitalypb.ObjectType_BLOB
	case model.TreeType:
		return gitalypb.ObjectType_TREE
	case model.TagType:
		return gitalypb.ObjectType_TAG
	default:
		return gitalypb.ObjectType_UNKNOWN
	}
}
