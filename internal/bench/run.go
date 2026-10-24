package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Metric is one measured scenario window.
type Metric struct {
	Milliseconds int64 `json:"ms"`
	BytesOut     int64 `json:"bytesOut"` // server → client
	BytesIn      int64 `json:"bytesIn"`  // client → server
}

// Report is the whole run's result, keyed "<target>/<scenario>".
type Report struct {
	Profile string            `json:"profile"`
	Repo    string            `json:"repo"` // repo size summary (commits, files)
	Rows    map[string]Metric `json:"rows"`
	Env     string            `json:"env"` // go version + timestamp
}

// gitRun times one git command against a server and returns the metric
// window (bytes counted by the server-side proxy, wall time around the
// process).
func gitRun(s serverIface, dir string, args ...string) (Metric, error) {
	start := time.Now()
	if _, err := git(dir, args...); err != nil {
		return Metric{}, err
	}
	ms := time.Since(start).Milliseconds()
	req, resp := s.take()
	if os.Getenv("BANYAN_BENCH_DEBUG") != "" {
		fmt.Printf("  [debug] git %v → req=%d resp=%d %dms\n", args[:min(3, len(args))], req, resp, ms)
	}
	return Metric{Milliseconds: ms, BytesOut: resp, BytesIn: req}, nil
}

// Run executes the full comparison for one profile and returns the report.
//
// Temperature lanes (docs/12 §3, the scion method's honest reporting):
//   - cold:  fresh banyan process, first client request after the seed push
//     (weave/views/locations all cold) — a restarted deployment's first hit;
//   - warm:  same server, subsequent identical operations (medians of the
//     non-first runs) — caches holding steady state;
//   - incremental: a client that already holds the history fetching one new
//     commit (have subtraction / negotiation efficiency).
//
// The git baseline runs the same scenarios against `git http-backend` over
// the identical repo and transport; its per-request CGI model has no
// in-process warm state, so its "warm" rows reflect OS caching only — the
// report says so rather than hiding the asymmetry.
func Run(profileName string, p SeedProfile, runs int, workdir string) (*Report, error) {
	if runs < 2 {
		runs = 2 // cold + at least one warm sample
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
	newClient := func(name string, round int) string {
		return filepath.Join(dirBase, fmt.Sprintf("%s-%d", name, round))
	}

	// clone-full: run 0 is the cold lane, the rest warm (fresh client each
	// time; the server keeps its state).
	var warm []Metric
	for i := 0; i < runs; i++ {
		dst := newClient("clone", i)
		m, err := gitRun(s, "", "clone", "-q", repoURL, dst)
		if err != nil {
			return err
		}
		if i == 0 {
			rep.Rows[target+"/clone-full-cold"] = m
		} else {
			warm = append(warm, m)
		}
	}
	rep.Rows[target+"/clone-full-warm"] = median(warm)

	// ls-remote (advertisement cost — for banyan this includes the woven
	// root view resolution).
	ls := make([]Metric, 0, runs)
	for i := 0; i < runs; i++ {
		m, err := gitRun(s, "", "ls-remote", repoURL)
		if err != nil {
			return err
		}
		ls = append(ls, m)
	}
	rep.Rows[target+"/ls-remote"] = median(ls)

	// fetch-incremental: one client holds the history; each round pushes one
	// new commit (unmeasured) and fetches it back.
	fetches := make([]Metric, 0, runs)
	fetchClient := newClient("fetch-client", 0)
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
		m, err := gitRun(s, fetchClient, "fetch", "-q", "origin")
		if err != nil {
			return err
		}
		fetches = append(fetches, m)
	}
	rep.Rows[target+"/fetch-incremental"] = median(fetches)

	// push: fresh client commits one unique change and pushes it.
	var pushes []Metric
	for i := 0; i < runs; i++ {
		dst := newClient("push-client", i)
		if _, err := git("", "clone", "-q", repoURL, dst); err != nil {
			return err
		}
		s.take() // discard the setup clone's bytes — only the push is measured
		if err := IncrementCommit(dst, target, 100+i); err != nil {
			return err
		}
		m, err := gitRun(s, dst, "push", "-q", repoURL, "main")
		if err != nil {
			return err
		}
		pushes = append(pushes, m)
	}
	rep.Rows[target+"/push-one-commit"] = median(pushes)

	if subURL != "" {
		// subtree clone (banyan structural capability): download equals the
		// subtree's own history closure, not the monorepo's.
		var subs []Metric
		for i := 0; i < runs; i++ {
			m, err := gitRun(s, "", "clone", "-q", subURL, newClient("sub", i))
			if err != nil {
				return err
			}
			subs = append(subs, m)
		}
		rep.Rows[target+"/clone-subtree"] = median(subs)
	}
	return nil
}

// median picks the median wall time; bytes are means (per-run byte counts
// are deterministic, but averaging keeps one code path for all metrics).
func median(ms []Metric) Metric {
	if len(ms) == 0 {
		return Metric{}
	}
	sorted := append([]Metric(nil), ms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Milliseconds < sorted[j].Milliseconds })
	var out Metric
	out.Milliseconds = sorted[len(sorted)/2].Milliseconds
	for _, x := range sorted {
		out.BytesOut += x.BytesOut
		out.BytesIn += x.BytesIn
	}
	out.BytesOut /= int64(len(sorted))
	out.BytesIn /= int64(len(sorted))
	return out
}

// Render prints the comparison table (banyan vs git ratios).
func (r *Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "banyan-bench  profile=%s  repo=%s\n", r.Profile, r.Repo)
	fmt.Fprintf(&b, "%-34s %12s %12s %10s %10s\n", "scenario", "banyan", "git", "bytes ratio", "ms ratio")
	scenarios := map[string]bool{}
	for k := range r.Rows {
		scenarios[strings.TrimPrefix(strings.TrimPrefix(k, "banyan/"), "git/")] = true
	}
	names := make([]string, 0, len(scenarios))
	for n := range scenarios {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		bm, bok := r.Rows["banyan/"+n]
		gm, gok := r.Rows["git/"+n]
		if !bok {
			fmt.Fprintf(&b, "%-34s %12s %12s\n", n, "-", fmtMetric(gm))
			continue
		}
		if !gok {
			fmt.Fprintf(&b, "%-34s %12s %12s\n", n, fmtMetric(bm), "-(git 无对应能力)")
			continue
		}
		br := ratio(bm.BytesOut, gm.BytesOut)
		mr := ratio(bm.Milliseconds, gm.Milliseconds)
		fmt.Fprintf(&b, "%-34s %12s %12s %10s %10s\n", n, fmtMetric(bm), fmtMetric(gm), br, mr)
	}
	return b.String()
}

func fmtMetric(m Metric) string {
	return fmt.Sprintf("%6.1fKB %4dms", float64(m.BytesOut)/1024, m.Milliseconds)
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

// regressionLimit is the red line (docs/12 §3): any row regressing beyond
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
