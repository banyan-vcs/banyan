package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	Env     string            `json:"env"` // cpu/maxprocs/go/git fingerprint + timestamp
	// Control is the host-performance thermistor: one local clone of a
	// fixed tiny repo per paired round. Not a scenario — a bias detector:
	// its drift across the run measures the MACHINE, so wide spreads
	// downgrade wall-time ratios (bytes are immune). Absorbed into
	// baselines for reference; Check ignores it.
	Control []int64 `json:"control,omitempty"`
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

// scenarioEnv carries the sample's shape into the scenario runner: which
// branch pushes ride (seed repos: main; a real sample: its own HEAD), the
// subtree lane's directory, the report line, and where burst writers write.
type scenarioEnv struct {
	branch    string             // the pushed branch name
	subDir    string             // top-level directory for the subtree lane ("" = none)
	summary   string             // Report.Repo line
	burstPath func(w int) string // burst writer w's file, repo-relative
}

// Run executes the seeded-profile comparison (the synthetic profiles).
func Run(profileName string, p SeedProfile, runs int, workdir string) (*Report, error) {
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
	env := scenarioEnv{
		branch:  "main",
		subDir:  "pkg01",
		summary: fmt.Sprintf("%d commits, %d dirs x %d files", p.Commits, p.Dirs, p.FilesPerDir),
		burstPath: func(w int) string {
			return fmt.Sprintf("pkg%02d/writer%02d.txt", w%5, w)
		},
	}
	return runTargets(profileName, seed, []string{"main"}, scratch, workdir, env, Options{Runs: runs, Workdir: workdir})
}

// runTargets runs both peers over an already-materialized sample: pushDir
// holds the objects to push into banyan (pushArgs refspecs), workdir/gitroot
// is what the git CGI baseline serves, scratch makes the increment commits.
//
// Temperature lanes (docs/operations/performance.md §3, the scion method's
// honest reporting):
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
func runTargets(profileName, pushDir string, pushArgs []string, scratch, workdir string, env scenarioEnv, o Options) (*Report, error) {
	runs := o.Runs
	if runs < 3 {
		runs = 3 // cold + at least two warm samples for a spread
	}
	rep := &Report{
		Profile: profileName,
		Repo:    env.summary,
		Runs:    runs,
		Rows:    map[string]Metric{},
		Env: fmt.Sprintf("cpu=%d maxprocs=%d go=%s git=%s %s",
			runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.Version(), gitVersion(), time.Now().Format(time.RFC3339)),
	}
	want := familyFilter(o.Scenarios)

	// --- banyan target ---
	banyanSrv, err := StartBanyan(filepath.Join(workdir, "banyan-shards"))
	if err != nil {
		return nil, err
	}
	defer banyanSrv.Stop()
	banyan := banyanAdapter{banyanSrv}
	banyanRepo := banyan.RepoURL() + "/r/seed.git"
	banyanSub := ""
	if env.subDir != "" {
		banyanSub = banyan.RepoURL() + "/r/seed.git/tree/" + env.subDir
	}
	// Seed push (unmeasured — same objects the git baseline serves).
	if _, err := git(pushDir, append([]string{"push", "-q", banyanRepo}, pushArgs...)...); err != nil {
		return nil, err
	}
	banyan.take() // discard the seed push — the cold lane starts clean
	// The push auto-creates the repo with default branch main; a real
	// sample's HEAD is its own branch (git.git says master), so re-point the
	// advertised symref before the cold lane reads it.
	if env.branch != "" && env.branch != "main" {
		if err := banyanSrv.SetDefaultBranch("seed", env.branch); err != nil {
			return nil, err
		}
	}

	// --- git baseline target ---
	gitSrv, err := StartGitCGI(filepath.Join(workdir, "gitroot"))
	if err != nil {
		return nil, err
	}
	defer gitSrv.Stop()
	gitBase := gitAdapter{gitSrv}
	gitRepo := gitBase.RepoURL() + "/seed.git"

	// Paired, interleaved rounds — the anti-bias core (docs/operations/
	// performance.md §3b): for every scenario family, banyan's sample and
	// git's sample of the SAME round run back-to-back, so host drift
	// (thermal throttling, background load, page-cache state) lands inside
	// the round, on both sides of the ratio equally, instead of between two
	// whole-target blocks. A control clone measured before each pair shows
	// how much of the wall-time movement was the machine itself.
	famB := buildFamilies(rep, "banyan", banyan, banyanRepo, banyanSub, scratch, filepath.Join(workdir, "clients-b"), runs, env, want)
	byName := map[string]*family{}
	for _, f := range buildFamilies(rep, "git", gitBase, gitRepo, "", scratch, filepath.Join(workdir, "clients-g"), runs, env, want) {
		byName[f.name] = f
	}
	control, err := newControl(workdir)
	if err != nil {
		return nil, err
	}
	for _, a := range famB {
		b := byName[a.name] // nil = banyan-only family (the subtree lanes)
		if a.setup != nil {
			if err := a.setup(); err != nil {
				return nil, err
			}
		}
		if b != nil && b.setup != nil {
			if err := b.setup(); err != nil {
				return nil, err
			}
		}
		for i := 0; i < a.rounds; i++ {
			rep.Control = append(rep.Control, control.sample())
			if err := a.round(i); err != nil {
				return nil, err
			}
			if b != nil {
				if err := b.round(i); err != nil {
					return nil, err
				}
			}
		}
		if a.finish != nil {
			a.finish()
		}
		if b != nil && b.finish != nil {
			b.finish()
		}
	}
	return rep, nil
}

