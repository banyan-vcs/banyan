package cli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Exit codes (docs/06 §5): 0 ok / 1 conflict / 2 server or usage error / 3 network.
func cmdErr(stderr io.Writer, err error) int {
	switch e := err.(type) {
	case *apiError:
		fmt.Fprintln(stderr, "error:", err)
		if isConflictErr(e) {
			return 1
		}
		return 2
	case *netError:
		fmt.Fprintln(stderr, "network error:", err)
		return 3
	}
	fmt.Fprintln(stderr, "error:", err)
	return 2
}

func usageErr(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "error: "+format+"\n", args...)
	return 2
}

// parseCloneURL splits a git-style /r/... URL into server base, repo and
// subtree path (both /tree/<p> and legacy //<p> forms, docs/04 §1).
func parseCloneURL(raw string) (base, repo, sub string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", fmt.Errorf("bad URL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", "", fmt.Errorf("bad URL %q: want http(s)://host/r/<repo>[/tree/<path>]", raw)
	}
	p := strings.TrimPrefix(u.Path, "/r/")
	switch {
	case strings.Contains(p, "/tree/"):
		i := strings.Index(p, "/tree/")
		repo = strings.TrimSuffix(p[:i], ".git")
		sub = "/" + strings.TrimPrefix(p[i+len("/tree/"):], "/")
	case strings.Contains(p, "//"):
		i := strings.Index(p, "//")
		repo = strings.TrimSuffix(p[:i], ".git")
		sub = "/" + p[i+2:]
	default:
		repo = strings.TrimSuffix(p, ".git")
	}
	if repo == "" || strings.Contains(repo, "/") {
		return "", "", "", fmt.Errorf("bad repo in URL %q", raw)
	}
	return u.Scheme + "://" + u.Host, repo, sub, nil
}

func defaultAuthor() (name, email string) {
	name = os.Getenv("USER")
	if name == "" {
		name = os.Getenv("USERNAME")
	}
	if name == "" {
		name = "banyan"
	}
	return name, name + "@banyan.local"
}

// --- clone: minimal download — view tree + checkout blobs, never history ---

func runClone(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var rawurl, dir, sparseCSV, branch string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--sparse" && i+1 < len(args):
			i++
			sparseCSV = args[i]
		case (a == "--branch" || a == "-b") && i+1 < len(args):
			i++
			branch = args[i]
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErr(stderr, "clone: unknown flag %q", a)
		case rawurl == "":
			rawurl = a
		case dir == "":
			dir = a
		default:
			return usageErr(stderr, "clone: unexpected argument %q", a)
		}
	}
	if rawurl == "" {
		return usageErr(stderr, "clone: URL required (http://host/r/<repo>[/tree/<path>])")
	}
	base, repo, sub, err := parseCloneURL(rawurl)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}
	if dir == "" {
		dir = repo
		if segs := strings.Split(strings.Trim(sub, "/"), "/"); sub != "" && segs[0] != "" {
			dir = segs[len(segs)-1]
		}
	}
	var sparse []string
	if sparseCSV != "" {
		for _, s := range strings.Split(sparseCSV, ",") {
			if s = strings.TrimSpace(s); s != "" {
				sparse = append(sparse, s)
			}
		}
	}

	c := newClient(base)
	res, err := c.fetchTrees(repo, sub, branch, "", sparse)
	if err != nil {
		return cmdErr(stderr, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return cmdErr(stderr, err)
	}
	ws := &workspace{root: dir, cfg: wsConfig{Server: base, Repo: repo, Sub: sub, Branch: res.Branch, Sparse: sparse},
		man: manifest{Commit: res.Head, Tree: res.Tree, Files: map[string]fileState{}}}
	for oid, raw := range res.Trees {
		ws.cachePut(oid, raw)
	}
	for oid, raw := range res.Blobs {
		ws.cachePut(oid, raw)
	}
	src := &treeSource{ws: ws, client: c, trees: res.Trees, blobs: res.Blobs}
	files, err := ws.collectFiles(src, res.Tree, sparse)
	if err != nil {
		return cmdErr(stderr, err)
	}
	ws.man.Files = files
	if err := ws.materializeAll(src, files); err != nil {
		return cmdErr(stderr, err)
	}
	if err := ws.save(); err != nil {
		return cmdErr(stderr, err)
	}
	fmt.Fprintf(stdout, "cloned %s%s (branch %s, %d files, head %s)\n", repo, sub, res.Branch, len(files), short(res.Head))
	return 0
}

