// snapshot.go — banyan snapshot: checkpoint/restore of the whole thin
// workspace (docs/client/cli.md). Purely local and offline — the dirty set's bytes are
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

	"github.com/spf13/cobra"
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
	Files     map[string]fileState `json:"files"`               // tracked state at save
	Modified  []string             `json:"modified,omitempty"`  // dirty at save — bytes in files/
	Deleted   []string             `json:"deleted,omitempty"`   // tracked, missing at save
	Untracked []untrackedSnapshot  `json:"untracked,omitempty"` // not tracked at save — bytes in files/
}

// newSnapshotCmd builds the snapshot family. Save is the default subcommand:
// bare `snapshot -m msg` still checkpoints, so the save flags ride the
// parent as persistent flags — one definition serves both spellings.
func newSnapshotCmd(wsRoot *string, stdout, stderr io.Writer) *cobra.Command {
	var message, name string
	parent := &cobra.Command{
		Use:   "snapshot",
		Short: "checkpoint the working state (offline; save is the default)",
		RunE:  cmdRun(func() int { return snapSave(*wsRoot, message, name, stdout, stderr) }),
	}
	parent.PersistentFlags().StringVarP(&message, "message", "m", "", "checkpoint description")
	parent.PersistentFlags().StringVarP(&name, "name", "n", "", "checkpoint name (default: snap-<N>)")
	parent.AddCommand(&cobra.Command{
		Use:   "save",
		Short: "checkpoint the working state (offline)",
		Args:  cobra.NoArgs,
		RunE:  cmdRun(func() int { return snapSave(*wsRoot, message, name, stdout, stderr) }),
	})
	parent.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "list checkpoints",
		Args:  cobra.NoArgs,
		RunE:  cmdRun(func() int { return snapList(*wsRoot, stdout, stderr) }),
	})
	var force bool
	apply := &cobra.Command{
		Use:   "apply <name>",
		Short: "restore a checkpoint byte for byte",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if code := snapApply(*wsRoot, args[0], force, stdout, stderr); code != 0 {
				return &exitError{code}
			}
			return nil
		},
	}
	apply.Flags().BoolVar(&force, "force", false, "apply onto a dirty tree")
	parent.AddCommand(apply)
	parent.AddCommand(&cobra.Command{
		Use:   "drop <name>",
		Short: "remove a checkpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if code := snapDrop(*wsRoot, args[0], stdout, stderr); code != 0 {
				return &exitError{code}
			}
			return nil
		},
	})
	return parent
}

// snapSave checkpoints the current working state. The working tree is left
// untouched — unlike git stash there is no implicit revert; restore is an
// explicit `snapshot apply`.
func snapSave(wsRoot, message, name string, stdout, stderr io.Writer) int {
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
func snapApply(wsRoot, name string, force bool, stdout, stderr io.Writer) int {
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
				client = wsClient(ws)
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

func snapDrop(wsRoot, name string, stdout, stderr io.Writer) int {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
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
