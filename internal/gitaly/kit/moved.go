package kit

// Shared helpers lifted from the families when gitaly split per-service:
// everything here is cross-family surface (see kit/base.go).

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// CustomHooksKey is the metadata blob key under which a repository's
// custom-hooks archive rides.
const CustomHooksKey = "custom-hooks.tar"

// BundleHeaderV2 is the v2 bundle magic line.
const BundleHeaderV2 = "# v2 git bundle"

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func StreamBytes(data []byte, send func([]byte) error) error {
	for off := 0; off < len(data); off += wireChunk {
		end := off + wireChunk
		if end > len(data) {
			end = len(data)
		}
		if err := send(data[off:end]); err != nil {
			return err
		}
	}
	return nil
}

// splitPath splits a slash path into clean segments.
func SplitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

// modeValue converts the octal tree mode ("100644") to its numeric form.
func ModeValue(mode string) int32 {
	v, err := strconv.ParseInt(mode, 8, 32)
	if err != nil {
		return 0
	}
	return int32(v)
}

func PaginationWindow(p *gitalypb.PaginationParameter) (limit, offset uint32) {
	if p == nil {
		return 0, 0
	}
	if l := p.GetLimit(); l > 0 {
		limit = uint32(l)
	}
	if tok := p.GetPageToken(); isAllDigits(tok) {
		n, _ := strconv.ParseUint(tok, 10, 32)
		offset = uint32(n)
	}
	return limit, offset
}

// repoChecksum is the show-ref --head XOR over the allowed ref set — the
// same value praefect's replica checksums compare (docs/integration/gitaly-compat.md).
func RepoChecksum(ctx context.Context, s *Base, name string) string {
	refs, err := s.AdvertisedRefs(ctx, name)
	if err != nil {
		return ""
	}
	// Fixed-width XOR: big.Int.Bytes() drops leading zero bytes and the
	// checksum loses a hex char whenever the XOR's first byte is 0 (1/256
	// by content — a lurking flake the fixed-width oracle caught).
	var acc [sha1.Size]byte
	for _, r := range refs {
		if !checksumRefsAllowed.MatchString(r.Name) {
			continue
		}
		h := sha1.Sum([]byte(string(r.Head) + " " + r.Name))
		for i := range acc {
			acc[i] ^= h[i]
		}
	}
	return hex.EncodeToString(acc[:])
}