// --- status: offline manifest vs working copy ---

func runStatus(wsRoot string, args []string, stdout, stderr io.Writer) int {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	st, err := ws.scan()
	if err != nil {
		return cmdErr(stderr, err)
	}
	for _, p := range st.Modified {
		fmt.Fprintf(stdout, "M  %s\n", p)
	}
	for _, p := range st.Deleted {
		fmt.Fprintf(stdout, "D  %s\n", p)
	}
	for _, p := range st.Untracked {
		fmt.Fprintf(stdout, "?? %s\n", p)
	}
	if st.dirty() || len(st.Untracked) > 0 {
		fmt.Fprintf(stdout, "(%d modified, %d deleted, %d untracked)\n", len(st.Modified), len(st.Deleted), len(st.Untracked))
	} else {
		fmt.Fprintln(stdout, "clean")
	}
	return 0
}

// --- commit: scan → upload new blobs → server-side assembly (docs/06 §4.2) ---

func runCommit(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var message, name, email string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" && i+1 < len(args):
			i++
			message = args[i]
		case a == "--name" && i+1 < len(args):
			i++
			name = args[i]
		case a == "--email" && i+1 < len(args):
			i++
			email = args[i]
		case strings.HasPrefix(a, "-"):
			return usageErr(stderr, "commit: unknown flag %q", a)
		default:
			return usageErr(stderr, "commit: unexpected argument %q", a)
		}
	}
	if message == "" {
		return usageErr(stderr, "commit: -m <message> required")
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
		fmt.Fprintln(stdout, "nothing to commit, working tree clean")
		return 0
	}

	var changes []map[string]any
	var uploads []map[string]any
	uploaded := map[string]bool{}
	upload := func(path string, data []byte, mode string) {
		oid := blobHash(data)
		changes = append(changes, map[string]any{"path": path, "blob": oid, "mode": mode})
		if !uploaded[oid] {
			uploaded[oid] = true
			uploads = append(uploads, map[string]any{"oid": oid, "type": "blob", "data": data})
		}
	}
	for _, p := range st.Modified {
		data, err := os.ReadFile(filepath.Join(ws.root, filepath.FromSlash(p)))
		if err != nil {
			return cmdErr(stderr, err)
		}
		mode := ws.man.Files[p].Mode
		if mode == "" {
			mode = "100644"
		}
		upload(p, data, mode)
	}
	for _, p := range st.Untracked {
		full := filepath.Join(ws.root, filepath.FromSlash(p))
		data, err := os.ReadFile(full)
		if err != nil {
			return cmdErr(stderr, err)
		}
		mode := "100644"
		if info, serr := os.Stat(full); serr == nil && info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		upload(p, data, mode)
	}
	for _, p := range st.Deleted {
		changes = append(changes, map[string]any{"path": p, "delete": true})
	}

	c := newClient(ws.cfg.Server)
	if len(uploads) > 0 {
		if err := c.putObjects(ws.cfg.Repo, uploads); err != nil {
			return cmdErr(stderr, err)
		}
	}
	if name == "" {
		defName, defEmail := defaultAuthor()
		name = defName
		if email == "" {
			email = defEmail
		}
	}
	res, err := c.commit(ws.cfg.Repo, ws.cfg.Sub, ws.cfg.Branch, ws.man.Commit, message, name, email, changes)
	if err != nil {
		if isConflictErr(err) {
			fmt.Fprintln(stderr, "conflict: base moved — run `banyan pull`, then retry")
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return cmdErr(stderr, err)
	}

	// Update the manifest from the local change list (server confirmed head+tree).
	for _, ch := range changes {
		path, _ := ch["path"].(string)
		if del, _ := ch["delete"].(bool); del {
			delete(ws.man.Files, path)
			continue
		}
		ws.man.Files[path] = fileState{OID: ch["blob"].(string), Mode: ch["mode"].(string)}
	}
	ws.man.Commit, ws.man.Tree = res.Head, res.Tree
	if err := ws.save(); err != nil {
		return cmdErr(stderr, err)
	}
	fmt.Fprintf(stdout, "committed %s (%d changes)\n", short(res.Head), len(changes))
	return 0
}

// --- pull: server-side tree-diff — the download equals the checkout difference ---

