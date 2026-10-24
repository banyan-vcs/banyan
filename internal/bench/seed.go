// Package bench is the performance comparison harness (docs/12 §3): fixed
// deterministic seed repos, banyan and a genuine git http-backend (CGI) as
// the peer baseline on the same transport and machine, three temperature
// lanes (cold server / warm server / incremental client), byte and wall-time
// metrics per scenario, and a >20% regression gate against a saved baseline
// (tekeburak/scion's method: multi-baseline same-machine benchmarks with
// honest per-lane reporting and a red line that turns the nightly job red).
//
// It is a standalone tool (cmd/banyan-bench), deliberately NOT part of the
// go test hot path — correctness is pinned by the integration suite, the
// bench gate pins performance.
package bench

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SeedProfile parameterizes the deterministic seed repo.
type SeedProfile struct {
	Commits     int // history depth
	Dirs        int // top-level directories
	FilesPerDir int
	BlobMin     int // blob size range (bytes, line-based text)
	BlobMax     int
	ChurnFiles  int // files touched per commit
}

var Profiles = map[string]SeedProfile{
	"small":  {Commits: 30, Dirs: 5, FilesPerDir: 5, BlobMin: 1024, BlobMax: 3072, ChurnFiles: 3},
	"medium": {Commits: 120, Dirs: 10, FilesPerDir: 8, BlobMin: 2048, BlobMax: 6144, ChurnFiles: 4},
}

// git runs git with the benchmark's fixed identity config.
func git(dir string, args ...string) (string, error) {
	full := append([]string{"-c", "core.autocrlf=false", "-c", "user.name=bench", "-c", "user.email=bench@banyan.dev"}, args...)
	cmd := exec.Command("git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %v (dir=%s): %w\n%s", args, dir, err, out.String())
	}
	return out.String(), nil
}

func fileContent(rnd *rand.Rand, path string, version, min, max int) []byte {
	size := min + rnd.Intn(max-min)
	var b strings.Builder
	for b.Len() < size {
		fmt.Fprintf(&b, "content of %s at version %d, line %d\n", path, version, b.Len()/48)
	}
	return []byte(b.String())
}

// SeedRepo builds a deterministic working repository at dst (real git
// plumbing, fixed rand seed → reproducible object sets) and mirrors it into
// a bare repository at barePath.
func SeedRepo(dst, barePath string, p SeedProfile, seed int64) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if _, err := git(dst, "init", "-q", "-b", "main", "."); err != nil {
		return err
	}
	rnd := rand.New(rand.NewSource(seed))

	write := func(rel string, content []byte) error {
		full := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, content, 0o644)
	}

	// Initial tree: p.Dirs × p.FilesPerDir files.
	for d := 0; d < p.Dirs; d++ {
		for f := 0; f < p.FilesPerDir; f++ {
			rel := fmt.Sprintf("pkg%02d/file%02d.txt", d, f)
			if err := write(rel, fileContent(rnd, rel, 0, p.BlobMin, p.BlobMax)); err != nil {
				return err
			}
		}
	}
	if _, err := git(dst, "add", "-A"); err != nil {
		return err
	}
	if _, err := git(dst, "commit", "-qm", "seed: initial tree"); err != nil {
		return err
	}

	// History: churn a few files per commit; every 8th commit adds a new file
	// (tree growth), every 5th touches a nested subtree.
	for c := 1; c < p.Commits; c++ {
		for i := 0; i < p.ChurnFiles; i++ {
			d := rnd.Intn(p.Dirs)
			f := rnd.Intn(p.FilesPerDir)
			rel := fmt.Sprintf("pkg%02d/file%02d.txt", d, f)
			if err := write(rel, fileContent(rnd, rel, c, p.BlobMin, p.BlobMax)); err != nil {
				return err
			}
		}
		if c%8 == 0 {
			rel := fmt.Sprintf("pkg%02d/generated/gen%03d.txt", rnd.Intn(p.Dirs), c)
			if err := write(rel, fileContent(rnd, rel, c, p.BlobMin, p.BlobMax)); err != nil {
				return err
			}
		}
		if _, err := git(dst, "add", "-A"); err != nil {
			return err
		}
		if _, err := git(dst, "commit", "-qm", fmt.Sprintf("seed: change %d", c)); err != nil {
			return err
		}
	}

	// Mirror into a bare repo (the git baseline serves this directly; banyan
	// receives the same objects via git push). HEAD must point at main or
	// clones of the bare repo check out nothing.
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		return err
	}
	if _, err := git("", "init", "-q", "--bare", "-b", "main", barePath); err != nil {
		return err
	}
	// http-backend refuses pushes over http by default (authenticated-only);
	// the bench peer must accept them like banyan's demo-friendly server.
	if _, err := git(barePath, "config", "http.receivepack", "true"); err != nil {
		return err
	}
	if _, err := git(dst, "push", "-q", barePath, "main"); err != nil {
		return err
	}
	return nil
}

// IncrementCommit appends one unique commit in the working repo at dir
// (unique per round: the content embeds round) — used to create measurable
// fetch/push traffic.
func IncrementCommit(dir, label string, round int) error {
	rel := "bench-increment.txt"
	content := []byte(fmt.Sprintf("bench increment %s round %d\n%s\n", label, round, strings.Repeat("payload ", 64)))
	full := filepath.Join(dir, rel)
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return err
	}
	if _, err := git(dir, "add", "-A"); err != nil {
		return err
	}
	_, err := git(dir, "commit", "-qm", fmt.Sprintf("bench: increment %s #%d", label, round))
	return err
}
