// banyan-bench runs the performance comparison harness (docs/11 §3):
// banyan vs a genuine git http-backend on the same machine and transport,
// deterministic seed repos, cold/warm/incremental lanes, and a >20%
// regression gate against a saved baseline.
//
//	banyan-bench --profile small                 # run + print table
//	banyan-bench --profile small --save base.json
//	banyan-bench --profile small --check base.json   # exit 1 on regression
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/banyan-vcs/banyan/internal/bench"
)

func main() {
	profile := flag.String("profile", "small", "seed profile: small | medium")
	runs := flag.Int("runs", 3, "runs per scenario (run 0 = cold lane, rest warm)")
	save := flag.String("save", "", "write the report as a baseline JSON file")
	check := flag.String("check", "", "compare against a baseline JSON; exit 1 on >20% regression")
	workdir := flag.String("work", "", "working directory (default: temp)")
	flag.Parse()

	p, ok := bench.Profiles[*profile]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown profile %q (have small, medium)\n", *profile)
		os.Exit(2)
	}
	dir := *workdir
	if dir == "" {
		t, err := os.MkdirTemp("", "banyan-bench-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "tempdir:", err)
			os.Exit(2)
		}
		dir = t
		defer os.RemoveAll(dir)
	}

	rep, err := bench.Run(*profile, p, *runs, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(rep.Render())

	if *save != "" {
		if err := rep.Save(*save); err != nil {
			fmt.Fprintf(os.Stderr, "save baseline: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("baseline saved: %s\n", *save)
	}
	if *check != "" {
		raw, err := os.ReadFile(*check)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read baseline: %v\n", err)
			os.Exit(2)
		}
		var baseline bench.Report
		if err := json.Unmarshal(raw, &baseline); err != nil {
			fmt.Fprintf(os.Stderr, "parse baseline: %v\n", err)
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
}
