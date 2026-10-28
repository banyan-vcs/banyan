package gitaly

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// Repository search surface: file-name pathspec search, content grep with
// git-grep-shaped match blocks, .gitattributes resolution and license
// detection. All pure Go over the object plane.

// searchFilesFilterMaxLength caps the filter regex (upstream parity).
const searchFilesFilterMaxLength = 1000

func validateSearchFilesRequest(repo *gitalypb.Repository, ref []byte, query string) error {
	if query == "" {
		return status.Error(codes.InvalidArgument, "gitaly: no query given")
	}
	if len(ref) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: no ref given")
	}
	if ref[0] == '-' {
		return status.Error(codes.InvalidArgument, "gitaly: invalid ref argument")
	}
	if repo == nil {
		return status.Error(codes.InvalidArgument, "gitaly: repository is required")
	}
	return nil
}

// SearchFilesByName answers tree paths matching the query pathspec, filtered
// by an optional regex, paged by offset/limit — `git ls-tree <ref> <query>`
// semantics in one response.
func (rs *repositoryService) SearchFilesByName(req *gitalypb.SearchFilesByNameRequest, stream gitalypb.RepositoryService_SearchFilesByNameServer) error {
	ctx := stream.Context()
	if err := validateSearchFilesRequest(req.GetRepository(), req.GetRef(), req.GetQuery()); err != nil {
		return err
	}
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	var filter *regexp.Regexp
	if f := req.GetFilter(); f != "" {
		if len(f) > searchFilesFilterMaxLength {
			return status.Error(codes.InvalidArgument, "gitaly: filter exceeds maximum length")
		}
		filter, err = regexp.Compile(f)
		if err != nil {
			return status.Error(codes.InvalidArgument, "gitaly: filter did not compile: "+err.Error())
		}
	}
	oid, err := rs.s.resolveRevision(ctx, name, string(req.GetRef()))
	if err != nil {
		return err
	}
	rec, err := rs.s.commitRecord(ctx, name, oid)
	if err != nil {
		return err
	}
	paths, err := rs.s.listTreePaths(ctx, name, rec.Tree)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	var files [][]byte
	skipped := uint32(0)
	limit := req.GetLimit()
	for _, p := range paths {
		if !globMatch(req.GetQuery(), p) {
			continue
		}
		if filter != nil && !filter.MatchString(p) {
			continue
		}
		if skipped < req.GetOffset() {
			skipped++
			continue
		}
		if limit != 0 && uint32(len(files)) >= limit {
			break
		}
		files = append(files, []byte(p))
	}
	return stream.Send(&gitalypb.SearchFilesByNameResponse{Files: files})
}

// SearchFilesByContent greps tree blobs case-insensitively and streams one
// match block per hit group in `git grep --null -n -B2 -A2` shape:
// "<path>\0<lineno>:<text>" for matches, "<path>\0<lineno>-<text>" for
// context lines, blocks separated by "--\n" (each block ends with an
// EndOfMatch message; binary blobs are skipped like git -I).
func (rs *repositoryService) SearchFilesByContent(req *gitalypb.SearchFilesByContentRequest, stream gitalypb.RepositoryService_SearchFilesByContentServer) error {
	ctx := stream.Context()
	if err := validateSearchFilesRequest(req.GetRepository(), req.GetRef(), req.GetQuery()); err != nil {
		return err
	}
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	re, err := regexp.Compile("(?i)" + req.GetQuery())
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: query did not compile: "+err.Error())
	}
	oid, err := rs.s.resolveRevision(ctx, name, string(req.GetRef()))
	if err != nil {
		return err
	}
	rec, err := rs.s.commitRecord(ctx, name, oid)
	if err != nil {
		return err
	}
	blobs, err := rs.s.listTreeBlobPaths(ctx, name, rec.Tree)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	for _, bp := range blobs {
		_, raw, err := rs.s.Objects.Raw(ctx, name, bp.oid)
		if err != nil || looksBinary(raw) {
			continue
		}
		block := grepBlock(bp.path, string(raw), re)
		if block == "" {
			continue
		}
		if err := streamBytes([]byte(block), func(p []byte) error {
			return stream.Send(&gitalypb.SearchFilesByContentResponse{MatchData: p})
		}); err != nil {
			return err
		}
		if err := stream.Send(&gitalypb.SearchFilesByContentResponse{EndOfMatch: true}); err != nil {
			return err
		}
	}
	return nil
}

