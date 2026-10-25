// Statistics for the bench harness: hyperfine-style sampling with Tukey-fence
// outlier detection (1.5×IQR), percentiles, and dispersion. Wall-time samples
// carry run-to-run noise; byte counts are deterministic per scenario and are
// reported as means.
package bench

import (
	"fmt"
	"sort"
	"strings"
)

// sampleSet accumulates one scenario's wall-time samples and byte totals.
type sampleSet struct {
	ms    []int64
	bytes []int64 // per-sample server→client bytes
	in    []int64 // per-sample client→server bytes
}

func (s *sampleSet) add(ms, bytesOut, bytesIn int64) {
	s.ms = append(s.ms, ms)
	s.bytes = append(s.bytes, bytesOut)
	s.in = append(s.in, bytesIn)
}

func (s *sampleSet) empty() bool { return len(s.ms) == 0 }

// summarize folds the samples into a Metric: p50 wall time with outlier
// count (Tukey fence over the sample distribution), mean bytes.
func (s *sampleSet) summarize() Metric {
	n := len(s.ms)
	if n == 0 {
		return Metric{}
	}
	sorted := append([]int64(nil), s.ms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var out Metric
	out.Milliseconds = sorted[n/2]
	out.Samples = sorted
	out.Outliers = tukeyOutliers(sorted)
	var b, bi int64
	for i := range s.bytes {
		b += s.bytes[i]
		bi += s.in[i]
	}
	out.BytesOut = b / int64(n)
	out.BytesIn = bi / int64(n)
	return out
}

// tukeyOutliers counts samples beyond 1.5×IQR of the sorted distribution
// (hyperfine's soft-limit rule): a high count means the lane is noisy, not
// that the median is wrong — the renderer surfaces it as ±.
func tukeyOutliers(sorted []int64) int {
	n := len(sorted)
	if n < 4 {
		return 0
	}
	q1 := sorted[n/4]
	q3 := sorted[3*n/4]
	iqr := q3 - q1
	lo, hi := q1-3*iqr/2, q3+3*iqr/2
	out := 0
	for _, v := range sorted {
		if v < lo || v > hi {
			out++
		}
	}
	return out
}

// pctl returns the p-th percentile of the raw samples (linear interpolation
// off; index-based is fine for benchmark reporting).
func pctl(raw []int64, p float64) int64 {
	sorted := append([]int64(nil), raw...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1)*p + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// disp renders the ± spread around the median: (max-min) of the non-outlier
// samples, keeping the table honest about noise.
func disp(sorted []int64) string {
	if len(sorted) < 2 {
		return ""
	}
	lo, hi := sorted[0], sorted[len(sorted)-1]
	// Trim Tukey outliers from the spread so one hiccup does not dominate.
	q1 := sorted[len(sorted)/4]
	q3 := sorted[3*len(sorted)/4]
	iqr := q3 - q1
	tlo, thi := q1-3*iqr/2, q3+3*iqr/2
	for _, v := range sorted {
		if v >= tlo {
			lo = v
			break
		}
	}
	for i := len(sorted) - 1; i >= 0; i-- {
		if sorted[i] <= thi {
			hi = sorted[i]
			break
		}
	}
	if hi < lo {
		lo, hi = sorted[0], sorted[len(sorted)-1]
	}
	return fmt.Sprintf("±%dms", hi-lo)
}

// RenderMarkdown emits the comparison as a Markdown report (the published
// artifact): one table per metric family plus lane notes.
func (r *Report) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# banyan vs git 性能对比报告\n\n")
	fmt.Fprintf(&b, "- profile：%s（%s）\n", r.Profile, r.Repo)
	fmt.Fprintf(&b, "- 运行：%s\n", r.Env)
	fmt.Fprintf(&b, "- 方法：同机对等基线（git http-backend CGI）；每场景 %d 采样，中位数 + IQR 离群计数；字节为服务端计数代理往返均值\n\n", r.Runs)

	fmt.Fprintf(&b, "## 字节（server→client，越低越好）\n\n")
	fmt.Fprintf(&b, "| 场景 | banyan | git | banyan/git | 口径 |\n|---|---|---|---|---|\n")
	for _, row := range r.orderedRows() {
		br, gr := r.Rows["banyan/"+row], r.Rows["git/"+row]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", row, kb(br.BytesOut), kb(gr.BytesOut), xRatio(br.BytesOut, gr.BytesOut), laneNote(row))
	}

	fmt.Fprintf(&b, "\n## 耗时（墙钟中位数，越低越好）\n\n")
	fmt.Fprintf(&b, "| 场景 | banyan | git | banyan/git | 离群 |\n|---|---|---|---|---|\n")
	for _, row := range r.orderedRows() {
		br, gr := r.Rows["banyan/"+row], r.Rows["git/"+row]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", row, msCell(br), msCell(gr), xRatio(br.Milliseconds, gr.Milliseconds), outlierCell(br, gr))
	}
	fmt.Fprintf(&b, "\n%s\n", laneLegend())
	return b.String()
}

func (r *Report) orderedRows() []string {
	seen := map[string]bool{}
	var names []string
	for k := range r.Rows {
		n := strings.TrimPrefix(strings.TrimPrefix(k, "banyan/"), "git/")
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func kb(v int64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fKB", float64(v)/1024)
}

func msCell(m Metric) string {
	if m.Milliseconds == 0 && m.BytesOut == 0 && m.BytesIn == 0 {
		return "—"
	}
	if len(m.Samples) > 1 {
		return fmt.Sprintf("%dms %s", m.Milliseconds, disp(m.Samples))
	}
	return fmt.Sprintf("%dms", m.Milliseconds)
}

func outlierCell(br, gr Metric) string {
	var parts []string
	if br.Outliers > 0 {
		parts = append(parts, fmt.Sprintf("banyan×%d", br.Outliers))
	}
	if gr.Outliers > 0 {
		parts = append(parts, fmt.Sprintf("git×%d", gr.Outliers))
	}
	if len(parts) == 0 {
		return "clean"
	}
	return strings.Join(parts, ", ")
}

func xRatio(a, b int64) string {
	if b == 0 {
		if a == 0 {
			return "—"
		}
		return "n/a（git 无对应能力）"
	}
	return fmt.Sprintf("%.2fx", float64(a)/float64(b))
}

// laneNote annotates comparability caveats per scenario family.
func laneNote(scenario string) string {
	switch {
	case strings.Contains(scenario, "subtree"):
		return "banyan 只传子树闭包；git 整仓（结构优势，非同口径）"
	case strings.HasPrefix(scenario, "burst"):
		return "git 侧被拒不重推则无落地；含 pull--rebase 重试"
	default:
		return "同口径"
	}
}

func laneLegend() string {
	return "口径说明：cold=服务进程冷启动后首个请求；warm=同进程稳态（git CGI 每请求新进程，warm 仅 OS 缓存）；" +
		"incremental=持史客户端取单新提交；dev-loop=clone 后每轮 commit+push+fetch；" +
		"burst=8 并发写者各改不同文件同时 push（git 侧 non-FF 被拒后 pull --rebase 重推；banyan 收敛列车服务端合并）。"
}

// RenderCSV emits every raw sample (scion bench-real's shape:
// workload,tool,run,ms,bytes_out,bytes_in) for downstream analysis.
func (r *Report) RenderCSV() string {
	var b strings.Builder
	b.WriteString("workload,tool,run,ms,bytes_out,bytes_in\n")
	type key struct{ workload, tool string }
	var keys []key
	for k, m := range r.Rows {
		i := strings.Index(k, "/")
		if i < 0 {
			continue
		}
		keys = append(keys, key{k[i+1:], k[:i]})
		_ = m
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].workload != keys[j].workload {
			return keys[i].workload < keys[j].workload
		}
		return keys[i].tool < keys[j].tool
	})
	for _, k := range keys {
		m := r.Rows[k.tool+"/"+k.workload]
		for run, ms := range m.Samples {
			fmt.Fprintf(&b, "%s,%s,%d,%d,%d,%d\n", k.workload, k.tool, run, ms, m.BytesOut, m.BytesIn)
		}
	}
	return b.String()
}
