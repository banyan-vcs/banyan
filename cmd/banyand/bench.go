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
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/banyan-vcs/banyan/internal/bench"
)

type benchOptions struct {
	Profile   string
	Repo      string
	Scenarios string
	Runs      int
	Save      string
	Check     string
	MD        string
	CSV       string
	Workdir   string
	MemLimit  int
}

func newBenchCmd() *cobra.Command {
	var o benchOptions
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "the performance harness: banyan vs git http-backend (docs/operations/performance.md §3)",
		RunE: func(_ *cobra.Command, _ []string) error {
			runBench(o)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&o.Profile, "profile", "small", "seed profile: small | medium | large")
	fl.StringVar(&o.Repo, "repo", "", "real-repository sample: mirror this local repo (all branches+tags) instead of a synthetic seed")
	fl.StringVar(&o.Scenarios, "scenarios", "", "comma-separated scenario families to run (default all): "+strings.Join(bench.ScenarioFamilies, ", "))
	fl.IntVar(&o.Runs, "runs", 3, "runs per scenario (run 0 = cold lane, rest warm; minimum 3)")
	fl.StringVar(&o.Save, "save", "", "write the report as a baseline JSON file")
	fl.StringVar(&o.Check, "check", "", "compare against a baseline JSON; exit 1 on >20% regression")
	fl.StringVar(&o.MD, "md", "", "write a Markdown comparison report")
	fl.StringVar(&o.CSV, "csv", "", "write every raw sample as CSV (workload,tool,run,ms,bytes)")
	fl.StringVar(&o.Workdir, "work", "", "working directory (default: temp)")
	fl.IntVar(&o.MemLimit, "memlimit", 0, "soft memory limit in MiB for the in-process server (GOMEMLIMIT semantics: the GC works harder as the heap nears it — a big real-sample materialization degrades instead of starving the machine); 0 = no limit")
	return cmd
}

func runBench(o benchOptions) {
	// The bench runs the server in-process, so an unbounded materialization
	// (git.git-scale cold advertisement, docs/operations/performance.md §3b
	// finding ④) grows until the OOM killer takes the whole run — and
	// sometimes the machine's responsiveness with it. A soft limit turns
	// that death into GC pressure.
	if o.MemLimit > 0 {
		debug.SetMemoryLimit(int64(o.MemLimit) << 20)
	}

	families, okScen := parseScenarios(o.Scenarios)
	if !okScen {
		os.Exit(2) // parseScenarios already complained
	}

	dir := o.Workdir
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
	if o.Repo != "" {
		name := strings.TrimSuffix(filepath.Base(o.Repo), ".git")
		rep, err = bench.RunReal(name, o.Repo, bench.Options{Runs: o.Runs, Workdir: dir, Scenarios: families})
	} else {
		p, ok := bench.Profiles[o.Profile]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown profile %q (have small, medium, large)\n", o.Profile)
			os.Exit(2)
		}
		rep, err = bench.Run(o.Profile, p, o.Runs, dir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(rep.Render())

	if o.Save != "" {
		if err := rep.Save(o.Save); err != nil {
			fmt.Fprintf(os.Stderr, "save baseline: %v\n", o.Save)
			os.Exit(2)
		}
		fmt.Printf("baseline saved: %s\n", o.Save)
	}
	if o.Check != "" {
		raw, err := os.ReadFile(o.Check)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read baseline: %v\n", o.Check)
			os.Exit(2)
		}
		var baseline bench.Report
		if err := json.Unmarshal(raw, &baseline); err != nil {
			fmt.Fprintf(os.Stderr, "parse baseline: %v\n", o.Check)
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
	if o.MD != "" {
		if err := rep.WriteMarkdown(o.MD); err != nil {
			fmt.Fprintf(os.Stderr, "write markdown: %v\n", o.MD)
			os.Exit(2)
		}
		fmt.Printf("markdown report: %s\n", o.MD)
	}
	if o.CSV != "" {
		if err := rep.WriteCSV(o.CSV); err != nil {
			fmt.Fprintf(os.Stderr, "write csv: %v\n", o.CSV)
			os.Exit(2)
		}
		fmt.Printf("raw samples: %s\n", o.CSV)
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
