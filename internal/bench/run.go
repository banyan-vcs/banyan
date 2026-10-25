package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metric is one measured scenario window.
type Metric struct {
	Milliseconds int64   `json:"ms"`
	BytesOut     int64   `json:"bytesOut"` // server → client
	BytesIn      int64   `json:"bytesIn"`  // client → server
	Samples      []int64 `json:"samples,omitempty"`
	Outliers     int     `json:"outliers,omitempty"`
}

// Report is the whole run's result, keyed "<target>/<scenario>".
type Report struct {
	Profile string            `json:"profile"`
	Repo    string            `json:"repo"` // repo size summary (commits, files)
	Runs    int               `json:"runs"`
	Rows    map[string]Metric `json:"rows"`
	Env     string            `json:"env"` // go version + timestamp
}

// gitRun times one git command against a server and returns the metric
// window (bytes counted by the server-side proxy, wall time around the
// process).
func gitRun(s serverIface, dir string, args ...string) (ms int64, bytesOut, bytesIn int64, err error) {
	start := time.Now()
	if _, err = git(dir, args...); err != nil {
		return 0, 0, 0, err
	}
	ms = time.Since(start).Milliseconds()
	q, a := s.take() // q = client→server, a = server→client
	bytesOut, bytesIn = a, q
	if os.Getenv("BANYAN_BENCH_DEBUG") != "" {
		fmt.Printf("  [debug] git %v → resp=%d req=%d %dms\n", args[:min(3, len(args))], bytesOut, bytesIn, ms)
	}
	return ms, bytesOut, bytesIn, nil
}

// Run executes the full comparison for one profile and returns the report.
//
// Temperature lanes (docs/11 §3, the scion method's honest reporting):
//   - cold:  fresh banyan process, first client request after the seed push
//     (weave/views/locations all cold) — a restarted deployment's first hit;
//   - warm:  same server, subsequent identical operations (medians of the
//     non-first runs) — caches holding steady state;
//   - incremental: a client that already holds the history fetching one new
//     commit (have subtraction / negotiation efficiency).
//
// Beyond the transport lanes this harness carries the workflow lanes
// (scion's bench-real shapes): dev-loop (steady-state commit+push+fetch
// rounds) and burst writers (concurrent pushers contending on one branch —
// git's client-side non-FF refusal vs banyan's server-side convergence).
//
// The git baseline runs the same scenarios against `git http-backend` over
// the identical repo and transport; its per-request CGI model has no
// in-process warm state, so its "warm" rows reflect OS caching only — the
// report says so rather than hiding the asymmetry.
func Run(profileName string, p SeedProfile, runs int, workdir string) (*Report, error) {
	if runs < 3 {
		runs = 3 // cold + at least two warm samples for a spread
	}
	seed := filepath.Join(workdir, "seed")
	bare := filepath.Join(workdir, "gitroot", "seed.git")
	if err := SeedRepo(seed, bare, p, 42); err != nil {
		return nil, err
	}
	// A scratch clone of the seed produces the per-round increment commits.
	scratch := filepath.Join(workdir, "scratch")
	if _, err := git("", "clone", "-q", "--no-local", bare, scratch); err != nil {
		return nil, err
	}

	rep := &Report{
		Profile: profileName,
		Repo:    fmt.Sprintf("%d commits, %d dirs x %d files", p.Commits, p.Dirs, p.FilesPerDir),
		Runs:    runs,
		Rows:    map[string]Metric{},
		Env:     fmt.Sprintf("%s", time.Now().Format(time.RFC3339)),
	}

	// --- banyan target ---
	banyanSrv, err := StartBanyan(filepath.Join(workdir, "banyan-shards"))
	if err != nil {
		return nil, err
	}
	defer banyanSrv.Stop()
	banyan := banyanAdapter{banyanSrv}
	banyanRepo := banyan.RepoURL() + "/r/seed.git"
	banyanSub := banyan.RepoURL() + "/r/seed.git/tree/pkg01"
	// Seed push (unmeasured — same objects the git baseline serves).
	if _, err := git(seed, "push", "-q", banyanRepo, "main"); err != nil {
		return nil, err
	}
	banyan.take() // discard the seed push — the cold lane starts clean
	if err := runScenarios(rep, "banyan", banyan, banyanRepo, banyanSub, scratch, filepath.Join(workdir, "clients-b"), runs); err != nil {
		return nil, err
	}

	// --- git baseline target ---
	gitSrv, err := StartGitCGI(filepath.Join(workdir, "gitroot"))
	if err != nil {
		return nil, err
	}
	defer gitSrv.Stop()
	gitBase := gitAdapter{gitSrv}
	gitRepo := gitBase.RepoURL() + "/seed.git"
	if err := runScenarios(rep, "git", gitBase, gitRepo, "", scratch, filepath.Join(workdir, "clients-g"), runs); err != nil {
		return nil, err
	}
	return rep, nil
}

