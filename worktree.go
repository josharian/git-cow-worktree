package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Worktree is one entry from `git worktree list --porcelain`.
type Worktree struct {
	Path     string
	HEAD     string // sha; empty for unmaterialized worktrees
	Branch   string // empty for detached
	Bare     bool
	Detached bool
	Locked   bool
	IsMain   bool // first entry in `git worktree list` is the main worktree
}

// listWorktrees parses `git worktree list --porcelain` from inside repoDir.
func listWorktrees(repoDir string) ([]Worktree, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain", "-z")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	return parseWorktreesPorcelain(string(out)), nil
}

// parseWorktreesPorcelain parses the `-z` (null-separated) porcelain
// output. Records are separated by a double null; fields within a record
// by single nulls.
func parseWorktreesPorcelain(out string) []Worktree {
	var wts []Worktree
	records := strings.Split(strings.TrimRight(out, "\x00"), "\x00\x00")
	for i, rec := range records {
		if rec == "" {
			continue
		}
		var wt Worktree
		wt.IsMain = i == 0
		sc := bufio.NewScanner(strings.NewReader(rec))
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		sc.Split(splitNul)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "worktree "):
				wt.Path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "HEAD "):
				h := strings.TrimPrefix(line, "HEAD ")
				if h != "0000000000000000000000000000000000000000" {
					wt.HEAD = h
				}
			case strings.HasPrefix(line, "branch "):
				wt.Branch = strings.TrimPrefix(line, "branch ")
			case line == "bare":
				wt.Bare = true
			case line == "detached":
				wt.Detached = true
			case line == "locked" || strings.HasPrefix(line, "locked "):
				wt.Locked = true
			}
		}
		if wt.Path != "" {
			wts = append(wts, wt)
		}
	}
	return wts
}

func splitNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// candidatePool returns the bounded set of worktrees we'll consider as
// CoW sources: the worktree containing cwd, the main worktree, and the
// most-recently-modified other worktrees up to maxOthers. Unmaterialized
// (zero-HEAD) and bare worktrees are dropped, as is targetPath itself.
func candidatePool(all []Worktree, cwd, targetPath string, maxOthers int) []Worktree {
	var cwdWT, mainWT *Worktree
	var others []Worktree
	for i := range all {
		wt := &all[i]
		if wt.Bare || wt.HEAD == "" {
			continue
		}
		if targetPath != "" && samePath(wt.Path, targetPath) {
			continue
		}
		if cwd != "" && cwdWT == nil && pathHasPrefix(cwd, wt.Path) {
			cwdWT = wt
			continue
		}
		if wt.IsMain {
			mainWT = wt
			continue
		}
		others = append(others, *wt)
	}
	sort.Slice(others, func(i, j int) bool {
		return worktreeMtime(others[i].Path) > worktreeMtime(others[j].Path)
	})
	if len(others) > maxOthers {
		others = others[:maxOthers]
	}
	var out []Worktree
	if cwdWT != nil {
		out = append(out, *cwdWT)
	}
	if mainWT != nil && (cwdWT == nil || cwdWT.Path != mainWT.Path) {
		out = append(out, *mainWT)
	}
	out = append(out, others...)
	return out
}

func worktreeMtime(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.ModTime().Unix()
}

// scoredSource is a candidate source worktree and its distance, in commits,
// from the target.
type scoredSource struct {
	WT    Worktree
	Score int
}

// rankSources scores each candidate by commits ahead+behind vs targetSHA (in
// repoDir) and returns them best-first, dropping any whose scoring failed.
// Ties break by candidate order (cwd first, then main, then mtime).
// Candidates are scored in parallel since each is a separate `git rev-list`.
//
// The whole ranking is returned, not just the winner, because distance alone
// doesn't establish that a worktree is worth cloning from: see analyze.
func rankSources(repoDir string, candidates []Worktree, targetSHA string) []scoredSource {
	type scored struct {
		scoredSource
		err  error
		rank int // preference order: cwd=0, main=1, others=2+
	}
	results := make([]scored, len(candidates))
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Go(func() {
			s, err := commitDistance(repoDir, c.HEAD, targetSHA)
			results[i] = scored{scoredSource: scoredSource{WT: c, Score: s}, err: err, rank: i}
		})
	}
	wg.Wait()

	var ss []scored
	for _, r := range results {
		if r.err != nil {
			continue
		}
		ss = append(ss, r)
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].Score != ss[j].Score {
			return ss[i].Score < ss[j].Score
		}
		return ss[i].rank < ss[j].rank
	})
	ranked := make([]scoredSource, len(ss))
	for i, s := range ss {
		ranked[i] = s.scoredSource
	}
	return ranked
}

// commitDistance returns commits-ahead + commits-behind between a and b.
func commitDistance(repoDir, a, b string) (int, error) {
	cmd := exec.Command("git", "rev-list", "--left-right", "--count", a+"..."+b)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("git rev-list: %w", err)
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return 0, fmt.Errorf("unexpected rev-list output: %q", string(out))
	}
	ahead, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	behind, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	return ahead + behind, nil
}

func samePath(a, b string) bool {
	ar, err1 := absClean(a)
	br, err2 := absClean(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ar == br
}

func pathHasPrefix(child, parent string) bool {
	cr, err1 := absClean(child)
	pr, err2 := absClean(parent)
	if err1 != nil || err2 != nil {
		return strings.HasPrefix(child, parent)
	}
	if cr == pr {
		return true
	}
	if !strings.HasSuffix(pr, string(os.PathSeparator)) {
		pr += string(os.PathSeparator)
	}
	return strings.HasPrefix(cr, pr)
}

// absClean returns p as an absolute path with symlinks resolved.
//
// A path that doesn't exist yet still resolves: we walk up to its deepest
// existing ancestor and re-attach the rest. The worktree we are about to
// create is named before it exists, and on macOS the difference matters —
// /tmp and /var are symlinks, so an unresolved name would compare unequal to
// the resolved one Git records, and the new worktree could be mistaken for a
// candidate source for itself.
func absClean(p string) (string, error) {
	r, err := osAbs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for dir := r; ; {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return r, nil // nothing along the way exists
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}
