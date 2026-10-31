package gitaly

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Snapshots, replication and the bundle-URI generator (docs/19).
//
// GetSnapshot answers a bare-repository TAR — HEAD, packed-refs and a
// pack + idx pair — the same layout git itself works with, so the snapshot
// is consumable by plain git (unpacked, cloned from) and by
// CreateRepositoryFromSnapshot on the way back in. ReplicateRepository is
// the object-plane mirror of the same state. GenerateBundleURI writes a
// fresh bundle into the backup sink's bundle-uri area (upstream's sink key
// scheme); the smart-HTTP advertisement stays config-driven.

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

// GetSnapshot streams the repository state as a bare-repo TAR.
func (rs *repositoryService) GetSnapshot(req *gitalypb.GetSnapshotRequest, stream gitalypb.RepositoryService_GetSnapshotServer) error {
	ctx := stream.Context()
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return err
	}
	return rs.writeSnapshot(ctx, name, &chunkWriter{send: func(p []byte) error {
		return stream.Send(&gitalypb.GetSnapshotResponse{Data: p})
	}})
}

// writeSnapshot renders the bare-repo TAR (HEAD, packed-refs, pack + idx)
// to w — the shared core of GetSnapshot and the partition backup.
func (rs *repositoryService) writeSnapshot(ctx context.Context, name string, w io.Writer) error {
	refs, err := rs.s.listRefs(ctx, name)
	if err != nil {
		return err
	}
	def, _ := rs.s.defaultBranch(ctx, name)

	// One full-closure pack (heads + tags + aux refs, HEAD included).
	brefs := make([]bundleRef, 0, len(refs))
	for _, r := range refs {
		brefs = append(brefs, bundleRef{ref: r.Name, oid: r.Head})
	}
	order, err := rs.s.bundleClosure(ctx, name, brefs)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	var pack bytes.Buffer
	i := 0
	if err := storage.StreamEncodePack(&pack, rs.s.Algo, len(order), func(int) (model.ObjectType, []byte, error) {
		oid := order[i]
		i++
		return rs.s.Objects.Raw(ctx, name, oid)
	}); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	idx, err := writePackIdxV2(pack.Bytes(), rs.s.Algo)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	// A pack is named by its trailing checksum — the 20 bytes git's index
	// also carries as the pack checksum — not by a whole-file digest.
	packSum := pack.Bytes()[len(pack.Bytes())-rs.s.Algo.New().Size():]
	packName := "pack-" + hex.EncodeToString(packSum)

	// packed-refs: "<oid> <ref>" sorted; annotated tags carry their peeled
	// commit as a "^<oid>" row directly beneath.
	type refRow struct{ line string; peel string }
	rows := make([]refRow, 0, len(refs))
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		row := refRow{line: fmt.Sprintf("%s %s\n", r.Head, r.Name)}
		if typ, _, terr := rs.s.Objects.Raw(ctx, name, r.Head); terr == nil && typ == model.TagType {
			if peeled, perr := rs.s.peelToCommit(ctx, name, r.Head); perr == nil {
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

func sha1Sum(b []byte) []byte {
	s := sha1.Sum(b)
	return s[:]
}

type chunkWriter struct{ send func([]byte) error }

func (c *chunkWriter) Write(p []byte) (int, error) {
	if err := c.send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// snapshotFile is one extracted member of a snapshot TAR.
type snapshotFile struct {
	name string
	data []byte
}

// extractSnapshotTar pulls the members of a snapshot TAR into memory (repo
// snapshots are pack-shaped; the few control files ride along).
func extractSnapshotTar(data []byte) (map[string][]byte, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[hdr.Name] = body
	}
	return out, nil
}

// CreateRepositoryFromSnapshot downloads a snapshot TAR over HTTP and
// seeds the new repository from it. A failed ingest rolls the repository
// back (the CreateRepositoryFrom* family contract).
func (rs *repositoryService) CreateRepositoryFromSnapshot(ctx context.Context, req *gitalypb.CreateRepositoryFromSnapshotRequest) (*gitalypb.CreateRepositoryFromSnapshotResponse, error) {
	url := req.GetHttpUrl()
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, status.Error(codes.InvalidArgument, "gitaly: snapshot URL must be http(s)")
	}
	if err := rs.createRepositoryChecked(ctx, req.GetRepository()); err != nil {
		return nil, err
	}
	name := req.GetRepository().GetRelativePath()
	rollback := func() {
		_ = rs.s.Meta.DeleteRepo(ctx, name)
		_ = rs.s.Objects.DeleteRepo(ctx, name)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		rollback()
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if auth := req.GetHttpAuth(); auth != "" {
		httpReq.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		rollback()
		return nil, status.Errorf(codes.Unavailable, "gitaly: fetch snapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rollback()
		return nil, status.Errorf(codes.Unavailable, "gitaly: snapshot fetch: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWireMsg))
	if err != nil {
		rollback()
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	files, err := extractSnapshotTar(raw)
	if err != nil {
		rollback()
		return nil, status.Error(codes.InvalidArgument, "gitaly: snapshot tar: "+err.Error())
	}

	// Ingest every pack member.
	for name_, data := range files {
		if !strings.HasPrefix(name_, "objects/pack/") || !strings.HasSuffix(name_, ".pack") {
			continue
		}
		dir, err := os.MkdirTemp("", "banyan-snapshot-*")
		if err != nil {
			rollback()
			return nil, status.Error(codes.Internal, err.Error())
		}
		packPath := filepath.Join(dir, "snapshot.pack")
		if err := os.WriteFile(packPath, data, 0o644); err != nil {
			os.RemoveAll(dir)
			rollback()
			return nil, status.Error(codes.Internal, err.Error())
		}
		_, err = rs.s.Objects.IngestPackFile(ctx, req.GetRepository().GetRelativePath(), packPath, int64(len(data)))
		os.RemoveAll(dir)
		if err != nil {
			rollback()
			return nil, status.Errorf(codes.InvalidArgument, "gitaly: snapshot pack rejected: %v", err)
		}
	}

	// Refs from packed-refs; HEAD picks the default branch back up.
	if pr, ok := files["packed-refs"]; ok {
		for _, line := range strings.Split(string(pr), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || strings.HasSuffix(fields[1], "^{}") {
				continue
			}
			if err := rs.s.Svc.UpdateRef(ctx, service.RefUpdate{
				Repo: name, Name: fields[1], New: model.OID(fields[0])}); err != nil {
				rollback()
				return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
			}
		}
	}
	if head, ok := files["HEAD"]; ok {
		if target := strings.TrimSpace(strings.TrimPrefix(string(head), "ref: ")); strings.HasPrefix(target, "refs/heads/") {
			_ = rs.s.Meta.SetDefaultBranch(ctx, name, strings.TrimPrefix(target, "refs/heads/"))
		}
	}
	return &gitalypb.CreateRepositoryFromSnapshotResponse{}, nil
}

// ReplicateRepository mirrors one repository's complete state onto another
// (the praefect replication surface, served by the object plane): every
// ref's closure, the refs themselves, HEAD's default branch and the
// custom-hooks tarball.
func (rs *repositoryService) ReplicateRepository(ctx context.Context, req *gitalypb.ReplicateRepositoryRequest) (*gitalypb.ReplicateRepositoryResponse, error) {
	source, err := rs.s.existingRepo(ctx, req.GetSource())
	if err != nil {
		return nil, err
	}
	target := req.GetRepository().GetRelativePath()

	// A replication replaces the destination wholesale.
	if _, terr := rs.s.existingRepo(ctx, req.GetRepository()); terr == nil {
		if derr := rs.s.Meta.DeleteRepo(ctx, target); derr != nil {
			return nil, status.Error(codes.Internal, derr.Error())
		}
		if derr := rs.s.Objects.DeleteRepo(ctx, target); derr != nil {
			return nil, status.Error(codes.Internal, derr.Error())
		}
	}
	if err := rs.s.Meta.CreateRepo(ctx, target, rs.s.Algo); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	refs, err := rs.s.listRefs(ctx, source)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		tip := r.Head
		if typ, _, terr := rs.s.Objects.Raw(ctx, source, tip); terr == nil && typ == model.TagType {
			if peeled, perr := rs.s.peelToCommit(ctx, source, tip); perr == nil {
				// The tag object itself plus its commit closure.
				typ2, raw, _ := rs.s.Objects.Raw(ctx, source, tip)
				if typ2 != "" {
					_ = rs.s.Svc.PutObjects(ctx, target, []service.PutObject{{
						OID: tip, Type: string(typ2), Data: raw}})
				}
				tip = peeled
			}
		}
		if err := rs.copyClosure(ctx, source, target, tip); err != nil {
			return nil, status.Errorf(codes.Internal, "gitaly: %v", err)
		}
		if err := rs.s.Svc.UpdateRef(ctx, service.RefUpdate{
			Repo: target, Name: r.Name, New: r.Head}); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	if def, derr := rs.s.defaultBranch(ctx, source); derr == nil {
		_ = rs.s.Meta.SetDefaultBranch(ctx, target, def)
	}
	if hooks, ok, herr := rs.s.Meta.GetRepoBlob(ctx, source, customHooksKey); herr == nil && ok {
		_ = rs.s.Meta.SetRepoBlob(ctx, target, customHooksKey, hooks)
	}
	return &gitalypb.ReplicateRepositoryResponse{}, nil
}

// GenerateBundleURI writes a fresh full bundle for the repository into the
// backup sink's bundle-uri area (upstream's sink key scheme:
// <storage>/<relative_path>/uri/default.bundle — each run overwrites).
// The response is empty — upstream's callers learn the URI from the
// sink/CDN, not the RPC.
func (rs *repositoryService) GenerateBundleURI(ctx context.Context, req *gitalypb.GenerateBundleURIRequest) (*gitalypb.GenerateBundleURIResponse, error) {
	sink, err := rs.s.backupSinkOf()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "server-side backups are not configured")
	}
	name, err := rs.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	bundle, err := rs.s.fullBundle(ctx, name, mustRefs(ctx, rs, name))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gitaly: %v", err)
	}
	rel := fmt.Sprintf("%s/%s/uri/default.bundle", req.GetRepository().GetStorageName(),
		req.GetRepository().GetRelativePath())
	if err := sink.write(rel, bundle); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &gitalypb.GenerateBundleURIResponse{}, nil
}

func mustRefs(ctx context.Context, rs *repositoryService, name string) []listedRef {
	refs, err := rs.s.listRefs(ctx, name)
	if err != nil {
		return nil
	}
	return refs
}