// runScenarios measures every scenario for one target. dirBase holds the
// per-scenario client clones; subURL empty skips the subtree scenarios
// (stock git has no subtree clone — the full clone is its nearest baseline,
// and the sparse-checkout variant is measured separately).
func runScenarios(rep *Report, target string, s serverIface, repoURL, subURL, scratch, dirBase string, runs int) error {
	store := func(row string, set *sampleSet) {
		if !set.empty() {
			rep.Rows[target+"/"+row] = set.summarize()
		}
	}

	// clone-full: run 0 is the cold lane, the rest warm (fresh client each
	// time; the server keeps its state).
	var cold sampleSet
	warm := &sampleSet{}
	for i := 0; i < runs; i++ {
		dst := filepath.Join(dirBase, fmt.Sprintf("clone-%d", i))
		ms, out, in, err := gitRun(s, "", "clone", "-q", repoURL, dst)
		if err != nil {
			return err
		}
		if i == 0 {
			cold.add(ms, out, in)
		} else {
			warm.add(ms, out, in)
		}
	}
	store("clone-full-cold", &cold)
	store("clone-full-warm", warm)

	// ls-remote (advertisement cost — for banyan this includes the woven
	// root view resolution).
	ls := &sampleSet{}
	for i := 0; i < runs; i++ {
		ms, out, in, err := gitRun(s, "", "ls-remote", repoURL)
		if err != nil {
			return err
		}
		ls.add(ms, out, in)
	}
	store("ls-remote", ls)

	// fetch-incremental: one client holds the history; each round pushes one
	// new commit (unmeasured) and fetches it back.
	fetches := &sampleSet{}
	fetchClient := filepath.Join(dirBase, "fetch-client")
	if _, err := git("", "clone", "-q", repoURL, fetchClient); err != nil {
		return err
	}
	for i := 0; i < runs; i++ {
		if err := IncrementCommit(scratch, target, i); err != nil {
			return err
		}
		if _, err := git(scratch, "push", "-q", repoURL, "main"); err != nil {
			return err
		}
		s.take() // discard the push's bytes
		ms, out, in, err := gitRun(s, fetchClient, "fetch", "-q", "origin")
		if err != nil {
			return err
		}
		fetches.add(ms, out, in)
	}
	store("fetch-incremental", fetches)

	// push: fresh client commits one unique change and pushes it.
	pushes := &sampleSet{}
	for i := 0; i < runs; i++ {
		dst := filepath.Join(dirBase, fmt.Sprintf("push-client-%d", i))
		if _, err := git("", "clone", "-q", repoURL, dst); err != nil {
			return err
		}
		s.take() // discard the setup clone's bytes — only the push is measured
		if err := IncrementCommit(dst, target, 100+i); err != nil {
			return err
		}
		ms, out, in, err := gitRun(s, dst, "push", "-q", repoURL, "main")
		if err != nil {
			return err
		}
		pushes.add(ms, out, in)
	}
	store("push-one-commit", pushes)

	// clone-shallow (depth=1): both servers implement deepen.
	shallow := &sampleSet{}
	for i := 0; i < runs; i++ {
		dst := filepath.Join(dirBase, fmt.Sprintf("shallow-%d", i))
		ms, out, in, err := gitRun(s, "", "clone", "-q", "--depth", "1", repoURL, dst)
		if err != nil {
			return err
		}
		shallow.add(ms, out, in)
	}
	store("clone-shallow", shallow)

	// clone-partial (blob:none): both servers advertise the filter.
	partial := &sampleSet{}
	for i := 0; i < runs; i++ {
		dst := filepath.Join(dirBase, fmt.Sprintf("partial-%d", i))
		ms, out, in, err := gitRun(s, "", "clone", "-q", "--filter=blob:none", "--no-checkout", repoURL, dst)
		if err != nil {
			return err
		}
		partial.add(ms, out, in)
	}
	store("clone-partial", partial)

	// dev-loop: one client, steady-state rounds of edit+commit+push+fetch —
	// the collaborative heartbeat (scion's "land"), each round a sample.
	loop := &sampleSet{}
	loopClient := filepath.Join(dirBase, "dev-loop")
	if _, err := git("", "clone", "-q", repoURL, loopClient); err != nil {
		return err
	}
	s.take()
	for i := 0; i < runs; i++ {
		if err := IncrementCommit(loopClient, target, 200+i); err != nil {
			return err
		}
		start := time.Now()
		if _, err := git(loopClient, "push", "-q", "origin", "main"); err != nil {
			return err
		}
		if _, err := git(loopClient, "fetch", "-q", "origin"); err != nil {
			return err
		}
		ms := time.Since(start).Milliseconds()
		q, a := s.take() // q = client→server, a = server→client
		loop.add(ms, a, q)
	}
	store("dev-loop", loop)

	// burst-writers: W clients (cloned outside the timer) each edit a
	// DIFFERENT file and push at once. git's client-side non-FF refusal makes
	// losers pull --rebase and push again; banyan's convergence train lands
	// them server-side. Record total (first start → last finish) plus the
	// writers' own p50/p99 (scion's land-N-writers shape).
	const writers = 8
	totals := &sampleSet{}
	p50s := &sampleSet{}
	p99s := &sampleSet{}
	for round := 0; round < max(2, runs-1); round++ {
		dirs := make([]string, writers)
		for w := 0; w < writers; w++ {
			d := filepath.Join(dirBase, fmt.Sprintf("burst-%d-%d", round, w))
			if _, err := git("", "clone", "-q", repoURL, d); err != nil {
				return err
			}
			rel := fmt.Sprintf("pkg%02d/writer%02d.txt", w%5, w)
			full := filepath.Join(d, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(fmt.Sprintf("writer %d round %d\n%s\n", w, round, strings.Repeat("burst ", 64))), 0o644); err != nil {
				return err
			}
			if _, err := git(d, "add", "-A"); err != nil {
				return err
			}
			if _, err := git(d, "commit", "-qm", fmt.Sprintf("burst writer %d round %d", w, round)); err != nil {
				return err
			}
			dirs[w] = d
		}
		s.take()
		total, each := runBurst(s, dirs, repoURL)
		q, a := s.take() // q = client→server, a = server→client
		totals.add(total, a, q)
		p50s.add(pctl(each, 0.50), 0, 0)
		p99s.add(pctl(each, 0.99), 0, 0)
	}
	store("burst-writers-8-total", totals)
	store("burst-writers-8-p50", p50s)
	store("burst-writers-8-p99", p99s)

	if subURL != "" {
		// subtree clone (banyan structural capability): download equals the
		// subtree's own history closure, not the monorepo's.
		subs := &sampleSet{}
		for i := 0; i < runs; i++ {
			dst := filepath.Join(dirBase, fmt.Sprintf("sub-%d", i))
			ms, out, in, err := gitRun(s, "", "clone", "-q", subURL, dst)
			if err != nil {
				return err
			}
			subs.add(ms, out, in)
		}
		store("clone-subtree", subs)

		// subtree push: a subtree-view client commits and pushes — the
		// directory-granular write path (no git counterpart; the full-repo
		// push row is the nearest baseline).
		subPush := &sampleSet{}
		for i := 0; i < runs; i++ {
			dst := filepath.Join(dirBase, fmt.Sprintf("sub-push-%d", i))
			if _, err := git("", "clone", "-q", subURL, dst); err != nil {
				return err
			}
			s.take()
			if err := IncrementCommit(dst, target, 300+i); err != nil {
				return err
			}
			ms, out, in, err := gitRun(s, dst, "push", "-q", "origin", "main")
			if err != nil {
				return err
			}
			subPush.add(ms, out, in)
		}
		store("push-subtree", subPush)
	}
	return nil
}

