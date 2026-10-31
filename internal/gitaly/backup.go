package gitaly

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server-side backups (docs/19). Upstream gitaly streams bundles, refs and
// custom-hook archives into a go-cloud blob sink and tracks runs through a
// backup-id marker registry. banyan mirrors that layout on a local sink
// directory (the single-node equivalent — no external object storage to
// talk to), key-compatible with the upstream scheme:
//
//	<prefix>/<storage>/<relative_path>/<backup_id>/NNN.bundle|.refs|.custom_hooks.tar
//	<prefix>/manifests/<storage>/<relative_path>/<backup_id>.toml
//	<prefix>/backup_ids/<backup_id>
//
// An unconfigured sink answers FailedPrecondition exactly like upstream.

const backupIDPrefix = "backup_ids"

// backupStep is one increment of a backup: a bundle, the refs it captured
// and the custom-hooks archive current at that point.
type backupStep struct {
	BundlePath      string
	RefPath         string
	PreviousRefPath string
	CustomHooksPath string
}

// backupManifest is the per-repo, per-run record (upstream's TOML shape).
type backupManifest struct {
	ID            string
	Empty         bool
	NonExistent   bool
	ObjectFormat  string
	HeadReference string
	Steps         []backupStep
}

// backupSink is the local-directory sink rooted at Options.BackupDir.
type backupSink struct{ root string }

func (s *backupSink) path(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

func (s *backupSink) write(rel string, data []byte) error {
	p := s.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (s *backupSink) read(rel string) ([]byte, error) { return os.ReadFile(s.path(rel)) }

func (s *backupSink) exists(rel string) bool {
	_, err := os.Stat(s.path(rel))
	return err == nil
}

// backupSinkOf answers the configured sink, or the upstream-shaped
// precondition error when server-side backups are off.
func (s *Server) backupSinkOf() (*backupSink, error) {
	if s.Opts.BackupDir == "" {
		return nil, status.Error(codes.FailedPrecondition, "server-side backups are not configured")
	}
	return &backupSink{root: s.Opts.BackupDir}, nil
}

// manifestRel is the manifest key for one repo + run.
func manifestRel(prefix, storage, relPath, backupID string) string {
	return fmt.Sprintf("%s/manifests/%s/%s/%s.toml", prefix, storage, relPath, backupID)
}

func stepRel(prefix, storage, relPath, backupID string, n int, ext string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%03d.%s", prefix, storage, relPath, backupID, n, ext)
}

// renderManifest writes the minimal TOML subset upstream emits.
func renderManifest(m *backupManifest) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "empty = %t\n", m.Empty)
	fmt.Fprintf(&b, "non_existent = %t\n", m.NonExistent)
	if m.ObjectFormat != "" {
		fmt.Fprintf(&b, "object_format = %q\n", m.ObjectFormat)
	}
	if m.HeadReference != "" {
		fmt.Fprintf(&b, "head_reference = %q\n", m.HeadReference)
	}
	for _, s := range m.Steps {
		b.WriteString("[[steps]]\n")
		if s.BundlePath != "" {
			fmt.Fprintf(&b, "bundle_path = %q\n", s.BundlePath)
		}
		if s.RefPath != "" {
			fmt.Fprintf(&b, "ref_path = %q\n", s.RefPath)
		}
		if s.PreviousRefPath != "" {
			fmt.Fprintf(&b, "previous_ref_path = %q\n", s.PreviousRefPath)
		}
		if s.CustomHooksPath != "" {
			fmt.Fprintf(&b, "custom_hooks_path = %q\n", s.CustomHooksPath)
		}
	}
	return []byte(b.String())
}