func runPull(wsRoot string, args []string, stdout, stderr io.Writer) int {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	st, err := ws.scan()
	if err != nil {
		return cmdErr(stderr, err)
	}
	if st.dirty() {
		fmt.Fprintf(stderr, "conflict: working tree has local changes (commit or revert first):\n")
		for _, p := range st.Modified {
			fmt.Fprintf(stderr, "  M %s\n", p)
		}
		for _, p := range st.Deleted {
			fmt.Fprintf(stderr, "  D %s\n", p)
		}
		return 1
	}

	c := newClient(ws.cfg.Server)
	res, err := c.fetchTrees(ws.cfg.Repo, ws.cfg.Sub, ws.cfg.Branch, ws.man.Tree, nil)
	if err != nil {
		return cmdErr(stderr, err)
	}
	if res.Head == ws.man.Commit && res.Tree == ws.man.Tree {
		fmt.Fprintln(stdout, "Already up to date.")
		return 0
	}
	for oid, raw := range res.Trees {
		ws.cachePut(oid, raw)
	}
	for oid, raw := range res.Blobs {
		ws.cachePut(oid, raw)
	}
	src := &treeSource{ws: ws, client: c, trees: res.Trees, blobs: res.Blobs}
	newFiles, err := ws.collectFiles(src, res.Tree, ws.cfg.Sparse)
	if err != nil {
		return cmdErr(stderr, err)
	}

	untracked := map[string]bool{}
	for _, p := range st.Untracked {
		untracked[p] = true
	}
	var changed, added []string
	for p, nf := range newFiles {
		if of, ok := ws.man.Files[p]; !ok {
			added = append(added, p)
		} else if of.OID != nf.OID {
			changed = append(changed, p)
		}
	}
	var deleted []string
	for p := range ws.man.Files {
		if _, ok := newFiles[p]; !ok {
			deleted = append(deleted, p)
		}
	}
	sort.Strings(changed)
	sort.Strings(added)
	sort.Strings(deleted)
	for _, p := range append(append([]string{}, changed...), added...) {
		if untracked[p] {
			fmt.Fprintf(stderr, "conflict: incoming change would overwrite untracked file:\n  %s\n", p)
			return 1
		}
	}

	for _, p := range changed {
		if err := ws.writeTracked(src, p, newFiles[p]); err != nil {
			return cmdErr(stderr, err)
		}
	}
	for _, p := range added {
		if err := ws.writeTracked(src, p, newFiles[p]); err != nil {
			return cmdErr(stderr, err)
		}
	}
	for _, p := range deleted {
		if err := os.Remove(filepath.Join(ws.root, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			return cmdErr(stderr, err)
		}
	}
	ws.man.Files = newFiles
	ws.man.Commit, ws.man.Tree = res.Head, res.Tree
	if err := ws.save(); err != nil {
		return cmdErr(stderr, err)
	}
	fmt.Fprintf(stdout, "updated to %s (%d changed, %d added, %d deleted)\n", short(res.Head), len(changed), len(added), len(deleted))
	return 0
}

// --- log: remote history, zero download ---

func runLog(wsRoot string, args []string, stdout, stderr io.Writer) int {
	limit := 20
	var path string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" && i+1 < len(args):
			i++
			fmt.Sscanf(args[i], "%d", &limit)
		case strings.HasPrefix(a, "-"):
			return usageErr(stderr, "log: unknown flag %q", a)
		case path == "":
			path = a
		default:
			return usageErr(stderr, "log: unexpected argument %q", a)
		}
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	// The RPC filters by repo-absolute anchor path (docs/02 §10); a subtree
	// workspace defaults to its own directory's history.
	query := ""
	switch {
	case path != "" && ws.cfg.Sub != "":
		query = strings.TrimSuffix(ws.cfg.Sub, "/") + "/" + path
	case path != "":
		query = "/" + path
	case ws.cfg.Sub != "":
		query = ws.cfg.Sub
	}
	entries, err := newClient(ws.cfg.Server).log(ws.cfg.Repo, query, limit)
	if err != nil {
		return cmdErr(stderr, err)
	}
	for _, e := range entries {
		fmt.Fprintf(stdout, "%s %s\n", short(e.OID), e.Subject)
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "(no history)")
	}
	return 0
}

// --- cat: rev:path → stdout (HEAD resolves locally from the cache) ---