// controlChannel is the host-performance thermistor: a fixed tiny LOCAL
// clone measured before every paired round. Deterministic repo, never the
// sample, no server in the path — only the machine is under test (process
// spawn + pack write + checkout).
type controlChannel struct {
	bare string
	dst  string
	n    int
}

func newControl(workdir string) (*controlChannel, error) {
	c := &controlChannel{
		bare: filepath.Join(workdir, "control.git"),
		dst:  filepath.Join(workdir, "control-clones"),
	}
	if err := SeedRepo(filepath.Join(workdir, "control-seed"), c.bare, Profiles["small"], 7); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *controlChannel) sample() int64 {
	c.n++
	dst := filepath.Join(c.dst, fmt.Sprintf("ctl-%d", c.n))
	start := time.Now()
	if _, err := git("", "clone", "-q", "--no-local", c.bare, dst); err != nil {
		return -1 // a failed probe is itself a host signal; keep sampling
	}
	return time.Since(start).Milliseconds()
}

// gitVersion surfaces the baseline's git for the report env fingerprint.
func gitVersion() string {
	out, err := git("", "--version")
	if err != nil {
		return "?"
	}
	parts := strings.SplitN(strings.TrimSpace(out), " ", 3)
	if len(parts) < 3 {
		return "?"
	}
	return parts[2]
}

// ScenarioFamilies is the filterable scenario set (--scenarios): big real
// samples can skip the burst lane's per-writer full clones while keeping
// every transport lane.
var ScenarioFamilies = []string{
	"clone-full", "ls-remote", "fetch-incremental", "push-one-commit",
	"clone-shallow", "clone-partial", "dev-loop", "burst",
	"clone-subtree", "push-subtree",
}

// familyFilter gates scenario families by name; an empty set runs everything.
func familyFilter(set map[string]bool) func(string) bool {
	if len(set) == 0 {
		return func(string) bool { return true }
	}
	return func(f string) bool { return set[f] }
}

// family is one scenario family's per-target harness: setup runs once
// before the family's rounds (client clones, increment bases), round(i)
// executes one measured sample, finish stores the accumulated rows. The
// paired scheduler in runTargets runs each family's rounds interleaved
// across the two targets.
type family struct {
	name   string
	rounds int
	setup  func() error
	round  func(i int) error
	finish func()
}