// parseManifest reads the TOML subset renderManifest produces.
func parseManifest(data []byte, id string) *backupManifest {
	m := &backupManifest{ID: id}
	var step *backupStep
	assign := func(key, val string) {
		v := strings.Trim(strings.TrimSpace(val), `"`)
		switch key {
		case "bundle_path":
			step.BundlePath = v
		case "ref_path":
			step.RefPath = v
		case "previous_ref_path":
			step.PreviousRefPath = v
		case "custom_hooks_path":
			step.CustomHooksPath = v
		case "head_reference":
			m.HeadReference = v
		}
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "[[steps]]":
			m.Steps = append(m.Steps, backupStep{})
			step = &m.Steps[len(m.Steps)-1]
		case strings.HasPrefix(line, "empty "):
			m.Empty = strings.HasSuffix(line, "true")
		case strings.HasPrefix(line, "non_existent "):
			m.NonExistent = strings.HasSuffix(line, "true")
		case strings.Contains(line, " = "):
			key, val, _ := strings.Cut(line, " = ")
			if step != nil || key == "head_reference" {
				assign(key, val)
			}
		}
	}
	return m
}

// backupRefRow is one row of a refs file (show-ref shape).
type backupRefRow struct {
	OID model.OID
	Ref string
}

// backupRefs renders the refs file: "<oid> <ref>" lines, HEAD excluded (it
// rides the manifest's head_reference).
func backupRefsFile(refs []listedRef) []byte {
	var b strings.Builder
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		fmt.Fprintf(&b, "%s %s\n", r.Head, r.Name)
	}
	return []byte(b.String())
}

func parseBackupRefs(data []byte) []backupRefRow {
	var out []backupRefRow
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		out = append(out, backupRefRow{OID: model.OID(fields[0]), Ref: fields[1]})
	}
	return out
}

// createBackup writes one backup run for the repository into the sink.
func (s *Server) createBackup(ctx context.Context, sink *backupSink,
	repo *gitalypb.Repository, vanity *gitalypb.Repository,
	backupID, latestBackupID, pathPrefix string, incremental bool) error {

	if backupID == "" {
		return status.Error(codes.InvalidArgument, "empty BackupId")
	}
	storage, relPath := repo.GetStorageName(), repo.GetRelativePath()
	if vanity != nil {
		storage, relPath = vanity.GetStorageName(), vanity.GetRelativePath()
	}
	prefix := pathPrefix
	if prefix == "" {
		prefix = "."
	}

	name, err := s.existingRepo(ctx, repo)
	m := &backupManifest{ID: backupID, ObjectFormat: "sha1"}
	if err != nil {
		// A non-existent repository still gets a manifest: restoring a
		// deleted repo must know there is nothing to bring back.
		m.NonExistent = true
		m.Empty = true
		return sink.write(manifestRel(prefix, storage, relPath, backupID), renderManifest(m))
	}

	refs, err := s.listRefs(ctx, name)
	if err != nil {
		return err
	}
	def, _ := s.defaultBranch(ctx, name)
	m.HeadReference = "refs/heads/" + def

	n, previousRefs := 1, ""
	if incremental {
		if prev, perr := latestManifest(sink, prefix, storage, relPath, latestBackupID); perr == nil &&
			!prev.NonExistent && len(prev.Steps) > 0 && sink.exists(prev.Steps[len(prev.Steps)-1].RefPath) {
			// The new manifest carries the full step stack: earlier steps
			// still point at their original sink keys, so a restore of this
			// run replays the whole chain in order.
			m.Steps = append(m.Steps, prev.Steps...)
			n = len(prev.Steps) + 1
			previousRefs = prev.Steps[len(prev.Steps)-1].RefPath
		}
	}
	step := backupStep{
		BundlePath:      stepRel(prefix, storage, relPath, backupID, n, "bundle"),
		RefPath:         stepRel(prefix, storage, relPath, backupID, n, "refs"),
		PreviousRefPath: previousRefs,
		CustomHooksPath: stepRel(prefix, storage, relPath, backupID, n, "custom_hooks.tar"),
	}
	m.Steps = append(m.Steps, step)

	if err := sink.write(step.RefPath, backupRefsFile(refs)); err != nil {
		return status.Errorf(codes.Internal, "backup: %v", err)
	}
	m.Empty = !hasRefRows(refs)
	if !m.Empty {
		var bundle []byte
		if previousRefs != "" {
			prevData, rerr := sink.read(previousRefs)
			if rerr != nil {
				return status.Errorf(codes.Internal, "backup: %v", rerr)
			}
			bundle, err = s.incrementalBundle(ctx, name, refs, parseBackupRefs(prevData))
		} else {
			bundle, err = s.fullBundle(ctx, name, refs)
		}
		if err != nil {
			return status.Errorf(codes.Internal, "backup: %v", err)
		}
		if err := sink.write(step.BundlePath, bundle); err != nil {
			return status.Errorf(codes.Internal, "backup: %v", err)
		}
	}

	// Custom hooks ride the repo-blob plane (the custom-hooks tarball).
	if hooks, ok, herr := s.Meta.GetRepoBlob(ctx, name, customHooksKey); herr == nil && ok {
		if err := sink.write(step.CustomHooksPath, hooks); err != nil {
			return status.Errorf(codes.Internal, "backup: %v", err)
		}
	}
	return sink.write(manifestRel(prefix, storage, relPath, backupID), renderManifest(m))
}

