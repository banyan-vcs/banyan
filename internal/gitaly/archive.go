package gitaly

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
)

// GetArchive streams a tree snapshot as tar, tar.gz or zip, byte-compatible
// with `git archive` for the tar flavors (pax_global_header comment, root
// uname/gname, tar_umask 002 modes, 10240-byte record padding). bzip2 needs
// a compressor outside the standard library and stays Unimplemented; LFS
// smudging has no banyan-side storage plane and refuses explicitly.
func (rs *repositoryService) GetArchive(req *gitalypb.GetArchiveRequest, stream gitalypb.RepositoryService_GetArchiveServer) error {
	ctx := stream.Context()
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	switch req.GetFormat() {
	case gitalypb.GetArchiveRequest_TAR, gitalypb.GetArchiveRequest_TAR_GZ, gitalypb.GetArchiveRequest_ZIP:
	case gitalypb.GetArchiveRequest_TAR_BZ2:
		// bzip2 compression has no standard-library writer; refusing
		// beats a wrong-format answer (see docs/19 §6).
		return status.Error(codes.Unimplemented, "gitaly: tar.bz2 archives need a bzip2 compressor")
	default:
		return status.Error(codes.InvalidArgument, "gitaly: invalid format")
	}
	if req.GetIncludeLfsBlobs() {
		return status.Error(codes.Unimplemented, "gitaly: include_lfs_blobs requires the LFS smudge plane")
	}

	commitOID, err := rs.s.resolveRevision(ctx, name, req.GetCommitId())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitaly: invalid commitId: %s", req.GetCommitId())
	}
	rec, err := rs.s.commitRecord(ctx, name, commitOID)
	if err != nil {
		return status.Error(codes.InvalidArgument, "gitaly: invalid commitId")
	}

	root := strings.Trim(string(req.GetPath()), "/")
	treeOID := rec.Tree
	base := ""
	if root != "" {
		entry, aerr := rs.s.entryAtPath(ctx, name, rec.Tree, root)
		if aerr != nil || entry == nil || !entry.IsDir {
			return withDetail(codes.FailedPrecondition, "gitaly: path doesn't exist",
				&gitalypb.GetArchiveError{Error: &gitalypb.GetArchiveError_ArchivePathNotFound{
					ArchivePathNotFound: &gitalypb.PathNotFoundError{Path: req.GetPath()}}})
		}
		treeOID = entry.Target
		base = root + "/"
	}

	excludes := make([]string, 0, len(req.GetExclude()))
	for i, ex := range req.GetExclude() {
		clean := strings.Trim(string(ex), "/")
		if clean == "" {
			continue
		}
		if !rs.pathUnderCommit(ctx, name, rec.Tree, clean) {
			return withDetail(codes.FailedPrecondition,
				fmt.Sprintf("gitaly: exclude[%d] doesn't exist", i),
				&gitalypb.GetArchiveError{Error: &gitalypb.GetArchiveError_ExcludePathNotFound{
					ExcludePathNotFound: &gitalypb.GetArchiveError_ExcludePathNotFoundError{
						Index: uint32(i), Path: ex}}})
		}
		excludes = append(excludes, clean)
	}

	entries, err := rs.collectArchiveEntries(ctx, name, treeOID, base, req.GetElidePath(), excludes)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if len(entries) == 0 && root == "" {
		return withDetail(codes.FailedPrecondition, "gitaly: path doesn't exist",
			&gitalypb.GetArchiveError{Error: &gitalypb.GetArchiveError_EmptyRootTree{
				EmptyRootTree: &gitalypb.GetArchiveError_EmptyRootTreeError{}}})
	}

	when := time.Unix(rec.When, 0).UTC()
	prefix := req.GetPrefix()
	var out io.Writer = streamWriter(func(p []byte) error {
		return stream.Send(&gitalypb.GetArchiveResponse{Data: p})
	})
	switch req.GetFormat() {
	case gitalypb.GetArchiveRequest_ZIP:
		return writeZipArchive(out, entries, prefix, when, name, rs)
	case gitalypb.GetArchiveRequest_TAR_GZ:
		zw := gzip.NewWriter(out)
		zw.Header.OS = 3 // gzip -n matches git's compressor byte for byte
		if err := writeTarArchive(zw, entries, prefix, when, commitOID, name, rs); err != nil {
			return err
		}
		return zw.Close()
	default:
		return writeTarArchive(out, entries, prefix, when, commitOID, name, rs)
	}
}

// pathUnderCommit reports whether a cleaned path resolves inside the tree.
func (rs *repositoryService) pathUnderCommit(ctx context.Context, repo string, tree model.OID, path string) bool {
	entry, err := rs.s.entryAtPath(ctx, repo, tree, path)
	return err == nil && entry != nil
}

// archiveEntry is one member of the snapshot: a file blob, a symlink, a
// directory marker or a gitlink (submodule pointer, presented as a
// directory like git does).
type archiveEntry struct {
	path    string // relative to the archive root, no leading prefix
	isDir   bool
	mode    string // tree-entry mode ("" for synthesized directory markers)
	oid     model.OID
}

// collectArchiveEntries walks the tree depth-first in git's order. With
// elide the walk starts at the sub-tree root (paths relative to it);
// otherwise the sub-tree keeps its path prefix, matching `git archive
// <commit> -- <path>` pathspec behavior.
func (rs *repositoryService) collectArchiveEntries(ctx context.Context, repo string, treeOID model.OID, base string, elide bool, excludes []string) ([]archiveEntry, error) {
	lookup := rs.s.Objects.Lookup(repo)
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
func streamWriter(send func([]byte) error) io.Writer { return &funcWriter{send: send} }

type funcWriter struct{ send func([]byte) error }

func (f *funcWriter) Write(p []byte) (int, error) {
	if err := f.send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// ---- tar writer: git archive-tar.c byte layout ----

const tarRecordSize = 512
const tarBlockSize = 10240 // git's blocking factor 20

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
	copy(h[265:], "root") // uname
	copy(h[297:], "root") // gname
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

func writeTarArchive(w io.Writer, entries []archiveEntry, prefix string, when time.Time, commitOID model.OID, repo string, rs *repositoryService) error {
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
	lookup := rs.s.Objects.Lookup(repo)
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
			_, raw, err := rs.s.Objects.Raw(context.Background(), repo, e.oid)
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
	if pad := tarBlockSize - t.written%tarBlockSize; pad != tarBlockSize {
		return t.write(make([]byte, pad))
	}
	return nil
}

// ---- zip writer: entry-level git semantics on the standard library ----

func writeZipArchive(w io.Writer, entries []archiveEntry, prefix string, when time.Time, repo string, rs *repositoryService) error {
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
			_, raw, err := rs.s.Objects.Raw(ctx, repo, e.oid)
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
			_, data, err := rs.s.Objects.Raw(ctx, repo, e.oid)
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