// bundleRefsOf lists refs/heads and refs/tags of the advertised view, sorted
// by ref name with a trailing HEAD line — the exact shape `git bundle create
// --all` writes and clones guess their default branch from.
func (s *Base) BundleRefsOf(ctx context.Context, repo string) ([]BundleRef, error) {
	refs, err := s.AdvertisedRefs(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []BundleRef
	var headOID model.OID
	for _, r := range refs {
		switch {
		case r.Name == "HEAD":
			headOID = r.Head
		case strings.HasPrefix(r.Name, "refs/heads/"), strings.HasPrefix(r.Name, "refs/tags/"):
			out = append(out, BundleRef{OID: r.Head, Ref: r.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	if !headOID.IsZero() {
		out = append(out, BundleRef{OID: headOID, Ref: "HEAD"})
	}
	return out, nil
}

// writeBundle streams a complete v2 bundle for the given refs.
func (s *Base) WriteBundle(ctx context.Context, repo string, refs []BundleRef, w io.Writer) error {
	if len(refs) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: refusing to create empty bundle")
	}
	var b strings.Builder
	b.WriteString(BundleHeaderV2 + "\n")
	for _, r := range refs {
		b.WriteString(string(r.OID) + " " + r.Ref + "\n")
	}
	b.WriteString("\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	order, err := s.BundleClosure(ctx, repo, refs)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	i := 0
	return storage.StreamEncodePack(w, s.Algo, len(order), func(int) (model.ObjectType, []byte, error) {
		oid := order[i]
		i++
		typ, raw, err := s.Objects.Raw(ctx, repo, oid)
		return typ, raw, err
	})
}

const wireChunk = 1 << 16 // 64KiB wire chunks: a few messages per pack page

type TreeBlob struct {
	Path string
	OID  model.OID
}

func (s *Base) ListTreeBlobPaths(ctx context.Context, repo string, tree model.OID) ([]TreeBlob, error) {
	lookup := s.Objects.Lookup(repo)
	var out []TreeBlob
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
				out = append(out, TreeBlob{Path: prefix + e.Name, OID: e.Target})
			}
		}
		return nil
	}
	if err := walk(tree, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// checksumRefsAllowed mirrors upstream's git-show-ref filter: only HEAD and
// the well-known ref namespaces feed the checksum.
var checksumRefsAllowed = regexp.MustCompile(`\AHEAD|(refs/(heads|tags|keep-around|merge-requests|environments|notes)/)`)

// ingestBundle validates and applies a bundle into an existing repository.
func (s *Base) IngestBundle(ctx context.Context, name string, data []byte, updateHead bool) error {
	refs, prerequisites, pack, err := ParseBundleHeader(data)
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: "+err.Error())
	}
	for _, pre := range prerequisites {
		if _, _, perr := s.Objects.Raw(ctx, name, pre); perr != nil {
			return status.Errorf(codes.FailedPrecondition,
				"gitaly: repository lacks prerequisite commit %s", pre)
		}
	}
	if len(pack) == 0 {
		return status.Error(codes.InvalidArgument, "gitaly: bundle carries no pack")
	}
	dir, err := os.MkdirTemp("", "banyan-bundle-*")
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer os.RemoveAll(dir)
	packPath := filepath.Join(dir, "bundle.pack")
	if err := os.WriteFile(packPath, pack, 0o644); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if _, err := s.Objects.IngestPackFile(ctx, name, packPath, int64(len(pack))); err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: bundle pack rejected: %v", err)
	}
	for _, r := range refs {
		if r.Ref == "HEAD" {
			continue
		}
		if !strings.HasPrefix(r.Ref, "refs/") {
			continue
		}
		if err := s.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: r.Ref, New: r.OID}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	if updateHead {
		for _, r := range refs {
			if r.Ref == "HEAD" {
				// The HEAD line's object id is the tip; the default branch
				// name comes from the heads we just wrote that point there.
				tip := r.OID
				for _, h := range refs {
					if strings.HasPrefix(h.Ref, "refs/heads/") && h.OID == tip {
						if err := s.Meta.SetDefaultBranch(ctx, name, strings.TrimPrefix(h.Ref, "refs/heads/")); err != nil {
							return status.Error(codes.Internal, err.Error())
						}
						break
					}
				}
			}
		}
	}
	return nil
}

// writeSnapshot renders the bare-repo TAR (HEAD, packed-refs, pack + idx)
// to w — the shared core of GetSnapshot and the partition backup.
func (s *Base) WriteSnapshot(ctx context.Context, name string, w io.Writer) error {
	refs, err := s.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	def, _ := s.DefaultBranch(ctx, name)

	// One full-closure pack (heads + tags + aux refs, HEAD included).
	brefs := make([]BundleRef, 0, len(refs))
	for _, r := range refs {
		brefs = append(brefs, BundleRef{Ref: r.Name, OID: r.Head})
	}
	order, err := s.BundleClosure(ctx, name, brefs)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	var pack bytes.Buffer
	i := 0
	if err := storage.StreamEncodePack(&pack, s.Algo, len(order), func(int) (model.ObjectType, []byte, error) {
		oid := order[i]
		i++
		return s.Objects.Raw(ctx, name, oid)
	}); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	idx, err := writePackIdxV2(pack.Bytes(), s.Algo)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	// A pack is named by its trailing checksum — the 20 bytes git's index
	// also carries as the pack checksum — not by a whole-file digest.
	packSum := pack.Bytes()[len(pack.Bytes())-s.Algo.New().Size():]
	packName := "pack-" + hex.EncodeToString(packSum)

	// packed-refs: "<oid> <ref>" sorted; annotated tags carry their peeled
	// commit as a "^<oid>" row directly beneath.
	type refRow struct {
		line string
		peel string
	}
	rows := make([]refRow, 0, len(refs))
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		row := refRow{line: fmt.Sprintf("%s %s\n", r.Head, r.Name)}
		if typ, _, terr := s.Objects.Raw(ctx, name, r.Head); terr == nil && typ == model.TagType {
			if peeled, perr := s.PeelToCommit(ctx, name, r.Head); perr == nil {
				row.peel = fmt.Sprintf("^%s\n", peeled)
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].line < rows[j].line })
	var refRows strings.Builder
	for _, row := range rows {
		refRows.WriteString(row.line)
		refRows.WriteString(row.peel)
	}

	tw := tar.NewWriter(w)
	addFile := func(path string, body []byte) {
		_ = tw.WriteHeader(&tar.Header{
			Name: path, Mode: 0o644, Size: int64(len(body)),
			ModTime: time.Now(), Format: tar.FormatUSTAR,
		})
		_, _ = tw.Write(body)
	}
	addDir := func(path string) {
		_ = tw.WriteHeader(&tar.Header{
			Name: path, Mode: 0o755, Typeflag: tar.TypeDir,
			ModTime: time.Now(), Format: tar.FormatUSTAR,
		})
	}
	// git requires the standard bare skeleton: HEAD, config, objects/ and
	// refs/ directories (packed-refs alone does not create refs/).
	for _, d := range []string{"objects", "objects/pack", "objects/info", "refs", "refs/heads", "refs/tags"} {
		addDir(d)
	}
	addFile("HEAD", []byte("ref: refs/heads/"+def+"\n"))
	addFile("packed-refs", []byte(refRows.String()))
	addFile("objects/info/packs", []byte("P "+packName+".pack\n"))
	addFile("objects/pack/"+packName+".pack", pack.Bytes())
	addFile("objects/pack/"+packName+".idx", idx)
	addFile("config", []byte("[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"))
	if err := tw.Close(); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

// copyClosure walks the tip's commit/tree/blob closure in the source
// repository and persists every object into the target repository.
func (s *Base) CopyClosure(ctx context.Context, source, target string, tip model.OID) error {
	lookup := s.Objects.Lookup(source)
	var batch []service.PutObject
	seen := map[model.OID]bool{}
	add := func(oid model.OID) error {
		if seen[oid] {
			return nil
		}
		seen[oid] = true
		typ, raw, err := s.Objects.Raw(ctx, source, oid)
		if err != nil {
			return fmt.Errorf("copy %s: %w", oid, err)
		}
		batch = append(batch, service.PutObject{OID: oid, Type: string(typ), Data: raw})
		return nil
	}
	var addTree func(tree model.OID) error
	addTree = func(tree model.OID) error {
		if seen[tree] {
			return nil
		}
		if err := add(tree); err != nil {
			return err
		}
		t, err := lookup.Tree(tree)
		if err != nil {
			return nil
		}
		for _, e := range t.Entries {
			if e.IsDir {
				if err := addTree(e.Target); err != nil {
					return err
				}
			} else if e.Mode != "160000" {
				if err := add(e.Target); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := s.WalkParents(ctx, source, []model.OID{tip}, &WalkFilter{}, func(oid model.OID, rec *CommitRecord) bool {
		if err := add(oid); err != nil {
			return false
		}
		if err := addTree(rec.Tree); err != nil {
			return false
		}
		return true
	}); err != nil {
		return err
	}
	if len(batch) == 0 {
		return nil
	}
	return s.Svc.PutObjects(ctx, target, batch)
}

// renderArchive writes the archive bytes for one commit to w — the shared
// core of GetArchive and SSHUploadArchive's wire renderer (no path
// scoping: the upload-archive protocol archives whole trees).
func (s *Base) RenderArchive(ctx context.Context, name string,
	format gitalypb.GetArchiveRequest_Format, commitID, prefix string, w io.Writer) error {
	commitOID, err := s.ResolveRevision(ctx, name, commitID)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: invalid commitId: %s", commitID)
	}
	rec, err := s.CommitRecord(ctx, name, commitOID)
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: invalid commitId")
	}
	entries, err := s.CollectArchiveEntries(ctx, name, rec.Tree, "", false, nil)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	when := time.Unix(rec.When, 0).UTC()
	switch format {
	case gitalypb.GetArchiveRequest_ZIP:
		return WriteZipArchive(w, entries, prefix, when, name, s)
	case gitalypb.GetArchiveRequest_TAR_GZ:
		zw := gzip.NewWriter(w)
		zw.Header.OS = 3 // gzip -n matches git's compressor byte for byte
		if err := WriteTarArchive(zw, entries, prefix, when, commitOID, name, s); err != nil {
			return err
		}
		return zw.Close()
	default:
		return WriteTarArchive(w, entries, prefix, when, commitOID, name, s)
	}
}

func parseModeOctal(mode string) (int64, bool) {
	var v int64
	for _, c := range mode {
		if c < '0' || c > '7' {
			return 0, false
		}
		v = v*8 + int64(c-'0')
	}
	return v, true
}

// parseMode turns a tree-entry mode ("100644") into its integer form.
func ParseMode(mode string) int64 {
	v, _ := strconv.ParseInt(mode, 8, 64)
	return v
}

// parseBundleHeader splits a bundle into header refs, prerequisites and the
// pack payload offset.
func ParseBundleHeader(data []byte) (refs []BundleRef, prerequisites []model.OID, pack []byte, err error) {
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 || string(data[:nl]) != BundleHeaderV2 {
		if bytes.HasPrefix(data, []byte("# v3 git bundle")) {
			return nil, nil, nil, fmt.Errorf("v3 bundles (capabilities/filters) are not supported")
		}
		return nil, nil, nil, fmt.Errorf("not a v2 git bundle")
	}
	rest := data[nl+1:]
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, nil, nil, fmt.Errorf("bundle header missing blank line")
		}
		line := string(rest[:nl])
		rest = rest[nl+1:]
		if line == "" {
			return refs, prerequisites, rest, nil
		}
		if strings.HasPrefix(line, "-") {
			fields := strings.Fields(line[1:])
			if len(fields) > 0 {
				prerequisites = append(prerequisites, model.OID(fields[0]))
			}
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 || !IsHexOID(fields[0]) {
			return nil, nil, nil, fmt.Errorf("malformed bundle ref line %q", line)
		}
		refs = append(refs, BundleRef{OID: model.OID(fields[0]), Ref: fields[1]})
	}
}

// writePackIdxV2 renders the idx v2 file for an already-encoded pack. The
// offsets and CRCs come from the pack decoder's observer — the
// authoritative mapping, identical to what go-git's index writer consumes.
func writePackIdxV2(packBytes []byte, algo model.HashAlgo) ([]byte, error) {
	dp, err := storage.DecodePack(packBytes)
	if err != nil {
		return nil, err
	}
	oids := make([]model.OID, 0, len(dp.Offsets))
	for oid := range dp.Offsets {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })

	width := 20
	if algo == model.SHA256 {
		width = 32
	}
	_ = width
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 't', 'O', 'c'})
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], 2)
	buf.Write(v[:])

	// Fanout: cumulative counts by first byte of the binary object name
	// (oids are lowercase hex strings here).
	fan := [256]uint32{}
	for _, oid := range oids {
		b, _ := strconv.ParseUint(string(oid[:2]), 16, 8)
		fan[byte(b)]++
	}
	var run uint32
	for i := 0; i < 256; i++ {
		run += fan[i]
		binary.BigEndian.PutUint32(v[:], run)
		buf.Write(v[:])
	}
	for _, oid := range oids {
		raw, _ := hex.DecodeString(string(oid))
		buf.Write(raw)
	}
	for _, oid := range oids {
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], dp.CRCs[oid])
		buf.Write(c[:])
	}
	var large []uint64
	for _, oid := range oids {
		off := dp.Offsets[oid]
		if off < 0x80000000 {
			binary.BigEndian.PutUint32(v[:], uint32(off))
			buf.Write(v[:])
			continue
		}
		binary.BigEndian.PutUint32(v[:], uint32(0x80000000|len(large)))
		buf.Write(v[:])
		large = append(large, off)
	}
	var big [8]byte
	for _, off := range large {
		binary.BigEndian.PutUint64(big[:], off)
		buf.Write(big[:])
	}
	// The pack-checksum field mirrors the pack's trailing checksum (the
	// same bytes the pack is named by), then the idx closes with its own
	// digest over everything above.
	buf.Write(packBytes[len(packBytes)-algo.New().Size():])
	idxSum := algo.New()
	idxSum.Write(buf.Bytes())
	buf.Write(idxSum.Sum(nil))
	return buf.Bytes(), nil
}