func hasRefRows(refs []listedRef) bool {
	for _, r := range refs {
		if r.Name != "HEAD" {
			return true
		}
	}
	return false
}

// fullBundle is the complete closure bundle (CreateBundle's bytes).
func (s *Server) fullBundle(ctx context.Context, repo string, refs []listedRef) ([]byte, error) {
	rs := &repositoryService{s: s}
	brefs := make([]bundleRef, 0, len(refs))
	for _, r := range refs {
		brefs = append(brefs, bundleRef{ref: r.Name, oid: r.Head})
	}
	var buf strings.Builder
	if err := rs.writeBundle(ctx, repo, brefs, &buf); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// incrementalBundle carries only objects the previous refs did not cover;
// the boundary commits land as bundle prerequisites (the form
// ingestBundle verifies on the way back in).
func (s *Server) incrementalBundle(ctx context.Context, repo string,
	refs []listedRef, prev []backupRefRow) ([]byte, error) {

	toBundleRefs := func(rows []backupRefRow) []bundleRef {
		out := make([]bundleRef, 0, len(rows))
		for _, r := range rows {
			out = append(out, bundleRef{ref: r.Ref, oid: r.OID})
		}
		return out
	}
	curRefs := toBundleRefs(parseBackupRefs(backupRefsFile(refs)))

	prevOrder, err := s.bundleClosure(ctx, repo, toBundleRefs(prev))
	if err != nil {
		return nil, err
	}
	prevSet := make(map[model.OID]bool, len(prevOrder))
	for _, oid := range prevOrder {
		prevSet[oid] = true
	}
	curOrder, err := s.bundleClosure(ctx, repo, curRefs)
	if err != nil {
		return nil, err
	}

	// Included = reachable now, absent before (content addressing makes the
	// set difference object-precise). Prerequisites = already-covered
	// commits referenced by included commits — the ingest boundary.
	included := make([]model.OID, 0, len(curOrder))
	includedSet := map[model.OID]bool{}
	prereqs := map[model.OID]bool{}
	for _, oid := range curOrder {
		if prevSet[oid] {
			continue
		}
		included = append(included, oid)
		includedSet[oid] = true
	}
	for _, oid := range included {
		typ, raw, err := s.Objects.Raw(ctx, repo, oid)
		if err != nil || typ != model.CommitType {
			continue
		}
		c, err := model.DecodeCommit(raw)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if !includedSet[p] {
				prereqs[p] = true
			}
		}
	}

	var b strings.Builder
	b.WriteString(bundleHeaderV2 + "\n")
	for _, pre := range sortedOIDs(prereqs) {
		fmt.Fprintf(&b, "-%s\n", pre)
	}
	byName := make(map[string]model.OID, len(curRefs))
	names := make([]string, 0, len(curRefs))
	for _, r := range curRefs {
		byName[r.ref] = r.oid
		names = append(names, r.ref)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%s %s\n", byName[n], n)
	}
	b.WriteString("\n")

	i := 0
	err = storage.StreamEncodePack(&b, s.Algo, len(included), func(int) (model.ObjectType, []byte, error) {
		oid := included[i]
		i++
		typ, raw, err := s.Objects.Raw(ctx, repo, oid)
		return typ, raw, err
	})
	if err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func sortedOIDs(set map[model.OID]bool) []model.OID {
	out := make([]model.OID, 0, len(set))
	for oid := range set {
		out = append(out, oid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// latestManifest finds the repo's manifest: the named backup when given,
// else the modification-time-newest manifest file.
func latestManifest(sink *backupSink, prefix, storage, relPath, backupID string) (*backupManifest, error) {
	if backupID != "" {
		data, err := sink.read(manifestRel(prefix, storage, relPath, backupID))
		if err != nil {
			return nil, err
		}
		return parseManifest(data, backupID), nil
	}
	dir := sink.path(fmt.Sprintf("%s/manifests/%s/%s", prefix, storage, relPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	newest, mod := "", time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if newest == "" || info.ModTime().After(mod) {
			newest, mod = strings.TrimSuffix(e.Name(), ".toml"), info.ModTime()
		}
	}
	if newest == "" {
		return nil, os.ErrNotExist
	}
	data, err := sink.read(manifestRel(prefix, storage, relPath, newest))
	if err != nil {
		return nil, err
	}
	return parseManifest(data, newest), nil
}

// restoreBackup rebuilds a repository from a backup run in the sink.
func (s *Server) restoreBackup(ctx context.Context, sink *backupSink,
	repo *gitalypb.Repository, vanity *gitalypb.Repository,
	backupID string, alwaysCreate, useLatest bool, pathPrefix string) error {

	storage, relPath := repo.GetStorageName(), repo.GetRelativePath()
	if vanity != nil {
		storage, relPath = vanity.GetStorageName(), vanity.GetRelativePath()
	}
	prefix := pathPrefix
	if prefix == "" {
		prefix = "."
	}

	m, err := latestManifest(sink, prefix, storage, relPath, backupID)
	if err != nil {
		if backupID == "" || useLatest {
			return status.Errorf(codes.NotFound, "gitaly: no backup manifests for %s/%s", storage, relPath)
		}
		return status.Errorf(codes.NotFound, "gitaly: backup %q not found for %s/%s", backupID, storage, relPath)
	}

	_, existErr := s.existingRepo(ctx, repo)
	exists := existErr == nil
	if exists && alwaysCreate {
		return status.Error(codes.FailedPrecondition, "gitaly: repository already exists")
	}

	if m.NonExistent {
		return nil // nothing to restore
	}
	name := repo.GetRelativePath()
	if !exists {
		if err := s.Meta.CreateRepo(ctx, name, s.Algo); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
	setHead := func() error {
		if m.HeadReference == "" {
			return nil
		}
		return s.Meta.SetDefaultBranch(ctx, name, strings.TrimPrefix(m.HeadReference, "refs/heads/"))
	}
	if m.Empty || len(m.Steps) == 0 {
		return setHead()
	}

	rs := &repositoryService{s: s}
	for _, step := range m.Steps {
		if !sink.exists(step.BundlePath) {
			continue // an increment with nothing new
		}
		data, rerr := sink.read(step.BundlePath)
		if rerr != nil {
			return status.Errorf(codes.Internal, "restore: %v", rerr)
		}
		if err := rs.ingestBundle(ctx, name, data, false); err != nil {
			return err
		}
	}

	// Reset refs to the captured set: write every captured ref, then drop
	// refs the backup did not know (the refs file is authoritative).
	latest := m.Steps[len(m.Steps)-1]
	refData, err := sink.read(latest.RefPath)
	if err != nil {
		return status.Errorf(codes.Internal, "restore: %v", err)
	}
	capturedNames := map[string]bool{}
	for _, r := range parseBackupRefs(refData) {
		capturedNames[r.Ref] = true
		if err := s.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: r.Ref, New: r.OID}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	current, err := s.listRefs(ctx, name)
	if err != nil {
		return err
	}
	for _, r := range current {
		if r.Name == "HEAD" || capturedNames[r.Name] {
			continue
		}
		if err := s.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: r.Name}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	if err := setHead(); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if sink.exists(latest.CustomHooksPath) {
		hooks, herr := sink.read(latest.CustomHooksPath)
		if herr != nil {
			return status.Error(codes.Internal, herr.Error())
		}
		if err := s.Meta.SetRepoBlob(ctx, name, customHooksKey, hooks); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
	return nil
}

// The RepositoryService backup pair (server-side backups; the sink layout
// lives above). Skipped-repo semantics collapse into the manifest's
// non_existent record — the single-node sink always accepts the run.
func (rs *repositoryService) BackupRepository(ctx context.Context, in *gitalypb.BackupRepositoryRequest) (*gitalypb.BackupRepositoryResponse, error) {
	sink, err := rs.s.backupSinkOf()
	if err != nil {
		return nil, err
	}
	if err := rs.s.createBackup(ctx, sink, in.GetRepository(), in.GetVanityRepository(),
		in.GetBackupId(), in.GetLatestBackupId(), in.GetPathPrefix(), in.GetIncremental()); err != nil {
		return nil, err
	}
	return &gitalypb.BackupRepositoryResponse{}, nil
}

func (rs *repositoryService) RestoreRepository(ctx context.Context, in *gitalypb.RestoreRepositoryRequest) (*gitalypb.RestoreRepositoryResponse, error) {
	sink, err := rs.s.backupSinkOf()
	if err != nil {
		return nil, err
	}
	if err := rs.s.restoreBackup(ctx, sink, in.GetRepository(), in.GetVanityRepository(),
		in.GetBackupId(), in.GetAlwaysCreate(), in.GetUseLatest(), in.GetPathPrefix()); err != nil {
		return nil, err
	}
	return &gitalypb.RestoreRepositoryResponse{}, nil
}

// writeBackupIDMarker implements the BackupService ID registry: the marker
// file's modification time orders runs (latest wins).
func (s *Server) writeBackupIDMarker(sink *backupSink, backupID, pathPrefix string) error {
	if backupID == "" {
		return status.Error(codes.InvalidArgument, "empty backup ID")
	}
	prefix := pathPrefix
	if prefix == "" {
		prefix = "."
	}
	return sink.write(fmt.Sprintf("%s/%s/%s", prefix, backupIDPrefix, backupID), nil)
}

func (s *Server) readLatestBackupID(sink *backupSink, pathPrefix string) (string, error) {
	prefix := pathPrefix
	if prefix == "" {
		prefix = "."
	}
	dir := sink.path(fmt.Sprintf("%s/%s", prefix, backupIDPrefix))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", status.Error(codes.NotFound, "no backup id markers found")
	}
	latest, mod := "", time.Time{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		// Lexicographic tiebreak for equal timestamps — coarse filesystem
		// clocks make ties the common case on rapid writes.
		if latest == "" || info.ModTime().After(mod) ||
			(info.ModTime().Equal(mod) && e.Name() > latest) {
			latest, mod = e.Name(), info.ModTime()
		}
	}
	if latest == "" {
		return "", status.Error(codes.NotFound, "no backup id markers found")
	}
	return latest, nil
}

// --- BackupService (the ID registry gRPC face) ---

type backupService struct {
	gitalypb.UnimplementedBackupServiceServer
	s *Server
}

func (bs *backupService) WriteBackupID(ctx context.Context, in *gitalypb.WriteBackupIDRequest) (*gitalypb.WriteBackupIDResponse, error) {
	sink, err := bs.s.backupSinkOf()
	if err != nil {
		return nil, err
	}
	if err := bs.s.writeBackupIDMarker(sink, in.GetBackupId(), in.GetPathPrefix()); err != nil {
		return nil, err
	}
	return &gitalypb.WriteBackupIDResponse{}, nil
}

func (bs *backupService) ReadLatestBackupID(ctx context.Context, in *gitalypb.ReadLatestBackupIDRequest) (*gitalypb.ReadLatestBackupIDResponse, error) {
	sink, err := bs.s.backupSinkOf()
	if err != nil {
		return nil, err
	}
	id, err := bs.s.readLatestBackupID(sink, in.GetPathPrefix())
	if err != nil {
		return nil, err
	}
	return &gitalypb.ReadLatestBackupIDResponse{BackupId: id}, nil
}