// buildFamilies instantiates every runnable scenario family for one target.
// dirBase holds the per-scenario client clones; subURL empty omits the
// subtree families (stock git has no subtree clone — the full clone is its
// nearest baseline, and the sparse-checkout variant is measured separately).
// env carries the sample's branch and burst layout; want gates families for
// filtered runs.
func buildFamilies(rep *Report, target string, s serverIface, repoURL, subURL, scratchIn, dirBase string, runs int, env scenarioEnv, want func(string) bool) []*family {
	store := func(row string, set *sampleSet) {
		if !set.empty() {
			rep.Rows[target+"/"+row] = set.summarize()
		}
	}
	var fams []*family

	// clone-full: round 0 is the cold lane, the rest warm (fresh client each
	// time; the server keeps its state).
	if want("clone-full") {
		var cold sampleSet
		warm := &sampleSet{}
		fams = append(fams, &family{
			name:   "clone-full",
			rounds: runs,
			round: func(i int) error {
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
				return nil
			},
			finish: func() {
				store("clone-full-cold", &cold)
				store("clone-full-warm", warm)
			},
		})
	}

	// ls-remote (advertisement cost — for banyan this includes the woven
	// root view resolution).
	if want("ls-remote") {
		ls := &sampleSet{}
		fams = append(fams, &family{
			name:   "ls-remote",
			rounds: runs,
			round: func(i int) error {
				ms, out, in, err := gitRun(s, "", "ls-remote", repoURL)
				if err != nil {
					return err
				}
				ls.add(ms, out, in)
				return nil
			},
			finish: func() { store("ls-remote", ls) },
		})
	}

	// fetch-incremental: one client holds the history; each round pushes one
	// new commit (unmeasured) and fetches it back.
	if want("fetch-incremental") {
		fetches := &sampleSet{}
		fetchClient := filepath.Join(dirBase, "fetch-client")
		scratch := scratchIn
		fams = append(fams, &family{
			name:   "fetch-incremental",
			rounds: runs,
			setup: func() error {
				if _, err := git("", "clone", "-q", repoURL, fetchClient); err != nil {
					return err
				}
				// A real merge-rich sample: the increment base must be
				// cloned from THIS target — banyan advertises the woven view
				// (a re-linearized chain), so a mirror-based scratch pushes
				// a stale old tip and rightly draws non-fast-forward. Seed
				// profiles pass a shared mirror clone (identity OIDs).
				if scratch == "" {
					scratch = filepath.Join(dirBase, "scratch")
					if _, err := git("", "clone", "-q", repoURL, scratch); err != nil {
						return err
					}
					s.take() // setup, unmeasured
				}
				return nil
			},
			round: func(i int) error {
				if err := IncrementCommit(scratch, target, i); err != nil {
					return err
				}
				if _, err := git(scratch, "push", "-q", repoURL, env.branch); err != nil {
					return err
				}
				s.take() // discard the push's bytes
				ms, out, in, err := gitRun(s, fetchClient, "fetch", "-q", "origin")
				if err != nil {
					return err
				}
				fetches.add(ms, out, in)
				return nil
			},
			finish: func() { store("fetch-incremental", fetches) },
		})
	}

	// push: fresh client commits one unique change and pushes it.
	if want("push-one-commit") {
		pushes := &sampleSet{}
		fams = append(fams, &family{
			name:   "push-one-commit",
			rounds: runs,
			round: func(i int) error {
				dst := filepath.Join(dirBase, fmt.Sprintf("push-client-%d", i))
				if _, err := git("", "clone", "-q", repoURL, dst); err != nil {
					return err
				}
				s.take() // discard the setup clone's bytes — only the push is measured
				if err := IncrementCommit(dst, target, 100+i); err != nil {
					return err
				}
				ms, out, in, err := gitRun(s, dst, "push", "-q", repoURL, env.branch)
				if err != nil {
					return err
				}
				pushes.add(ms, out, in)
				return nil
			},
			finish: func() { store("push-one-commit", pushes) },
		})
	}

	// clone-shallow (depth=1): both servers implement deepen.
	if want("clone-shallow") {
		shallow := &sampleSet{}
		fams = append(fams, &family{
			name:   "clone-shallow",
			rounds: runs,
			round: func(i int) error {
				dst := filepath.Join(dirBase, fmt.Sprintf("shallow-%d", i))
				ms, out, in, err := gitRun(s, "", "clone", "-q", "--depth", "1", repoURL, dst)
				if err != nil {
					return err
				}
				shallow.add(ms, out, in)
				return nil
			},
			finish: func() { store("clone-shallow", shallow) },
		})
	}

	// clone-partial (blob:none): both servers advertise the filter.
	if want("clone-partial") {
		partial := &sampleSet{}
		fams = append(fams, &family{
			name:   "clone-partial",
			rounds: runs,
			round: func(i int) error {
				dst := filepath.Join(dirBase, fmt.Sprintf("partial-%d", i))
				ms, out, in, err := gitRun(s, "", "clone", "-q", "--filter=blob:none", "--no-checkout", repoURL, dst)
				if err != nil {
					return err
				}
				partial.add(ms, out, in)
				return nil
			},
			finish: func() { store("clone-partial", partial) },
		})
	}

	// dev-loop: one client, steady-state rounds of edit+commit+push+fetch —
	// the collaborative heartbeat (scion's "land"), each round a sample.
	if want("dev-loop") {
		loop := &sampleSet{}
		loopClient := filepath.Join(dirBase, "dev-loop")
		fams = append(fams, &family{
			name:   "dev-loop",
			rounds: runs,
			setup: func() error {
				if _, err := git("", "clone", "-q", repoURL, loopClient); err != nil {
					return err
				}
				s.take()
				return nil
			},
			round: func(i int) error {
				if err := IncrementCommit(loopClient, target, 200+i); err != nil {
					return err
				}
				start := time.Now()
				if _, err := git(loopClient, "push", "-q", "origin", env.branch); err != nil {
					return err
				}
				if _, err := git(loopClient, "fetch", "-q", "origin"); err != nil {
					return err
				}
				ms := time.Since(start).Milliseconds()
				q, a := s.take() // q = client→server, a = server→client
				loop.add(ms, a, q)
				return nil
			},
			finish: func() { store("dev-loop", loop) },
		})
	}

	// burst-writers: W clients (cloned outside the timer) each edit a
	// DIFFERENT file and push at once. git's client-side non-FF refusal makes
	// losers pull --rebase and push again; banyan's convergence train lands
	// them server-side. Record total (first start → last finish) plus the
	// writers' own p50/p99 (scion's land-N-writers shape).
	if want("burst") {
		const writers = 8
		totals := &sampleSet{}
		p50s := &sampleSet{}
		p99s := &sampleSet{}
		fams = append(fams, &family{
			name:   "burst",
			rounds: max(2, runs-1),
			round: func(round int) error {
				dirs := make([]string, writers)
				for w := 0; w < writers; w++ {
					d := filepath.Join(dirBase, fmt.Sprintf("burst-%d-%d", round, w))
					if _, err := git("", "clone", "-q", repoURL, d); err != nil {
						return err
					}
					full := filepath.Join(d, filepath.FromSlash(env.burstPath(w)))
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
				total, each := runBurst(s, dirs, repoURL, env.branch)
				q, a := s.take() // q = client→server, a = server→client
				totals.add(total, a, q)
				p50s.add(pctl(each, 0.50), 0, 0)
				p99s.add(pctl(each, 0.99), 0, 0)
				return nil
			},
			finish: func() {
				store("burst-writers-8-total", totals)
				store("burst-writers-8-p50", p50s)
				store("burst-writers-8-p99", p99s)
			},
		})
	}

	if subURL != "" && want("clone-subtree") {
		// subtree clone (banyan structural capability): download equals the
		// subtree's own history closure, not the monorepo's.
		subs := &sampleSet{}
		fams = append(fams, &family{
			name:   "clone-subtree",
			rounds: runs,
			round: func(i int) error {
				dst := filepath.Join(dirBase, fmt.Sprintf("sub-%d", i))
				ms, out, in, err := gitRun(s, "", "clone", "-q", subURL, dst)
				if err != nil {
					return err
				}
				subs.add(ms, out, in)
				return nil
			},
			finish: func() { store("clone-subtree", subs) },
		})
	}

	if subURL != "" && want("push-subtree") {
		// subtree push: a subtree-view client commits and pushes — the
		// directory-granular write path (no git counterpart; the full-repo
		// push row is the nearest baseline).
		subPush := &sampleSet{}
		fams = append(fams, &family{
			name:   "push-subtree",
			rounds: runs,
			round: func(i int) error {
				dst := filepath.Join(dirBase, fmt.Sprintf("sub-push-%d", i))
				if _, err := git("", "clone", "-q", subURL, dst); err != nil {
					return err
				}
				s.take()
				if err := IncrementCommit(dst, target, 300+i); err != nil {
					return err
				}
				ms, out, in, err := gitRun(s, dst, "push", "-q", "origin", env.branch)
				if err != nil {
					return err
				}
				subPush.add(ms, out, in)
				return nil
			},
			finish: func() { store("push-subtree", subPush) },
		})
	}
	return fams
}

// runBurst fires all writers' pushes simultaneously and returns the total
// wall time (first start → last finish) plus each writer's own duration.
// A refused push (git non-FF under contention) pulls --rebase and retries —
// the documented git-side recovery, not a failure.
func runBurst(s serverIface, dirs []string, repoURL, branch string) (total int64, each []int64) {
	start := time.Now()
	var wg sync.WaitGroup
	each = make([]int64, len(dirs))
	for i, d := range dirs {
		wg.Add(1)
		go func(i int, d string) {
			defer wg.Done()
			ws := time.Now()
			for attempt := 0; attempt < 10; attempt++ {
				if _, err := git(d, "push", "-q", "origin", branch); err == nil {
					break
				}
				if _, err := git(d, "pull", "-q", "--rebase", "origin", branch); err != nil {
					continue // contention on the rebase too; retry the push
				}
				_, _ = git(d, "push", "-q", "origin", branch)
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
	fmt.Fprintf(&b, "banyand bench  profile=%s  repo=%s  runs=%d\n", r.Profile, r.Repo, r.Runs)
	fmt.Fprintf(&b, "%s\n", r.Env)
	if ctl := r.controlLine(); ctl != "" {
		fmt.Fprintf(&b, "%s\n", ctl)
	}
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

// controlLine renders the host thermistor: median local-clone time with its
// Tukey-trimmed spread, and a drift warning when the spread exceeds a
// quarter of the median — at that point wall-time ratios are machine noise,
// and the report says so instead of hiding it (byte ratios are unaffected).
func (r *Report) controlLine() string {
	live := make([]int64, 0, len(r.Control))
	for _, v := range r.Control {
		if v >= 0 {
			live = append(live, v)
		}
	}
	if len(live) < 2 {
		return ""
	}
	sort.Slice(live, func(i, j int) bool { return live[i] < live[j] })
	med := live[len(live)/2]
	warn := ""
	if live[len(live)-1]-live[0] > med/4 {
		warn = "  ← 主机漂移明显：耗时比可信度降级（字节比不受影响）"
	}
	return fmt.Sprintf("control %dms %s%s", med, disp(live), warn)
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

// regressionLimit is the red line (docs/operations/performance.md §3): any row regressing beyond
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
