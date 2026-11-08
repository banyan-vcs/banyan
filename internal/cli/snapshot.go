// snapshot.go — banyan snapshot: checkpoint/restore of the whole thin
// workspace (docs/06). Purely local and offline — the dirty set's bytes are
// copied verbatim under .banyan/snapshots/<name>/files/, clean files restore
// from the content-addressed cache (server backfill only when the cache was
// pruned). This is a time machine, not stash-apply: restoring does not merge
// onto a moved base, and apply never destroys untracked bytes it did not
// capture — porting old changes forward onto a new head is stack's job.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const snapshotsDir = ".banyan/snapshots"

// untrackedSnapshot records one untracked file captured at save time (bytes
// live in the snapshot's files/ mirror; the mode is re-derived from disk).
type untrackedSnapshot struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// snapshotDoc is the full state of one checkpoint: the manifest as it stood
// (tracked file map + view anchors) plus the dirty classification. Modified
// and untracked files have verbatim copies under files/<rel>.
type snapshotDoc struct {
	Name      string               `json:"name"`
	Created   string               `json:"created"`
	Message   string               `json:"message,omitempty"`
	Commit    string               `json:"commit"` // manifest at save time
	Tree      string               `json:"tree"`
	Files     map[string]fileState `json:"files"`                // tracked state at save
	Modified  []string             `json:"modified,omitempty"`   // dirty at save — bytes in files/
	Deleted   []string             `json:"deleted,omitempty"`    // tracked, missing at save
	Untracked []untrackedSnapshot  `json:"untracked,omitempty"` // not tracked at save — bytes in files/
}

// runSnapshot dispatches the snapshot subcommands (save is the default).
func runSnapshot(wsRoot string, args []string, stdout, stderr io.Writer) int {
	sub := "save"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "save":
		return snapSave(wsRoot, args, stdout, stderr)
	case "ls":
		return snapList(wsRoot, stdout, stderr)
	case "apply":
		return snapApply(wsRoot, args, stdout, stderr)
	case "drop":
		return snapDrop(wsRoot, args, stdout, stderr)
	default:
		return usageErr(stderr, "snapshot: unknown subcommand %q (save | ls | apply | drop)", sub)
	}
}

// snapSave checkpoints the current working state. The working tree is left
// untouched — unlike git stash there is no implicit revert; restore is an
// explicit `snapshot apply`.
func snapSave(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var message, name string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" && i+1 < len(args):
			i++
			message = args[i]
		case (a == "-n" || a == "--name") && i+1 < len(args):
			i++
			name = args[i]
		case strings.HasPrefix(a, "-"):
			return usageErr(stderr, "snapshot save: unknown flag %q", a)
		default:
			return usageErr(stderr, "snapshot save: unexpected argument %q", a)
		}
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	st, err := ws.scan()
	if err != nil {
		return cmdErr(stderr, err)
	}
	if !st.dirty() && len(st.Untracked) == 0 {
		fmt.Fprintln(stdout, "nothing to snapshot, working tree clean")
		return 0
	}
	if name == "" {
		if name, err = nextSnapshotName(ws.root); err != nil {
			return cmdErr(stderr, err)
		}
	}
	if !validSnapshotName(name) {
		return usageErr(stderr, "snapshot save: invalid name %q", name)
	}
	dir := filepath.Join(ws.root, snapshotsDir, name)
	if _, serr := os.Stat(filepath.Join(dir, "snapshot.json")); serr == nil {
		return usageErr(stderr, "snapshot save: %q already exists", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return cmdErr(stderr, err)
	}

	doc := snapshotDoc{
		Name:     name,
		Created:  time.Now().Format(time.RFC3339),
		Message:  message,
		Commit:   ws.man.Commit,
		Tree:     ws.man.Tree,
		Files:    ws.man.Files,
		Modified: st.Modified,
		Deleted:  st.Deleted,
	}
	for _, p := range st.Modified {
		if err := copyToSnapshot(ws.root, dir, p); err != nil {
			return cmdErr(stderr, err)
		}
	}
	for _, p := range st.Untracked {
		full := filepath.Join(ws.root, filepath.FromSlash(p))
		mode := "100644"
		if info, serr := os.Stat(full); serr == nil && info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		doc.Untracked = append(doc.Untracked, untrackedSnapshot{Path: p, Mode: mode})
		if err := copyToSnapshot(ws.root, dir, p); err != nil {
			return cmdErr(stderr, err)
		}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return cmdErr(stderr, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), raw, 0o644); err != nil {
		return cmdErr(stderr, err)
	}
	fmt.Fprintf(stdout, "snapshotted %s (%d modified, %d deleted, %d untracked)\n",
		name, len(st.Modified), len(st.Deleted), len(st.Untracked))
	return 0
}

// copyToSnapshot copies one working-tree file verbatim into the snapshot's
// files/ mirror — hashes are for status, bytes are for restore.
func copyToSnapshot(root, dir, rel string) error {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, "files", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func snapList(wsRoot string, stdout, stderr io.Writer) int {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	docs, err := loadSnapshots(ws.root)
	if err != nil {
		return cmdErr(stderr, err)
	}
	if len(docs) == 0 {
		fmt.Fprintln(stdout, "no snapshots")
		return 0
	}
	for _, d := range docs {
		msg := d.Message
		if msg == "" {
			msg = "-"
		}
		dirty := len(d.Modified) + len(d.Deleted) + len(d.Untracked)
		fmt.Fprintf(stdout, "%s\t%s\t%d dirty\t%s\n", d.Name, d.Created, dirty, msg)
	}
	return 0
}

func loadSnapshots(root string) ([]snapshotDoc, error) {
	entries, err := os.ReadDir(filepath.Join(root, snapshotsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var docs []snapshotDoc
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, snapshotsDir, e.Name(), "snapshot.json"))
		if err != nil {
			continue
		}
		var d snapshotDoc
		if json.Unmarshal(raw, &d) == nil {
			docs = append(docs, d)
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })
	return docs, nil
}

