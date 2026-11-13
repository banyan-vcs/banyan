// stack.go — banyan stack: a local queue of path-scoped changesets (docs/client/cli.md).
// push captures the selected dirty paths as one entry (bytes land in the
// content-addressed .banyan/stack-blobs/, independent of the prunable cache)
// and reverts them locally; commit replays entries oldest-first through the
// ordinary /api/v1/commit changeset assembly, so a moved base is just
// "pull, then retry" — path-level replay adapts to whatever head the server
// holds. Entries survive conflicts untouched; only landed entries are
// consumed. Untracked files are captured too (banyan has no ignore list —
// the same scan surface status/snapshot use).
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

const (
	stackFile  = ".banyan/stack.json"
	stackBlobs = ".banyan/stack-blobs"
)

// stackChange is one path in an entry: a blob pointer (content-addressed in
// stack-blobs/) or a deletion.
type stackChange struct {
	Path   string `json:"path"`
	Blob   string `json:"blob,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

type stackEntry struct {
	ID      int           `json:"id"`
	Message string        `json:"message"`
	Created string        `json:"created"`
	Base    string        `json:"base"` // manifest commit at push time (display; commit chains the live head)
	Changes []stackChange `json:"changes"`
}

type stackDoc struct {
	Next    int          `json:"next"`
	Entries []stackEntry `json:"entries"`
}

func (d *stackDoc) byID(id int) int {
	for i := range d.Entries {
		if d.Entries[i].ID == id {
			return i
		}
	}
	return -1
}

func loadStack(root string) (*stackDoc, error) {
	d := &stackDoc{Next: 1}
	raw, err := os.ReadFile(filepath.Join(root, stackFile))
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, d); err != nil {
		return nil, fmt.Errorf("read %s: %w", stackFile, err)
	}
	if d.Next < 1 {
		d.Next = 1
	}
	return d, nil
}

func (d *stackDoc) save(root string) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(root, stackFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o644)
}

// gcStackBlobs drops blob files no entry references anymore.
func (d *stackDoc) gcStackBlobs(root string) {
	live := map[string]bool{}
	for _, e := range d.Entries {
		for _, c := range e.Changes {
			if c.Blob != "" {
				live[c.Blob] = true
			}
		}
	}
	dir := filepath.Join(root, stackBlobs)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && !live[e.Name()] {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func stackBlobPath(root, oid string) string {
	return filepath.Join(root, stackBlobs, oid)
}

// runStack dispatches the stack subcommands.
func runStack(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageErr(stderr, "stack: subcommand required (push | ls | pop | drop | commit)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "push":
		return stackPush(wsRoot, rest, stdout, stderr)
	case "ls":
		return stackList(wsRoot, stdout, stderr)
	case "pop":
		return stackPop(wsRoot, stdout, stderr)
	case "drop":
		return stackDrop(wsRoot, rest, stdout, stderr)
	case "commit":
		return stackCommit(wsRoot, rest, stdout, stderr)
	default:
		return usageErr(stderr, "stack: unknown subcommand %q (push | ls | pop | drop | commit)", sub)
	}
}

// stackPush captures the selected dirty paths as one entry and reverts them
// locally. Path args match exactly or by directory prefix; with no args the
// whole dirty set (modified + deleted + untracked) is captured.
func stackPush(wsRoot string, args []string, stdout, stderr io.Writer) int {
	var message string
	var paths []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" && i+1 < len(args):
			i++
			message = args[i]
		case strings.HasPrefix(a, "-"):
			return usageErr(stderr, "stack push: unknown flag %q", a)
		default:
			paths = append(paths, a)
		}
	}
	if message == "" {
		return usageErr(stderr, "stack push: -m <message> required")
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	st, err := ws.scan()
	if err != nil {
		return cmdErr(stderr, err)
	}
	selects := func(rel string) bool {
		if len(paths) == 0 {
			return true
		}
		for _, p := range paths {
			p = strings.TrimSuffix(filepath.ToSlash(p), "/")
			if rel == p || strings.HasPrefix(rel, p+"/") {
				return true
			}
		}
		return false
	}
	dirty := map[string]string{} // rel → M | D | ?
	for _, p := range st.Modified {
		dirty[p] = "M"
	}
	for _, p := range st.Deleted {
		dirty[p] = "D"
	}
	for _, p := range st.Untracked {
		dirty[p] = "?"
	}
	var rels []string
	for rel := range dirty {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var selected, unmatched []string
	for _, rel := range rels {
		if selects(rel) {
			selected = append(selected, rel)
		}
	}
	if len(paths) > 0 {
		for _, p := range paths {
			p = strings.TrimSuffix(filepath.ToSlash(p), "/")
			hit := false
			for _, rel := range selected {
				if rel == p || strings.HasPrefix(rel, p+"/") {
					hit = true
					break
				}
			}
			if !hit {
				unmatched = append(unmatched, p)
			}
		}
		if len(unmatched) > 0 {
			return usageErr(stderr, "stack push: no dirty change matches %q (status first)", strings.Join(unmatched, ", "))
		}
	}
	if len(selected) == 0 {
		fmt.Fprintln(stdout, "nothing to stack, working tree clean")
		return 0
	}

	doc, err := loadStack(ws.root)
	if err != nil {
		return cmdErr(stderr, err)
	}
	entry := stackEntry{
		ID:      doc.Next,
		Message: message,
		Created: time.Now().Format(time.RFC3339),
		Base:    ws.man.Commit,
	}
	nDelete := 0
	for _, rel := range selected {
		switch dirty[rel] {
		case "D":
			entry.Changes = append(entry.Changes, stackChange{Path: rel, Delete: true})
			nDelete++
		case "M":
			data, rerr := os.ReadFile(filepath.Join(ws.root, filepath.FromSlash(rel)))
			if rerr != nil {
				return cmdErr(stderr, rerr)
			}
			ch, cerr := stackCapture(ws.root, rel, data, ws.man.Files[rel].Mode)
			if cerr != nil {
				return cmdErr(stderr, cerr)
			}
			entry.Changes = append(entry.Changes, *ch)
		case "?":
			full := filepath.Join(ws.root, filepath.FromSlash(rel))
			data, rerr := os.ReadFile(full)
			if rerr != nil {
				return cmdErr(stderr, rerr)
			}
			mode := "100644"
			if info, serr := os.Stat(full); serr == nil && info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			ch, cerr := stackCapture(ws.root, rel, data, mode)
			if cerr != nil {
				return cmdErr(stderr, cerr)
			}
			entry.Changes = append(entry.Changes, *ch)
		}
	}

	// Revert the captured paths locally: tracked content comes back from the
	// manifest OID (cache first, server backfill on a pruned cache — the
	// same source apply uses); untracked files are removed (their bytes are
	// now safe in the stack).
	var client *client
	for _, rel := range selected {
		if dirty[rel] == "?" {
			_ = os.Remove(filepath.Join(ws.root, filepath.FromSlash(rel)))
			continue
		}
		fs, ok := ws.man.Files[rel]
		if !ok {
			continue
		}
		var data []byte
		if cached, gerr := ws.cacheGet(fs.OID); gerr {
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
		if err := writeFileState(ws.root, rel, data, fs.Mode); err != nil {
			return cmdErr(stderr, err)
		}
	}

	doc.Next++
	doc.Entries = append(doc.Entries, entry)
	if err := doc.save(ws.root); err != nil {
		return cmdErr(stderr, err)
	}
	files := len(entry.Changes) - nDelete
	fmt.Fprintf(stdout, "stacked #%d %q (%d files, %d deletions) — paths reverted locally\n",
		entry.ID, message, files, nDelete)
	return 0
}

// stackCapture stores one blob in stack-blobs/ and returns the change record.
func stackCapture(root, rel string, data []byte, mode string) (*stackChange, error) {
	oid := blobHash(data)
	if mode == "" {
		mode = "100644"
	}
	p := stackBlobPath(root, oid)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return nil, err
	}
	return &stackChange{Path: rel, Blob: oid, Mode: mode}, nil
}

func stackList(wsRoot string, stdout, stderr io.Writer) int {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	doc, err := loadStack(ws.root)
	if err != nil {
		return cmdErr(stderr, err)
	}
	if len(doc.Entries) == 0 {
		fmt.Fprintln(stdout, "stack empty")
		return 0
	}
	for _, e := range doc.Entries {
		fmt.Fprintf(stdout, "#%d\t%s\t%d changes\t%s\n", e.ID, e.Created, len(e.Changes), e.Message)
	}
	fmt.Fprintln(stdout, "(oldest first — `banyan stack commit` lands in this order)")
	return 0
}

// stackPop re-applies the top entry to the working tree (no server contact)
// and removes it. Refuses when any affected path has local changes.
func stackPop(wsRoot string, stdout, stderr io.Writer) int {
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	doc, err := loadStack(ws.root)
	if err != nil {
		return cmdErr(stderr, err)
	}
	if len(doc.Entries) == 0 {
		fmt.Fprintln(stdout, "stack empty")
		return 0
	}
	top := doc.Entries[len(doc.Entries)-1]
	st, err := ws.scan()
	if err != nil {
		return cmdErr(stderr, err)
	}
	dirtySet := map[string]bool{}
	for _, l := range [][]string{st.Modified, st.Deleted, st.Untracked} {
		for _, p := range l {
			dirtySet[p] = true
		}
	}
	for _, c := range top.Changes {
		if dirtySet[c.Path] {
			fmt.Fprintf(stderr, "conflict: %s has local changes — commit or revert it first\n", c.Path)
			return 1
		}
	}
	for _, c := range top.Changes {
		if c.Delete {
			_ = os.Remove(filepath.Join(ws.root, filepath.FromSlash(c.Path)))
			continue
		}
		data, rerr := os.ReadFile(stackBlobPath(ws.root, c.Blob))
		if rerr != nil {
			return cmdErr(stderr, rerr)
		}
		if err := writeFileState(ws.root, c.Path, data, c.Mode); err != nil {
			return cmdErr(stderr, err)
		}
	}
	doc.Entries = doc.Entries[:len(doc.Entries)-1]
	if err := doc.save(ws.root); err != nil {
		return cmdErr(stderr, err)
	}
	doc.gcStackBlobs(ws.root)
	fmt.Fprintf(stdout, "popped #%d %q (%d changes)\n", top.ID, top.Message, len(top.Changes))
	return 0
}

func stackDrop(wsRoot string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return usageErr(stderr, "stack drop: <id> required")
	}
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return usageErr(stderr, "stack drop: bad id %q", args[0])
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	doc, err := loadStack(ws.root)
	if err != nil {
		return cmdErr(stderr, err)
	}
	i := doc.byID(id)
	if i < 0 {
		return usageErr(stderr, "stack drop: no entry #%d", id)
	}
	doc.Entries = append(doc.Entries[:i], doc.Entries[i+1:]...)
	if err := doc.save(ws.root); err != nil {
		return cmdErr(stderr, err)
	}
	doc.gcStackBlobs(ws.root)
	fmt.Fprintf(stdout, "dropped #%d\n", id)
	return 0
}

// stackCommit lands entries through the ordinary commit RPC, oldest first.
// The changeset is path-level replay: the server assembles onto whatever head
// it holds, so a moved base fails the first landing with CONFLICT_BASE_MOVED
// — "pull, retry" then lands everything on the fresh base. Landed entries are
// consumed; untouched ones survive the failure verbatim.
func stackCommit(wsRoot string, args []string, stdout, stderr io.Writer) int {
	wantID := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !strings.HasPrefix(a, "-") && wantID == 0:
			id, err := strconv.Atoi(a)
			if err != nil {
				return usageErr(stderr, "stack commit: bad id %q", a)
			}
			wantID = id
		default:
			return usageErr(stderr, "stack commit: unexpected argument %q", a)
		}
	}
	ws, err := loadWorkspace(wsRoot)
	if err != nil {
		return cmdErr(stderr, err)
	}
	doc, err := loadStack(ws.root)
	if err != nil {
		return cmdErr(stderr, err)
	}
	if len(doc.Entries) == 0 {
		fmt.Fprintln(stdout, "stack empty")
		return 0
	}
	var land []stackEntry
	if wantID != 0 {
		i := doc.byID(wantID)
		if i < 0 {
			return usageErr(stderr, "stack commit: no entry #%d", wantID)
		}
		land = []stackEntry{doc.Entries[i]}
	} else {
		land = append([]stackEntry(nil), doc.Entries...)
	}

	c := newClient(ws.cfg.Server)
	name, email := defaultAuthor()
	landed := 0
	flush := func() int {
		if err := doc.save(ws.root); err != nil {
			return cmdErr(stderr, err)
		}
		if err := ws.save(); err != nil {
			return cmdErr(stderr, err)
		}
		doc.gcStackBlobs(ws.root)
		return -1
	}
	for _, e := range land {
		var changes []map[string]any
		var uploads []map[string]any
		uploaded := map[string]bool{}
		blobs := map[string][]byte{}
		for _, ch := range e.Changes {
			if ch.Delete {
				changes = append(changes, map[string]any{"path": ch.Path, "delete": true})
				continue
			}
			changes = append(changes, map[string]any{"path": ch.Path, "blob": ch.Blob, "mode": ch.Mode})
			if !uploaded[ch.Blob] {
				uploaded[ch.Blob] = true
				data, rerr := os.ReadFile(stackBlobPath(ws.root, ch.Blob))
				if rerr != nil {
					return cmdErr(stderr, rerr)
				}
				blobs[ch.Blob] = data
				uploads = append(uploads, map[string]any{"oid": ch.Blob, "type": "blob", "data": data})
			}
		}
		if len(uploads) > 0 {
			if err := c.putObjects(ws.cfg.Repo, uploads); err != nil {
				return cmdErr(stderr, err)
			}
		}
		res, cerr := c.commit(ws.cfg.Repo, ws.cfg.Sub, ws.cfg.Branch, ws.man.Commit, e.Message, name, email, changes)
		if cerr != nil {
			if isConflictErr(cerr) {
				if code := flush(); code >= 0 {
					return code
				}
				fmt.Fprintln(stderr, "conflict: base moved — run `banyan pull`, then retry `banyan stack commit`")
				fmt.Fprintln(stderr, "error:", cerr)
				return 1
			}
			return cmdErr(stderr, cerr)
		}
		// Landing restores the entry to the working tree too: push reverted
		// these paths away, so the checkout and the manifest advance together
		// (the same contract runCommit applies). Local writes are best-effort
		// behind the server's truth — status surfaces any drift.
		for _, ch := range e.Changes {
			if ch.Delete {
				delete(ws.man.Files, ch.Path)
				if rerr := os.Remove(filepath.Join(ws.root, filepath.FromSlash(ch.Path))); rerr != nil && !os.IsNotExist(rerr) {
					fmt.Fprintf(stderr, "warning: remove %s: %v\n", ch.Path, rerr)
				}
				continue
			}
			if werr := writeFileState(ws.root, ch.Path, blobs[ch.Blob], ch.Mode); werr != nil {
				fmt.Fprintf(stderr, "warning: restore %s: %v\n", ch.Path, werr)
			}
			ws.man.Files[ch.Path] = fileState{OID: ch.Blob, Mode: ch.Mode}
		}
		ws.man.Commit, ws.man.Tree = res.Head, res.Tree
		if i := doc.byID(e.ID); i >= 0 {
			doc.Entries = append(doc.Entries[:i], doc.Entries[i+1:]...)
		}
		landed++
		fmt.Fprintf(stdout, "landed #%d %q → %s\n", e.ID, e.Message, short(res.Head))
	}
	if code := flush(); code >= 0 {
		return code
	}
	fmt.Fprintf(stdout, "landed %d entr%s, stack: %d left\n", landed, map[bool]string{true: "y", false: "ies"}[landed == 1], len(doc.Entries))
	return 0
}