// archiveEntry is one member of the snapshot: a file blob, a symlink, a
// directory marker or a gitlink (submodule pointer, presented as a
// directory like git does).
type archiveEntry struct {
	path  string // relative to the archive root, no leading prefix
	isDir bool
	mode  string // tree-entry mode ("" for synthesized directory markers)
	oid   model.OID
}

// collectArchiveEntries walks the tree depth-first in git's order. With
// elide the walk starts at the sub-tree root (paths relative to it);
// otherwise the sub-tree keeps its path prefix, matching `git archive
// <commit> -- <path>` pathspec behavior.
func (s *Base) CollectArchiveEntries(ctx context.Context, repo string, treeOID model.OID, base string, elide bool, excludes []string) ([]archiveEntry, error) {
	lookup := s.Objects.Lookup(repo)
	excluded := func(path string) bool {
		for _, ex := range excludes {
			if path == ex || strings.HasPrefix(path, ex+"/") {
				return true
			}
		}
		return false
	}
	var out []archiveEntry
	var walk func(tree model.OID, prefix string) error
	walk = func(tree model.OID, prefix string) error {
		t, err := lookup.Tree(tree)
		if err != nil {
			return nil // unreadable subtree: archive what is readable
		}
		for _, e := range t.Entries {
			path := prefix + e.Name
			if excluded(path) {
				continue
			}
			switch {
			case e.IsDir:
				out = append(out, archiveEntry{path: path + "/", isDir: true})
				if err := walk(e.Target, path+"/"); err != nil {
					return err
				}
			case e.Mode == "160000":
				out = append(out, archiveEntry{path: path + "/", isDir: true, mode: e.Mode, oid: e.Target})
			case e.Mode == "120000":
				out = append(out, archiveEntry{path: path, mode: e.Mode, oid: e.Target})
			default:
				out = append(out, archiveEntry{path: path, mode: e.Mode, oid: e.Target})
			}
		}
		return nil
	}
	if err := walk(treeOID, ""); err != nil {
		return nil, err
	}
	if !elide {
		// Pathspec form keeps the sub-tree's own path in member names.
		if base != "" {
			for i := range out {
				out[i].path = base + strings.TrimPrefix(out[i].path, "")
			}
		}
	}
	return out, nil
}