// runBurst fires all writers' pushes simultaneously and returns the total
// wall time (first start → last finish) plus each writer's own duration.
// A refused push (git non-FF under contention) pulls --rebase and retries —
// the documented git-side recovery, not a failure.
func runBurst(s serverIface, dirs []string, repoURL string) (total int64, each []int64) {
	start := time.Now()
	var wg sync.WaitGroup
	each = make([]int64, len(dirs))
	for i, d := range dirs {
		wg.Add(1)
		go func(i int, d string) {
			defer wg.Done()
			ws := time.Now()
			for attempt := 0; attempt < 10; attempt++ {
				if _, err := git(d, "push", "-q", "origin", "main"); err == nil {
					break
				}
				if _, err := git(d, "pull", "-q", "--rebase", "origin", "main"); err != nil {
					continue // contention on the rebase too; retry the push
				}
				_, _ = git(d, "push", "-q", "origin", "main")
			}
			each[i] = time.Since(ws).Milliseconds()
		}(i, d)
	}
	wg.Wait()
	return time.Since(start).Milliseconds(), each
}

// Render prints the comparison table (banyan vs git ratios).
func (r *Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "banyan-bench  profile=%s  repo=%s  runs=%d\n", r.Profile, r.Repo, r.Runs)
	fmt.Fprintf(&b, "%-34s %18s %18s %10s %10s\n", "scenario", "banyan", "git", "bytes", "ms")
	for _, n := range r.orderedRows() {
		bm, bok := r.Rows["banyan/"+n]
		gm, gok := r.Rows["git/"+n]
		if !bok {
			fmt.Fprintf(&b, "%-34s %18s %18s\n", n, "-", fmtMetric(gm))
			continue
		}
		if !gok {
			fmt.Fprintf(&b, "%-34s %18s %18s\n", n, fmtMetric(bm), "-(git 无)")
			continue
		}
		br := ratio(bm.BytesOut, gm.BytesOut)
		mr := ratio(bm.Milliseconds, gm.Milliseconds)
		fmt.Fprintf(&b, "%-34s %18s %18s %10s %10s\n", n, fmtMetric(bm), fmtMetric(gm), br, mr)
	}
	return b.String()
}

