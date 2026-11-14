// bench.go — `banyand bench`: the performance comparison harness (docs/operations/performance.md
// §3), formerly the standalone banyan-bench binary. banyan vs a genuine git
// http-backend on the same machine and transport, deterministic seed repos,
// cold/warm/incremental lanes plus workflow lanes (dev-loop, 8-writer
// burst), hyperfine-style sampling (percentiles + IQR outlier counts), and
// a >20% regression gate against a saved baseline.
//
//	banyand bench --profile medium                    # run + print table
//	banyand bench --profile medium --save base.json
//	banyand bench --profile medium --check base.json  # exit 1 on regression
//	banyand bench --profile large --runs 5 --md report.md --csv samples.csv
//
// Real-repository samples (docs/operations/performance.md §3b): mirror a
// local repo instead of a synthetic seed — real history exercises
// negotiation/closure/packing paths the seeds cannot.
//
//	banyand bench --repo F:\Workspace\git --md report.md --csv samples.csv
//	banyand bench --repo /path/repo --scenarios clone-full,ls-remote   # big samples
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/banyan-vcs/banyan/internal/bench"
)

func runBench(args []string) {
	fs := flag.NewFlagSet("banyand bench", flag.ExitOnError)
	profile := fs.String("profile", "small", "seed profile: small | medium | large")
	repo := fs.String("repo", "", "real-repository sample: mirror this local repo (all branches+tags) instead of a synthetic seed")
	scenarios := fs.String("scenarios", "", "comma-separated scenario families to run (default all): "+strings.Join(bench.ScenarioFamilies, ", "))
	runs := fs.Int("runs", 3, "runs per scenario (run 0 = cold lane, rest warm; minimum 3)")
	save := fs.String("save", "", "write the report as a baseline JSON file")
	check := fs.String("check", "", "compare against a baseline JSON; exit 1 on >20% regression")
	md := fs.String("md", "", "write a Markdown comparison report")
	csv := fs.String("csv", "", "write every raw sample as CSV (workload,tool,run,ms,bytes)")
	workdir := fs.String("work", "", "working directory (default: temp)")
	memlimit := fs.Int("memlimit", 0, "soft memory limit in MiB for the in-process server (GOMEMLIMIT semantics: the GC works harder as the heap nears it — a big real-sample materialization degrades instead of starving the machine); 0 = no limit")
	fs.Parse(args)

	// The bench runs the server in-process, so an unbounded materialization
	// (git.git-scale cold advertisement, docs/operations/performance.md §3b
	// finding ④) grows until the OOM killer takes the whole run — and
	// sometimes the machine's responsiveness with it. A soft limit turns
	// that death into GC pressure.
	if *memlimit > 0 {
		debug.SetMemoryLimit(int64(*memlimit) << 20)
	}

	families, okScen := parseScenarios(*scenarios)
	if !okScen {
		os.Exit(2) // parseScenarios already complained
	}

	dir := *workdir
	if dir == "" {
		t, err := os.MkdirTemp("", "banyand-bench-*")
		if err != nil {
			fmt.Println("tempdir:", err)
			os.Exit(2)
		}
		dir = t
		defer os.RemoveAll(dir)
	}

	var rep *bench.Report
	var err error
	if *repo != "" {
		name := strings.TrimSuffix(filepath.Base(*repo), ".git")
		rep, err = bench.RunReal(name, *repo, bench.Options{Runs: *runs, Workdir: dir, Scenarios: families})
	} else {
		p, ok := bench.Profiles[*profile]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown profile %q (have small, medium, large)\n", *profile)
			os.Exit(2)
		}
		rep, err = bench.Run(*profile, p, *runs, dir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(rep.Render())

	if *save != "" {
		if err := rep.Save(*save); err != nil {
			fmt.Fprintf(os.Stderr, "save baseline: %v\n", *save)
			os.Exit(2)
		}
		fmt.Printf("baseline saved: %s\n", *save)
	}
	if *check != "" {
		raw, err := os.ReadFile(*check)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read baseline: %v\n", *check)
			os.Exit(2)
		}
		var baseline bench.Report
		if err := json.Unmarshal(raw, &baseline); err != nil {
			fmt.Fprintf(os.Stderr, "parse baseline: %v\n", *check)
			os.Exit(2)
		}
		if bad := rep.Check(&baseline); len(bad) > 0 {
			fmt.Println("SLO RED — 回归超 20% 红线：")
			for _, row := range bad {
				fmt.Println("  -", row)
			}
			os.Exit(1)
		}
		fmt.Println("SLO GREEN — 全部指标在基线 20% 以内")
	}
	if *md != "" {
		if err := rep.WriteMarkdown(*md); err != nil {
			fmt.Fprintf(os.Stderr, "write markdown: %v\n", *md)
			os.Exit(2)
		}
		fmt.Printf("markdown report: %s\n", *md)
	}
	if *csv != "" {
		if err := rep.WriteCSV(*csv); err != nil {
			fmt.Fprintf(os.Stderr, "write csv: %v\n", *csv)
			os.Exit(2)
		}
		fmt.Printf("raw samples: %s\n", *csv)
	}
}

// parseScenarios parses the --scenarios family filter; ok=false on an
// unknown name (already reported). A nil map runs every family.
func parseScenarios(s string) (families map[string]bool, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	known := map[string]bool{}
	for _, f := range bench.ScenarioFamilies {
		known[f] = true
	}
	families = map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		f := strings.TrimSpace(strings.ToLower(part))
		if f == "" {
			continue
		}
		if !known[f] {
			fmt.Fprintf(os.Stderr, "unknown scenario family %q (have %s)\n", f, strings.Join(bench.ScenarioFamilies, ", "))
			return nil, false
		}
		families[f] = true
	}
	return families, true
}