// streamWriter adapts a send callback into an io.Writer.
func StreamWriter(send func([]byte) error) io.Writer { return &funcWriter{send: send} }

type funcWriter struct{ send func([]byte) error }

func (f *funcWriter) Write(p []byte) (int, error) {
	if err := f.send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// ---- tar writer: git archive-tar.c byte layout ----

const tarRecordSize = 512
const TarBlockSize = 10240 // git's blocking factor 20

type tarWriter struct {
	w       io.Writer
	written int
}

func (t *tarWriter) write(p []byte) error {
	n, err := t.w.Write(p)
	t.written += n
	return err
}

// octal renders n as a fixed-width zero-padded octal string with a trailing
// NUL, the shape xsnprintf("%0<width>o") leaves in the header field.
func octal(n int64, width int) []byte {
	s := fmt.Sprintf("%0*o", width, n)
	if len(s) > width { // overflow: git's xsnprintf would abort; clamp
		s = strings.Repeat("7", width)
	}
	return append([]byte(s), 0)
}

func (t *tarWriter) header(name string, mode int64, size int64, mtime int64, typeflag byte, linkname string) error {
	h := make([]byte, tarRecordSize)
	if len(name) > 100 {
		// ustar split: the last '/' that leaves a ≤155 prefix and a ≤100
		// name (get_path_prefix, archive-tar.c). Longer names are not
		// served — git would fall back to a pax path record.
		split := -1
		top := 155
		if len(name)-1 < top {
			top = len(name) - 1
		}
		for i := top; i >= 1 && i >= len(name)-101; i-- {
			if name[i] == '/' {
				split = i
				break
			}
		}
		if split < 0 || len(name)-split-1 > 100 {
			return fmt.Errorf("gitaly: archive path too long for ustar: %s", name)
		}
		copy(h[345:], name[:split])
		copy(h[0:], name[split+1:])
	} else {
		copy(h[0:], name)
	}
	copy(h[100:], octal(mode, 7))
	copy(h[108:], []byte("0000000\x00")) // uid
	copy(h[116:], []byte("0000000\x00")) // gid
	copy(h[124:], octal(size, 11))
	copy(h[136:], octal(mtime, 11))
	for i := 148; i < 156; i++ {
		h[i] = ' '
	}
	h[156] = typeflag
	if linkname != "" {
		copy(h[157:], linkname)
	}
	copy(h[257:], "ustar\x00")
	copy(h[263:], "00")
	copy(h[265:], "root")                // uname
	copy(h[297:], "root")                // gname
	copy(h[329:], []byte("0000000\x00")) // devmajor
	copy(h[337:], []byte("0000000\x00")) // devminor
	var chksum int
	for _, b := range h {
		chksum += int(b)
	}
	copy(h[148:], octal(int64(chksum), 7)) // 7 octal digits + NUL, git's %07o
	return t.write(h)
}

func (t *tarWriter) data(p []byte) error {
	if err := t.write(p); err != nil {
		return err
	}
	if pad := tarRecordSize - len(p)%tarRecordSize; pad != tarRecordSize {
		return t.write(make([]byte, pad))
	}
	return nil
}

// entryTarMode maps a tree-entry mode onto the archived file mode with git's
// tar_umask (002) applied, archive-tar.c write_tar_entry.
func entryTarMode(e archiveEntry) int64 {
	if e.isDir {
		return 0o775 // (mode | 0777) & ~002
	}
	m, _ := parseModeOctal(e.mode)
	switch {
	case e.mode == "120000":
		return 0o777 // symlink: mode |= 0777
	case m&0o111 != 0:
		return 0o775 // executable: (mode | 0777) & ~002
	default:
		return 0o664 // regular: (mode | 0666) & ~002
	}
}

func WriteTarArchive(w io.Writer, entries []archiveEntry, prefix string, when time.Time, commitOID model.OID, repo string, b *Base) error {
	t := &tarWriter{w: w}
	// pax_global_header carries the commit id as a pax comment record, the
	// same "52 comment=<oid>\n" body git emits.
	pax := fmt.Sprintf("52 comment=%s\n", commitOID)
	if err := t.header("pax_global_header", 0o666, int64(len(pax)), when.Unix(), 'g', ""); err != nil {
		return err
	}
	if err := t.data([]byte(pax)); err != nil {
		return err
	}
	lookup := b.Objects.Lookup(repo)
	full := func(path string) string {
		if prefix == "" {
			return path
		}
		return strings.TrimSuffix(prefix, "/") + "/" + path
	}
	if prefix != "" {
		root := strings.TrimSuffix(prefix, "/") + "/"
		if err := t.header(root, entryTarMode(archiveEntry{isDir: true}), 0, when.Unix(), '5', ""); err != nil {
			return err
		}
	}
	for _, e := range entries {
		name := full(e.path)
		mode := entryTarMode(e)
		switch {
		case e.isDir:
			if err := t.header(name, mode, 0, when.Unix(), '5', ""); err != nil {
				return err
			}
		case e.mode == "120000":
			_, raw, err := b.Objects.Raw(context.Background(), repo, e.oid)
			if err != nil {
				continue
			}
			if err := t.header(name, mode, 0, when.Unix(), '2', string(raw)); err != nil {
				return err
			}
		default:
			tree, err := lookup.Blob(e.oid)
			if err != nil {
				continue
			}
			if err := t.header(name, mode, int64(len(tree)), when.Unix(), '0', ""); err != nil {
				return err
			}
			if err := t.data(tree); err != nil {
				return err
			}
		}
	}
	// Two zero blocks end the archive; pad the stream to git's record size.
	if err := t.write(make([]byte, 2*tarRecordSize)); err != nil {
		return err
	}
	if pad := TarBlockSize - t.written%TarBlockSize; pad != TarBlockSize {
		return t.write(make([]byte, pad))
	}
	return nil
}

// ---- zip writer: entry-level git semantics on the standard library ----

func WriteZipArchive(w io.Writer, entries []archiveEntry, prefix string, when time.Time, repo string, b *Base) error {
	zw := zip.NewWriter(w)
	full := func(path string) string {
		if prefix == "" {
			return path
		}
		return strings.TrimSuffix(prefix, "/") + "/" + path
	}
	if prefix != "" {
		root := strings.TrimSuffix(prefix, "/") + "/"
		fh := &zip.FileHeader{Name: root, Modified: when, Method: zip.Store}
		fh.SetMode(0o755 | 0x80000000) // ModeDir
		if _, err := zw.CreateHeader(fh); err != nil {
			return err
		}
	}
	ctx := context.Background()
	for _, e := range entries {
		name := full(e.path)
		switch {
		case e.isDir:
			fh := &zip.FileHeader{Name: name, Modified: when, Method: zip.Store}
			fh.SetMode(0o755 | 0x80000000)
			if _, err := zw.CreateHeader(fh); err != nil {
				return err
			}
		case e.mode == "120000":
			_, raw, err := b.Objects.Raw(ctx, repo, e.oid)
			if err != nil {
				continue
			}
			fh := &zip.FileHeader{Name: name, Modified: when, Method: zip.Deflate}
			fh.SetMode(0o777 | 0x80000000 | 0xa00000) // ModeSymlink
			fw, err := zw.CreateHeader(fh)
			if err != nil {
				return err
			}
			if _, err := fw.Write(raw); err != nil {
				return err
			}
		default:
			_, data, err := b.Objects.Raw(ctx, repo, e.oid)
			if err != nil {
				continue
			}
			fh := &zip.FileHeader{Name: name, Modified: when, Method: zip.Deflate}
			m, _ := parseModeOctal(e.mode)
			if m&0o111 != 0 {
				fh.SetMode(0o755)
			} else {
				fh.SetMode(0o644)
			}
			fw, err := zw.CreateHeader(fh)
			if err != nil {
				return err
			}
			if _, err := fw.Write(data); err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// entryAtPath resolves a tree entry by walking path segments from tree.
// An empty path yields nil (the tree itself).
func (s *Base) EntryAtPath(ctx context.Context, repo string, tree model.OID, path string) (*model.TreeEntry, error) {
	segs := SplitPath(path)
	if len(segs) == 0 {
		return nil, nil
	}
	lookup := s.Objects.Lookup(repo)
	cur := tree
	var entry *model.TreeEntry
	for _, seg := range segs {
		t, err := lookup.Tree(cur)
		if err != nil {
			return nil, err
		}
		entry = nil
		for i := range t.Entries {
			if t.Entries[i].Name == seg {
				entry = &t.Entries[i]
				break
			}
		}
		if entry == nil {
			return nil, nil
		}
		if entry.IsDir {
			cur = entry.Target
		}
	}
	return entry, nil
}

// pathTouched reports whether the path's entry differs between a commit and
// its first parent (root commits touch everything they contain).
func (s *Base) PathTouched(ctx context.Context, repo string, rec *CommitRecord, path string) bool {
	entry, _ := s.EntryAtPath(ctx, repo, rec.Tree, path)
	if len(rec.Parents) == 0 {
		return entry != nil
	}
	pr, err := s.CommitRecord(ctx, repo, rec.Parents[0])
	if err != nil {
		return entry != nil
	}
	parentEntry, _ := s.EntryAtPath(ctx, repo, pr.Tree, path)
	if entry == nil || parentEntry == nil {
		return entry != nil || parentEntry != nil
	}
	return entry.Target != parentEntry.Target || entry.Mode != parentEntry.Mode
}

// SortedOIDs renders a deterministic order over an OID set.
func SortedOIDs(set map[model.OID]bool) []model.OID {
	out := make([]model.OID, 0, len(set))
	for oid := range set {
		out = append(out, oid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// withDetail attaches protobuf error details to a status error.
func WithDetail(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
	st := status.New(code, msg)
	st, err := st.WithDetails(details...)
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

// containsLFSPointer reports whether any blob in the tree (recursively) is
// an LFS pointer — the GetArchive gate for include_lfs_blobs: the flag is
// a no-op for pointer-free trees (what GitLab's unconditional default
// sends) and would alter bytes otherwise.
func (s *Base) ContainsLFSPointer(ctx context.Context, repo string, treeOID model.OID) (bool, error) {
	lookup := s.Objects.Lookup(repo)
	var walk func(model.OID) (bool, error)
	walk = func(oid model.OID) (bool, error) {
		t, err := lookup.Tree(oid)
		if err != nil {
			return false, status.Errorf(codes.NotFound, "gitaly: tree %s not found", oid)
		}
		for _, e := range t.Entries {
			if e.IsDir {
				hit, err := walk(e.Target)
				if err != nil || hit {
					return hit, err
				}
				continue
			}
			if e.Mode == "160000" {
				continue // gitlink, not a blob
			}
			if _, raw, err := s.Objects.Raw(ctx, repo, e.Target); err == nil &&
				len(raw) <= LFSPointerMaxSize {
				if _, _, ok := ParseLFSPointer(raw); ok {
					return true, nil
				}
			}
		}
		return false, nil
	}
	return walk(treeOID)
}

const LFSPointerMaxSize = 200

// parseLFSPointer mirrors gitaly's git.IsLFSPointer: spec-version prefix plus
// multiline oid/size lines; fileOid is the bare hex digest (no "sha256:").
func ParseLFSPointer(b []byte) (fileOid []byte, fileSize int64, ok bool) {
	if !bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec")) {
		return nil, 0, false
	}
	oid := lfsOIDRe.FindSubmatch(b)
	if len(oid) != 2 {
		return nil, 0, false
	}
	sz := lfsSizeRe.FindSubmatch(b)
	if len(sz) != 2 {
		return nil, 0, false
	}
	n, err := strconv.ParseInt(string(sz[1]), 10, 64)
	if err != nil {
		return nil, 0, false
	}
	out := make([]byte, len(oid[1]))
	copy(out, oid[1])
	return out, n, true
}

var (
	lfsOIDRe  = regexp.MustCompile(`(?m)^oid sha256:([0-9a-f]{64})$`)
	lfsSizeRe = regexp.MustCompile(`(?m)^size ([0-9]+)$`)
)

// globMatch implements '*'/'?'/'**' with '*' crossing '/' (FNM_PATHNAME off).
func GlobMatch(pattern, name string) bool {
	px, nx := 0, 0
	starPx, starNx := -1, -1
	for nx < len(name) || px < len(pattern) {
		if px < len(pattern) {
			switch pattern[px] {
			case '*':
				starPx, starNx = px, nx
				px++
				continue
			case '?':
				if nx < len(name) {
					px++
					nx++
					continue
				}
			default:
				if nx < len(name) && name[nx] == pattern[px] {
					px++
					nx++
					continue
				}
			}
		}
		if starPx >= 0 && starNx < len(name) {
			starNx++
			px, nx = starPx+1, starNx
			continue
		}
		return false
	}
	return true
}

func Itoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