func fmtMetric(m Metric) string {
	suffix := ""
	if m.Outliers > 0 {
		suffix = fmt.Sprintf(" (+%d)", m.Outliers)
	}
	return fmt.Sprintf("%7.1fKB %4dms%s", float64(m.BytesOut)/1024, m.Milliseconds, suffix)
}

func ratio(a, b int64) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fx", float64(a)/float64(b))
}

// Save writes the report as a baseline file.
func (r *Report) Save(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// WriteMarkdown writes the Markdown report artifact.
func (r *Report) WriteMarkdown(path string) error {
	return os.WriteFile(path, []byte(r.RenderMarkdown()), 0o644)
}

// WriteCSV writes the raw-sample CSV artifact.
func (r *Report) WriteCSV(path string) error {
	return os.WriteFile(path, []byte(r.RenderCSV()), 0o644)
}

// regressionLimit is the red line (docs/11 §3): any row regressing beyond
// this factor against the saved baseline fails the gate.
const regressionLimit = 1.2

// Check compares the report against a saved baseline; returns the offending
// rows (empty = green).
func (r *Report) Check(baseline *Report) []string {
	var bad []string
	keys := make([]string, 0, len(baseline.Rows))
	for k := range baseline.Rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want, ok := baseline.Rows[k]
		if !ok {
			continue
		}
		got, ok := r.Rows[k]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s: 场景缺失（基线有）", k))
			continue
		}
		if float64(got.Milliseconds) > float64(want.Milliseconds)*regressionLimit {
			bad = append(bad, fmt.Sprintf("%s: 耗时 %dms → %dms（>%.0f%%）", k, want.Milliseconds, got.Milliseconds, (regressionLimit-1)*100))
		}
		if want.BytesOut > 0 && float64(got.BytesOut) > float64(want.BytesOut)*regressionLimit {
			bad = append(bad, fmt.Sprintf("%s: 字节 %d → %d（>%.0f%%）", k, want.BytesOut, got.BytesOut, (regressionLimit-1)*100))
		}
	}
	return bad
}

// ExecPath exposes git's exec dir (diagnostics).
func ExecPath() (string, error) {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
