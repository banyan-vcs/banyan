// Thin-workspace state (docs/06 §2): .banyan/{config,manifest}.json plus a
// content-addressed object cache. No .git, no local history — every history
// question goes to the server; the manifest tree is the only local anchor.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

const (
	configF   = ".banyan/config.json"
	manifestF = ".banyan/manifest.json"
	cacheDir  = ".banyan/cache"
)

type wsConfig struct {
	Server string   `json:"server"`
	Repo   string   `json:"repo"`
	Sub    string   `json:"sub,omitempty"` // subtree view ("" = root repo)
	Branch string   `json:"branch,omitempty"`
	Sparse []string `json:"sparse,omitempty"` // top-level names materialized (cone)
}

type fileState struct {
	OID  string `json:"oid"`
	Mode string `json:"mode"`
}

type manifest struct {
	Commit string               `json:"commit"`
	Tree   string               `json:"tree"`
	Files  map[string]fileState `json:"files"`
}

type workspace struct {
	root string
	cfg  wsConfig
	man  manifest
}

func loadWorkspace(root string) (*workspace, error) {
	cfgRaw, err := os.ReadFile(filepath.Join(root, configF))
	if err != nil {
		return nil, fmt.Errorf("not a banyan workspace (missing %s): %w", configF, err)
	}
	var cfg wsConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return nil, fmt.Errorf("read %s: %w", configF, err)
	}
	ws := &workspace{root: root, cfg: cfg, man: manifest{Files: map[string]fileState{}}}
	if raw, err := os.ReadFile(filepath.Join(root, manifestF)); err == nil {
		if err := json.Unmarshal(raw, &ws.man); err != nil {
			return nil, fmt.Errorf("read %s: %w", manifestF, err)
		}
	}
	if ws.man.Files == nil {
		ws.man.Files = map[string]fileState{}
	}
	return ws, nil
}

func (w *workspace) save() error {
	for _, p := range []string{configF, manifestF} {
		if err := os.MkdirAll(filepath.Join(w.root, filepath.Dir(p)), 0o755); err != nil {
			return err
		}
	}
	cfgRaw, err := json.MarshalIndent(w.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.root, configF), cfgRaw, 0o644); err != nil {
		return err
	}
	manRaw, err := json.MarshalIndent(w.man, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.root, manifestF), manRaw, 0o644)
}

// --- object cache (blobs + trees, content-addressed, freely prunable) ---

func (w *workspace) cachePath(oid string) string {
	return filepath.Join(w.root, cacheDir, oid)
}

func (w *workspace) cachePut(oid string, data []byte) {
	if oid == "" {
		return
	}
	p := w.cachePath(oid)
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, data, 0o644)
}

func (w *workspace) cacheGet(oid string) ([]byte, bool) {
	b, err := os.ReadFile(w.cachePath(oid))
	return b, err == nil
}

// blobHash is the git blob OID (content addressing is the workspace's
// integrity contract — status compares hashes, never timestamps).
func blobHash(data []byte) string {
	return string(model.ObjectOID(model.BlobType, data, model.SHA1))
}

// --- status scan (offline: manifest vs working copy) ---

type status struct {
	Modified  []string // tracked, content differs
	Deleted   []string // tracked, file missing
	Untracked []string // on disk, not tracked (inside materialized scope)
}

func (s *status) dirty() bool { return len(s.Modified)+len(s.Deleted) > 0 }

