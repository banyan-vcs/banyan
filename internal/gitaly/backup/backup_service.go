package backup

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Runner is the server-side backup engine: sink layout, manifests,
// bundle/incremental capture and restore. The gRPC BackupService and the
// RepositoryService backup RPCs (repository family) both drive it.
type Runner struct {
	*kit.Base
}

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

func (s *backupSink) Path(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

func (s *backupSink) Write(rel string, data []byte) error {
	p := s.Path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (s *backupSink) Read(rel string) ([]byte, error) { return os.ReadFile(s.Path(rel)) }

func (s *backupSink) Exists(rel string) bool {
	_, err := os.Stat(s.Path(rel))
	return err == nil
}

// backupSinkOf answers the configured sink, or the upstream-shaped
// precondition error when server-side backups are off.
func (r *Runner) backupSinkOf() (*backupSink, error) {
	if r.Opts.BackupDir == "" {
		return nil, status.Error(codes.FailedPrecondition, "server-side backups are not configured")
	}
	return &backupSink{root: r.Opts.BackupDir}, nil
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
func backupRefsFile(refs []kit.ListedRef) []byte {
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
func (r *Runner) CreateBackup(ctx context.Context, sink *backupSink,
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

	name, err := r.ExistingRepo(ctx, repo)
	m := &backupManifest{ID: backupID, ObjectFormat: "sha1"}
	if err != nil {
		// A non-existent repository still gets a manifest: restoring a
		// deleted repo must know there is nothing to bring back.
		m.NonExistent = true
		m.Empty = true
		return sink.Write(manifestRel(prefix, storage, relPath, backupID), renderManifest(m))
	}

	refs, err := r.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	def, _ := r.DefaultBranch(ctx, name)
	m.HeadReference = "refs/heads/" + def

	n, previousRefs := 1, ""
	if incremental {
		if prev, perr := latestManifest(sink, prefix, storage, relPath, latestBackupID); perr == nil &&
			!prev.NonExistent && len(prev.Steps) > 0 && sink.Exists(prev.Steps[len(prev.Steps)-1].RefPath) {
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

	if err := sink.Write(step.RefPath, backupRefsFile(refs)); err != nil {
		return status.Errorf(codes.Internal, "backup: %v", err)
	}
	m.Empty = !hasRefRows(refs)
	if !m.Empty {
		var bundle []byte
		if previousRefs != "" {
			prevData, rerr := sink.Read(previousRefs)
			if rerr != nil {
				return status.Errorf(codes.Internal, "backup: %v", rerr)
			}
			bundle, err = r.incrementalBundle(ctx, name, refs, parseBackupRefs(prevData))
		} else {
			bundle, err = r.FullBundle(ctx, name, refs)
		}
		if err != nil {
			return status.Errorf(codes.Internal, "backup: %v", err)
		}
		if err := sink.Write(step.BundlePath, bundle); err != nil {
			return status.Errorf(codes.Internal, "backup: %v", err)
		}
	}

	// Custom hooks ride the repo-blob plane (the custom-hooks tarball).
	if hooks, ok, herr := r.Meta.GetRepoBlob(ctx, name, kit.CustomHooksKey); herr == nil && ok {
		if err := sink.Write(step.CustomHooksPath, hooks); err != nil {
			return status.Errorf(codes.Internal, "backup: %v", err)
		}
	}
	return sink.Write(manifestRel(prefix, storage, relPath, backupID), renderManifest(m))
}

func hasRefRows(refs []kit.ListedRef) bool {
	for _, r := range refs {
		if r.Name != "HEAD" {
			return true
		}
	}
	return false
}

// fullBundle is the complete closure bundle (CreateBundle's bytes).
func (r *Runner) FullBundle(ctx context.Context, repo string, refs []kit.ListedRef) ([]byte, error) {
	brefs := make([]kit.BundleRef, 0, len(refs))
	for _, r := range refs {
		brefs = append(brefs, kit.BundleRef{Ref: r.Name, OID: r.Head})
	}
	var buf strings.Builder
	if err := r.WriteBundle(ctx, repo, brefs, &buf); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// incrementalBundle carries only objects the previous refs did not cover;
// the boundary commits land as bundle prerequisites (the form
// ingestBundle verifies on the way back in).
func (r *Runner) incrementalBundle(ctx context.Context, repo string,
	refs []kit.ListedRef, prev []backupRefRow) ([]byte, error) {

	toBundleRefs := func(rows []backupRefRow) []kit.BundleRef {
		out := make([]kit.BundleRef, 0, len(rows))
		for _, r := range rows {
			out = append(out, kit.BundleRef{Ref: r.Ref, OID: r.OID})
		}
		return out
	}
	curRefs := toBundleRefs(parseBackupRefs(backupRefsFile(refs)))

	prevOrder, err := r.BundleClosure(ctx, repo, toBundleRefs(prev))
	if err != nil {
		return nil, err
	}
	prevSet := make(map[model.OID]bool, len(prevOrder))
	for _, oid := range prevOrder {
		prevSet[oid] = true
	}
	curOrder, err := r.BundleClosure(ctx, repo, curRefs)
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
		typ, raw, err := r.Objects.Raw(ctx, repo, oid)
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
	b.WriteString(kit.BundleHeaderV2 + "\n")
	for _, pre := range kit.SortedOIDs(prereqs) {
		fmt.Fprintf(&b, "-%s\n", pre)
	}
	byName := make(map[string]model.OID, len(curRefs))
	names := make([]string, 0, len(curRefs))
	for _, r := range curRefs {
		byName[r.Ref] = r.OID
		names = append(names, r.Ref)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%s %s\n", byName[n], n)
	}
	b.WriteString("\n")

	i := 0
	err = storage.StreamEncodePack(&b, r.Algo, len(included), func(int) (model.ObjectType, []byte, error) {
		oid := included[i]
		i++
		typ, raw, err := r.Objects.Raw(ctx, repo, oid)
		return typ, raw, err
	})
	if err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// latestManifest finds the repo's manifest: the named backup when given,
// else the modification-time-newest manifest file.
func latestManifest(sink *backupSink, prefix, storage, relPath, backupID string) (*backupManifest, error) {
	if backupID != "" {
		data, err := sink.Read(manifestRel(prefix, storage, relPath, backupID))
		if err != nil {
			return nil, err
		}
		return parseManifest(data, backupID), nil
	}
	dir := sink.Path(fmt.Sprintf("%s/manifests/%s/%s", prefix, storage, relPath))
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
	data, err := sink.Read(manifestRel(prefix, storage, relPath, newest))
	if err != nil {
		return nil, err
	}
	return parseManifest(data, newest), nil
}

// restoreBackup rebuilds a repository from a backup run in the sink.
func (r *Runner) RestoreBackup(ctx context.Context, sink *backupSink,
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

	_, existErr := r.ExistingRepo(ctx, repo)
	exists := existErr == nil
	if exists && alwaysCreate {
		return status.Error(codes.FailedPrecondition, "gitaly: repository already exists")
	}

	if m.NonExistent {
		return nil // nothing to restore
	}
	name := repo.GetRelativePath()
	if !exists {
		if err := r.Meta.CreateRepo(ctx, name, r.Algo); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
	setHead := func() error {
		if m.HeadReference == "" {
			return nil
		}
		return r.Meta.SetDefaultBranch(ctx, name, strings.TrimPrefix(m.HeadReference, "refs/heads/"))
	}
	if m.Empty || len(m.Steps) == 0 {
		return setHead()
	}

	for _, step := range m.Steps {
		if !sink.Exists(step.BundlePath) {
			continue // an increment with nothing new
		}
		data, rerr := sink.Read(step.BundlePath)
		if rerr != nil {
			return status.Errorf(codes.Internal, "restore: %v", rerr)
		}
		if err := r.IngestBundle(ctx, name, data, false); err != nil {
			return err
		}
	}

	// Reset refs to the captured set: write every captured ref, then drop
	// refs the backup did not know (the refs file is authoritative).
	latest := m.Steps[len(m.Steps)-1]
	refData, err := sink.Read(latest.RefPath)
	if err != nil {
		return status.Errorf(codes.Internal, "restore: %v", err)
	}
	capturedNames := map[string]bool{}
	for _, row := range parseBackupRefs(refData) {
		capturedNames[row.Ref] = true
		if err := r.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: row.Ref, New: row.OID}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	current, err := r.AdvertisedRefs(ctx, name)
	if err != nil {
		return err
	}
	for _, cur := range current {
		if cur.Name == "HEAD" || capturedNames[cur.Name] {
			continue
		}
		if err := r.Svc.UpdateRef(ctx, service.RefUpdate{Repo: name, Name: cur.Name}); err != nil {
			return status.Errorf(codes.FailedPrecondition, "gitaly: %v", err)
		}
	}
	if err := setHead(); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if sink.Exists(latest.CustomHooksPath) {
		hooks, herr := sink.Read(latest.CustomHooksPath)
		if herr != nil {
			return status.Error(codes.Internal, herr.Error())
		}
		if err := r.Meta.SetRepoBlob(ctx, name, kit.CustomHooksKey, hooks); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
	return nil
}

// writeBackupIDMarker implements the BackupService ID registry: the marker
// file's modification time orders runs (latest wins).
func (r *Runner) writeBackupIDMarker(sink *backupSink, backupID, pathPrefix string) error {
	if backupID == "" {
		return status.Error(codes.InvalidArgument, "empty backup ID")
	}
	prefix := pathPrefix
	if prefix == "" {
		prefix = "."
	}
	return sink.Write(fmt.Sprintf("%s/%s/%s", prefix, backupIDPrefix, backupID), nil)
}

func (r *Runner) readLatestBackupID(sink *backupSink, pathPrefix string) (string, error) {
	prefix := pathPrefix
	if prefix == "" {
		prefix = "."
	}
	dir := sink.Path(fmt.Sprintf("%s/%s", prefix, backupIDPrefix))
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
	*kit.Base
}

func (bs *backupService) WriteBackupID(ctx context.Context, in *gitalypb.WriteBackupIDRequest) (*gitalypb.WriteBackupIDResponse, error) {
	r := Runner{Base: bs.Base}
	sink, err := r.backupSinkOf()
	if err != nil {
		return nil, err
	}
	if err := r.writeBackupIDMarker(sink, in.GetBackupId(), in.GetPathPrefix()); err != nil {
		return nil, err
	}
	return &gitalypb.WriteBackupIDResponse{}, nil
}

func (bs *backupService) ReadLatestBackupID(ctx context.Context, in *gitalypb.ReadLatestBackupIDRequest) (*gitalypb.ReadLatestBackupIDResponse, error) {
	r := Runner{Base: bs.Base}
	sink, err := r.backupSinkOf()
	if err != nil {
		return nil, err
	}
	id, err := r.readLatestBackupID(sink, in.GetPathPrefix())
	if err != nil {
		return nil, err
	}
	return &gitalypb.ReadLatestBackupIDResponse{BackupId: id}, nil
}

// Sink mirrors backupSinkOf for cross-package callers (node partition
// backups) — the precondition error is the upstream-shaped one.
func (r *Runner) SinkForNode() (*backupSink, error) { return r.backupSinkOf() }
