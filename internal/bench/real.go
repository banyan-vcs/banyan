// real.go — the real-repository sample mode (docs/operations/performance.md
// §3b). The synthetic profiles stay deterministic and fast, but they are
// single-branch, text-only, and blob-size uniform; real history — binary
// blobs, merge topology, hundreds of tags, delta chains decades deep —
// exercises negotiation, closure, and packing paths the seeds cannot.
// RunReal mirrors a local repository into both targets (git http-backend
// serves the mirror directly; banyan receives the same objects via git push)
// and runs the identical scenario matrix against it.
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Options parameterizes one benchmark run.
type Options struct {
	Runs      int
	Workdir   string
	Scenarios map[string]bool // family filter (parseScenarios); nil = all
}

// RunReal executes the full comparison with a real repository as the sample.
// src is a local path (or URL) to clone --mirror from; the source is never
// mutated — everything lands in the workdir. name labels the report.
func RunReal(name, src string, o Options) (*Report, error) {
	if o.Runs < 3 {
		o.Runs = 3
	}
	rr, err := materialize(src, o.Workdir)
	if err != nil {
		return nil, err
	}
	dirFor := func(w int) string { return rr.dirs[w%len(rr.dirs)] }
	env := scenarioEnv{
		branch: rr.branch,
		subDir: rr.subDir,
		summary: fmt.Sprintf("%s: %d commits, %d refs, %.1f MiB packs, branch %q",
			name, rr.commits, rr.refs, rr.packMiB, rr.branch),
		burstPath: func(w int) string {
			return fmt.Sprintf("%s/_bench-writer%02d.txt", dirFor(w), w)
		},
	}
	// Real repos push every branch and tag — the advertisement parity the
	// git baseline has for free (it serves the mirror verbatim). scratch
	// stays empty: each target's increment base is cloned from that target
	// (banyan advertises the woven view, whose tips a mirror clone lacks).
	pushArgs := []string{"refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"}
	return runTargets(name, rr.bare, pushArgs, "", o.Workdir, env, o)
}

// materialize mirrors src into workdir/gitroot/seed.git (what the git CGI
// baseline serves) and derives the shape facts the scenarios need.
func materialize(src, workdir string) (*realRepo, error) {
	bare := filepath.Join(workdir, "gitroot", "seed.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		return nil, err
	}
	// --no-local: no hardlinks into the sample source — the bench copies are
	// independent (and behavior does not depend on filesystem hardlink
	// support). The mirror keeps every ref (heads, tags, notes) and HEAD.
	if _, err := git("", "clone", "-q", "--mirror", "--no-local", src, bare); err != nil {
		return nil, err
	}
	if _, err := git(bare, "config", "http.receivepack", "true"); err != nil {
		return nil, err
	}
	rr := &realRepo{bare: bare, branch: "master"}
	if out, err := git(bare, "symbolic-ref", "--short", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" {
			rr.branch = b
		}
	}
	// Shape stats for the report: history depth, ref breadth, pack bytes.
	if out, err := git(bare, "rev-list", "--count", rr.branch); err == nil {
		rr.commits, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if out, err := git(bare, "count-objects", "-v"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if v, ok := strings.CutPrefix(line, "size-pack: "); ok {
				if kb, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					rr.packMiB = kb / 1024
				}
			}
		}
	}
	if out, err := git(bare, "ls-remote", "."); err == nil {
		rr.refs = len(strings.Split(strings.TrimSpace(out), "\n"))
		if strings.TrimSpace(out) == "" {
			rr.refs = 0
		}
	}
	// Top-level directories by file count: the subtree lane rides the
	// biggest one (a meaningful closure), burst writers spread across them
	// (the cross-directory concurrency the convergence train lives on).
	counts := map[string]int{}
	if out, err := git(bare, "ls-tree", "-r", "--name-only", rr.branch); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if top, _, found := strings.Cut(line, "/"); found {
				counts[top]++
			}
		}
	}
	var tops []string
	for d := range counts {
		tops = append(tops, d)
	}
	sort.Slice(tops, func(i, j int) bool {
		if counts[tops[i]] != counts[tops[j]] {
			return counts[tops[i]] > counts[tops[j]]
		}
		return tops[i] < tops[j]
	})
	if len(tops) > 8 {
		tops = tops[:8]
	}
	rr.dirs = tops
	if len(tops) > 0 {
		rr.subDir = tops[0]
	}
	if rr.subDir == "" {
		return nil, fmt.Errorf("sample %s has no top-level directories — the subtree lane needs one", src)
	}
	return rr, nil
}

type realRepo struct {
	bare    string
	branch  string   // advertised default branch (the mirror's HEAD symref)
	subDir  string   // largest top-level directory (subtree lane)
	dirs    []string // top dirs by file count (burst writer spread)
	commits int
	refs    int
	packMiB float64
}