func (w *workspace) scan() (*status, error) {
	st := &status{}
	for path, fs := range w.man.Files {
		data, err := os.ReadFile(filepath.Join(w.root, filepath.FromSlash(path)))
		if err != nil {
			if os.IsNotExist(err) {
				st.Deleted = append(st.Deleted, path)
			}
			continue
		}
		if blobHash(data) != fs.OID {
			st.Modified = append(st.Modified, path)
		}
	}
	trackedScope := func(rel string) bool {
		if len(w.cfg.Sparse) == 0 {
			return true
		}
		if !strings.Contains(rel, "/") {
			return true // root-level files always materialize
		}
		for _, s := range w.cfg.Sparse {
			if strings.HasPrefix(rel, s+"/") {
				return true
			}
		}
		return false
	}
	err := filepath.WalkDir(w.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".banyan" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(w.root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if _, tracked := w.man.Files[rel]; tracked {
			return nil
		}
		if !trackedScope(rel) {
			return nil
		}
		st.Untracked = append(st.Untracked, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, list := range []*[]string{&st.Modified, &st.Deleted, &st.Untracked} {
		sort.Strings(*list)
	}
	return st, nil
}

// --- tree walking over (response → cache → server) sources ---

// treeSource resolves tree/blob bytes for the materialization walk: objects
// from a fetch response first, then the local cache, then a per-object GET.
type treeSource struct {
	ws     *workspace
	client *client
	trees  map[string][]byte
	blobs  map[string][]byte
}

func (t *treeSource) tree(oid string) (*model.Tree, error) {
	raw, ok := t.trees[oid]
	if !ok {
		raw, ok = t.ws.cacheGet(oid)
	}
	if !ok {
		var err error
		raw, err = t.client.getObject(t.ws.cfg.Repo, oid)
		if err != nil {
			return nil, fmt.Errorf("tree %s: %w", oid, err)
		}
		t.ws.cachePut(oid, raw)
	}
	return model.DecodeTree(raw)
}

func (t *treeSource) blob(oid string) ([]byte, error) {
	if raw, ok := t.blobs[oid]; ok {
		return raw, nil
	}
	if raw, ok := t.ws.cacheGet(oid); ok {
		return raw, nil
	}
	raw, err := t.client.getObject(t.ws.cfg.Repo, oid)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", oid, err)
	}
	t.ws.cachePut(oid, raw)
	return raw, nil
}

// collectFiles walks a tree and builds the tracked-file map for a scope —
// no disk writes, no blob fetches (pull diffs the maps first; blob bytes
// are only fetched for files actually written). scope semantics: root-level
// files always, directories only when listed (cone, docs/06 clone --sparse).
func (w *workspace) collectFiles(t *treeSource, treeOID string, sparse []string) (map[string]fileState, error) {
	files := map[string]fileState{}
	inScope := func(name string, isDir bool) bool {
		if len(sparse) == 0 {
			return true
		}
		if !isDir {
			return true
		}
		for _, s := range sparse {
			if s == name {
				return true
			}
		}
		return false
	}
	var walk func(oid, prefix string) error
	walk = func(oid, prefix string) error {
		tree, err := t.tree(oid)
		if err != nil {
			return err
		}
		for _, e := range tree.Entries {
			rel := e.Name
			if prefix != "" {
				rel = prefix + "/" + e.Name
			}
			if e.IsDir {
				if prefix == "" && !inScope(e.Name, true) {
					continue
				}
				if err := walk(string(e.Target), rel); err != nil {
					return err
				}
				continue
			}
			if e.Mode == "160000" { // gitlink: submodules are not materialized
				continue
			}
			files[rel] = fileState{OID: string(e.Target), Mode: e.Mode}
		}
		return nil
	}
	if err := walk(treeOID, ""); err != nil {
		return nil, err
	}
	return files, nil
}

// writeTracked materializes one tracked file (mkdir + fetch blob + write).
func (w *workspace) writeTracked(t *treeSource, rel string, fs fileState) error {
	data, err := t.blob(fs.OID)
	if err != nil {
		return err
	}
	dst := filepath.Join(w.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fs.Mode == "100755" {
		mode = 0o755
	}
	return os.WriteFile(dst, data, mode)
}

// materializeAll writes every collected file (clone path).
func (w *workspace) materializeAll(t *treeSource, files map[string]fileState) error {
	names := make([]string, 0, len(files))
	for rel := range files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for _, rel := range names {
		if err := w.writeTracked(t, rel, files[rel]); err != nil {
			return err
		}
	}
	return nil
}