// grepBlock renders one git-grep match group: hits with ±2 context lines,
// adjacent groups fused, separator "-" lines between distant groups.
func grepBlock(path, content string, re *regexp.Regexp) string {
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	type span struct{ from, to int }
	var spans []span
	for i, l := range lines {
		if re.MatchString(l) {
			if n := len(spans); n > 0 && i <= spans[n-1].to+2 {
				spans[n-1].to = i + 2 // fuse: within two context lines
			} else {
				spans = append(spans, span{i - 2, i + 2})
			}
		}
	}
	var b strings.Builder
	prevEnd := -10
	for _, sp := range spans {
		from, to := sp.from, sp.to
		if from < 0 {
			from = 0
		}
		if to > len(lines)-1 {
			to = len(lines) - 1
		}
		if prevEnd >= 0 && from > prevEnd+1 {
			b.WriteString("--\n")
		}
		for i := from; i <= to; i++ {
			mark := "-"
			if re.MatchString(lines[i]) {
				mark = ":"
			}
			b.WriteString(path)
			b.WriteByte(0)
			b.WriteString(itoa(uint32(i + 1)))
			b.WriteString(mark)
			b.WriteString(lines[i])
			b.WriteByte('\n')
		}
		prevEnd = to
	}
	return b.String()
}

// listTreePaths walks a tree collecting file paths (gitlinks skipped), in
// tree order.
func (s *Server) listTreePaths(ctx context.Context, repo string, tree model.OID) ([]string, error) {
	blobs, err := s.listTreeBlobPaths(ctx, repo, tree)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(blobs))
	for _, b := range blobs {
		out = append(out, b.path)
	}
	return out, nil
}

type treeBlob struct {
	path string
	oid  model.OID
}

func (s *Server) listTreeBlobPaths(ctx context.Context, repo string, tree model.OID) ([]treeBlob, error) {
	lookup := s.Objects.Lookup(repo)
	var out []treeBlob
	var walk func(tree model.OID, prefix string) error
	walk = func(tree model.OID, prefix string) error {
		t, err := lookup.Tree(tree)
		if err != nil {
			return nil // unreadable subtree: report what is readable
		}
		for _, e := range t.Entries {
			switch {
			case e.IsDir:
				if err := walk(e.Target, prefix+e.Name+"/"); err != nil {
					return err
				}
			case e.Mode == "160000":
				continue
			default:
				out = append(out, treeBlob{path: prefix + e.Name, oid: e.Target})
			}
		}
		return nil
	}
	if err := walk(tree, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- GetFileAttributes ----

type attrRule struct {
	pattern string
	attrs   map[string]string // name -> "set" | "unset" | "unspecified" | value
}

// parseAttributes decodes .gitattributes lines: "<pattern> <attr> <attr=value> <-attr> <!attr>...".
func parseAttributes(data string) []attrRule {
	var rules []attrRule
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		r := attrRule{pattern: strings.TrimPrefix(fields[0], "/"), attrs: map[string]string{}}
		for _, f := range fields[1:] {
			switch {
			case strings.HasPrefix(f, "!"):
				r.attrs[f[1:]] = "unspecified"
			case strings.HasPrefix(f, "-"):
				r.attrs[f[1:]] = "unset"
			case strings.Contains(f, "="):
				kv := strings.SplitN(f, "=", 2)
				r.attrs[kv[0]] = kv[1]
			default:
				r.attrs[f] = "set"
			}
		}
		rules = append(rules, r)
	}
	return rules
}

// attrPatternMatch applies git's attribute pattern rules: a pattern without
// a slash matches the basename; one with slashes matches the whole path
// (both anchored to the .gitattributes directory, here the root).
func attrPatternMatch(pattern, path string) bool {
	if !strings.Contains(pattern, "/") {
		base := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base = path[i+1:]
		}
		return globMatch(pattern, base)
	}
	return globMatch(pattern, path)
}