func loadSnapshot(root, name string) (*snapshotDoc, error) {
	if !validSnapshotName(name) {
		return nil, fmt.Errorf("snapshot: invalid name %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(root, snapshotsDir, name, "snapshot.json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot %q not found", name)
	}
	var d snapshotDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	return &d, nil
}

// snapApply restores the workspace to the snapshot-time state byte for byte:
// dirty files from the snapshot mirror, clean files from the cache (or the
// server, if the cache was pruned). Tracked files that landed after the
// snapshot disappear locally (the server keeps them); untracked files created
// since are left alone. Refuses a dirty tree unless --force.
func snapApply(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var name string
	force := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--force":
			force = true
		case !strings.HasPrefix(a, "-") && name == "":
			name = a
		default:
			return usageErr(stderr, "snapshot apply: unexpected argument %q", a)
		}
	}
	if name == "" {
		return usageErr(stderr, "snapshot apply: <name> required")
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	doc, err := loadSnapshot(ws.root, name)
	if err != nil {
		return cmdErr(stderr, err)
	}
	st, err := ws.scan()
	if err != nil {
		return cmdErr(stderr, err)
	}
	if st.dirty() && !force {
		fmt.Fprintln(stderr, "conflict: working tree has local changes — commit or snapshot first, or --force")
		return 1
	}

	snapDir := filepath.Join(ws.root, snapshotsDir, name)
	var client *client
	union := map[string]bool{}
	for p := range ws.man.Files {
		union[p] = true
	}
	for p := range doc.Files {
		union[p] = true
	}
	modified := map[string]bool{}
	for _, p := range doc.Modified {
		modified[p] = true
	}
	for p := range union {
		fs, inSnap := doc.Files[p]
		if !inSnap {
			_ = os.Remove(filepath.Join(ws.root, filepath.FromSlash(p)))
			continue
		}
		var data []byte
		if modified[p] {
			if data, err = os.ReadFile(filepath.Join(snapDir, "files", filepath.FromSlash(p))); err != nil {
				return cmdErr(stderr, err)
			}
		} else if cached, ok := ws.cacheGet(fs.OID); ok {
			data = cached
		} else {
			if client == nil {
				client = newClient(ws.cfg.Server)
			}
			if data, err = client.getObject(ws.cfg.Repo, fs.OID); err != nil {
				return cmdErr(stderr, err)
			}
			ws.cachePut(fs.OID, data)
		}
		if err := writeFileState(ws.root, p, data, fs.Mode); err != nil {
			return cmdErr(stderr, err)
		}
	}
	for _, u := range doc.Untracked {
		data, rerr := os.ReadFile(filepath.Join(snapDir, "files", filepath.FromSlash(u.Path)))
		if rerr != nil {
			return cmdErr(stderr, rerr)
		}
		if err := writeFileState(ws.root, u.Path, data, u.Mode); err != nil {
			return cmdErr(stderr, err)
		}
	}

	prevCommit := ws.man.Commit
	files := make(map[string]fileState, len(doc.Files))
	for p, fs := range doc.Files {
		files[p] = fs
	}
	ws.man = manifest{Commit: doc.Commit, Tree: doc.Tree, Files: files}
	if err := ws.save(); err != nil {
		return cmdErr(stderr, err)
	}
	if prevCommit != "" && prevCommit != doc.Commit {
		fmt.Fprintf(stderr, "note: base moved since snapshot (%s → %s); pull will overwrite the restored state — commit it first to keep it\n",
			short(doc.Commit), short(prevCommit))
	}
	fmt.Fprintf(stdout, "applied %s (%s)\n", name, doc.Created)
	return 0
}

// writeFileState materializes one file with its git mode (100755 → exec).
func writeFileState(root, rel string, data []byte, mode string) error {
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode == "100755" {
		perm = 0o755
	}
	return os.WriteFile(dst, data, perm)
}

func snapDrop(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return usageErr(stderr, "snapshot drop: <name> required")
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	name := args[0]
	if !validSnapshotName(name) {
		return usageErr(stderr, "snapshot drop: invalid name %q", name)
	}
	dir := filepath.Join(ws.root, snapshotsDir, name)
	if _, serr := os.Stat(filepath.Join(dir, "snapshot.json")); serr != nil {
		return usageErr(stderr, "snapshot drop: %q not found", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return cmdErr(stderr, err)
	}
	fmt.Fprintf(stdout, "dropped %s\n", name)
	return 0
}

func validSnapshotName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.ContainsAny(name, " \t")
}

// nextSnapshotName picks snap-<N>, N one past the highest existing.
func nextSnapshotName(root string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, snapshotsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return "snap-1", nil
		}
		return "", err
	}
	max := 0
	for _, e := range entries {
		if n, aerr := strconv.Atoi(strings.TrimPrefix(e.Name(), "snap-")); aerr == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("snap-%d", max+1), nil
}