func runCat(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return usageErr(stderr, "cat: exactly one argument ([rev:]<path>)")
	}
	rev, path := "HEAD", args[0]
	if i := strings.Index(args[0], ":"); i >= 0 {
		rev, path = args[0][:i], args[0][i+1:]
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	c := newClient(ws.cfg.Server)
	data, err := catLocal(ws, c, rev, path)
	if err != nil {
		// Fall back to the server-side resolve for non-HEAD revs or cache
		// misses. The API speaks branch names, not the "HEAD" alias.
		revAPI := rev
		if rev == "HEAD" || rev == "" {
			revAPI = ws.cfg.Branch
		}
		var out struct {
			Type string `json:"type"`
			OID  string `json:"oid"`
		}
		if jerr := c.post("/api/v1/resolve", map[string]any{
			"repo": ws.cfg.Repo, "sub": ws.cfg.Sub, "rev": revAPI, "path": path,
		}, &out); jerr != nil {
			return cmdErr(stderr, jerr)
		}
		data, err = c.getObject(ws.cfg.Repo, out.OID)
		if err != nil {
			return cmdErr(stderr, err)
		}
	}
	_, _ = stdout.Write(data)
	return 0
}

// catLocal resolves HEAD:path through the cached manifest tree; any miss
// (rev != HEAD, evicted tree) yields an error and the caller falls back to RPC.
func catLocal(ws *workspace, c *client, rev, path string) ([]byte, error) {
	if rev != "HEAD" && rev != "" {
		return nil, fmt.Errorf("non-HEAD rev")
	}
	cur := model.OID(ws.man.Tree)
	if cur.IsZero() {
		return nil, fmt.Errorf("empty manifest tree")
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, seg := range segs {
		raw, ok := ws.cacheGet(string(cur))
		if !ok {
			return nil, fmt.Errorf("tree %s not cached", cur)
		}
		tree, err := model.DecodeTree(raw)
		if err != nil {
			return nil, err
		}
		var found *model.TreeEntry
		for j := range tree.Entries {
			if tree.Entries[j].Name == seg {
				found = &tree.Entries[j]
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("path %q not in manifest tree", path)
		}
		if i == len(segs)-1 {
			if found.IsDir {
				return nil, fmt.Errorf("%q is a directory", path)
			}
			if data, ok := ws.cacheGet(string(found.Target)); ok {
				return data, nil
			}
			return c.getObject(ws.cfg.Repo, string(found.Target))
		}
		if !found.IsDir {
			return nil, fmt.Errorf("%q: not a directory", seg)
		}
		cur = found.Target
	}
	return nil, fmt.Errorf("empty path")
}

// --- fetch-objects: machine backfill into the cache ---

func runFetchObjects(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "fetch-objects: at least one oid required")
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	c := newClient(ws.cfg.Server)
	for _, oid := range args {
		data, err := c.getObject(ws.cfg.Repo, oid)
		if err != nil {
			return cmdErr(stderr, err)
		}
		ws.cachePut(oid, data)
	}
	fmt.Fprintf(stdout, "fetched %d objects\n", len(args))
	return 0
}

// --- branch / tag: remote refs listing ---

func runRefsList(wsRoot string, args []string, stdout, stderr io.Writer, prefix string) int {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	refs, err := newClient(ws.cfg.Server).listRefs(ws.cfg.Repo, ws.cfg.Sub)
	if err != nil {
		return cmdErr(stderr, err)
	}
	for _, r := range refs {
		if r.Name == "HEAD" {
			continue
		}
		if strings.HasPrefix(r.Name, prefix) {
			fmt.Fprintf(stdout, "%s %s\n", short(r.Head), strings.TrimPrefix(r.Name, prefix))
		}
	}
	return 0
}

// --- repo: registry listing ---

func runRepo(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "ls" {
		return usageErr(stderr, "repo: only `repo ls` is supported")
	}
	var server string
	if ws, err := loadWorkspace(wsRoot); err == nil {
		server = ws.cfg.Server
	} else if s := os.Getenv("BANYAN_SERVER"); s != "" {
		server = s
	} else {
		return usageErr(stderr, "repo ls: run inside a workspace or set BANYAN_SERVER")
	}
	var out struct {
		Repos []struct {
			ID            string `json:"id"`
			DefaultBranch string `json:"defaultBranch"`
			Algorithm     string `json:"algorithm"`
		} `json:"repos"`
	}
	if err := newClient(server).getJSON("/api/v1/repos", &out); err != nil {
		return cmdErr(stderr, err)
	}
	for _, r := range out.Repos {
		fmt.Fprintf(stdout, "%s\t%s (%s)\n", r.ID, r.DefaultBranch, r.Algorithm)
	}
	return 0
}

func short(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	return oid
}