// GetFileAttributes resolves requested attributes for paths against the tip
// tree's root .gitattributes (`git check-attr --stdin` semantics: every
// requested attribute is answered, "unspecified" when no rule speaks).
// Later rules override earlier ones, matching git.
func (rs *repositoryService) GetFileAttributes(ctx context.Context, req *gitalypb.GetFileAttributesRequest) (*gitalypb.GetFileAttributesResponse, error) {
	if len(req.GetRevision()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "gitaly: revision is required")
	}
	if len(req.GetPaths()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "gitaly: file paths are required")
	}
	if len(req.GetAttributes()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "gitaly: attributes are required")
	}
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	oid, err := rs.s.resolveRevision(ctx, name, string(req.GetRevision()))
	if err != nil {
		return nil, err
	}
	rec, err := rs.s.commitRecord(ctx, name, oid)
	if err != nil {
		return nil, err
	}
	var rules []attrRule
	if entry, aerr := rs.s.entryAtPath(ctx, name, rec.Tree, ".gitattributes"); aerr == nil && entry != nil && !entry.IsDir {
		if _, raw, rerr := rs.s.Objects.Raw(ctx, name, entry.Target); rerr == nil {
			rules = parseAttributes(string(raw))
		}
	}
	out := &gitalypb.GetFileAttributesResponse{}
	for _, path := range req.GetPaths() {
		for _, attr := range req.GetAttributes() {
			value := "unspecified"
			for _, r := range rules {
				if v, ok := r.attrs[attr]; ok && attrPatternMatch(r.pattern, path) {
					value = v
				}
			}
			out.AttributeInfos = append(out.AttributeInfos, &gitalypb.GetFileAttributesResponse_AttributeInfo{
				Path: path, Attribute: attr, Value: value})
		}
	}
	return out, nil
}

// ---- FindLicense ----

// licenseFingerprints maps characteristic phrases to SPDX identifiers; the
// list is a hand-curated subset of the SPDX library upstream embeds (docs/19
// §6 registers the approximation).
var licenseFingerprints = []struct {
	id   string
	hits []string
}{
	{"agpl-3.0", []string{"gnu affero general public license"}},
	{"lgpl-3.0", []string{"gnu lesser general public license", "version 3"}},
	{"lgpl-2.1", []string{"gnu lesser general public license", "version 2.1"}},
	{"gpl-3.0", []string{"gnu general public license", "version 3"}},
	{"gpl-2.0", []string{"gnu general public license", "version 2"}},
	{"apache-2.0", []string{"apache license", "version 2.0"}},
	{"mpl-2.0", []string{"mozilla public license", "version 2.0"}},
	{"mit", []string{"permission is hereby granted, free of charge, to any person obtaining a copy"}},
	{"bsd-3-clause", []string{"redistribution and use in source and binary forms", "neither the name"}},
	{"bsd-2-clause", []string{"redistribution and use in source and binary forms"}},
	{"isc", []string{"permission to use, copy, modify, and/or distribute this software"}},
	{"unlicense", []string{"free and unencumbered software released into the public domain"}},
}

// licenseCandidates are root-entry name prefixes/shapes the detector reads.
func licenseCandidate(name string) bool {
	base := strings.ToLower(name)
	return strings.HasPrefix(base, "license") || strings.HasPrefix(base, "copying") ||
		strings.HasPrefix(base, "copyright") || strings.HasPrefix(base, "notice")
}

// FindLicense detects the repository license from root candidate files.
func (rs *repositoryService) FindLicense(ctx context.Context, req *gitalypb.FindLicenseRequest) (*gitalypb.FindLicenseResponse, error) {
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	oid, err := rs.s.resolveRevision(ctx, name, "HEAD")
	if err != nil {
		return nil, err
	}
	rec, err := rs.s.commitRecord(ctx, name, oid)
	if err != nil {
		return nil, err
	}
	root, err := rs.s.Objects.Lookup(name).Tree(rec.Tree)
	if err != nil {
		return &gitalypb.FindLicenseResponse{}, nil
	}
	var candidates []model.TreeEntry
	for _, e := range root.Entries {
		if !e.IsDir && e.Mode != "160000" && licenseCandidate(e.Name) {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return &gitalypb.FindLicenseResponse{}, nil
	}
	// Longest candidate first: LICENSE over LICENSE.txt when both exist.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	for _, c := range candidates {
		_, raw, err := rs.s.Objects.Raw(ctx, name, c.Target)
		if err != nil || looksBinary(raw) {
			continue
		}
		text := normalizeLicenseText(string(raw))
		for _, fp := range licenseFingerprints {
			all := true
			for _, h := range fp.hits {
				if !strings.Contains(text, normalizeLicenseText(h)) {
					all = false
					break
				}
			}
			if all {
				return &gitalypb.FindLicenseResponse{LicenseShortName: fp.id, LicenseName: fp.id, LicensePath: c.Name}, nil
			}
		}
	}
	return &gitalypb.FindLicenseResponse{}, nil
}

// normalizeLicenseText lowercases and collapses all whitespace so phrase
// matching survives formatting differences.
func normalizeLicenseText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
