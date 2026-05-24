package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// TreeEntry is one row of `git ls-tree -r`.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "160000", "040000"
	SHA  string
}

// lsTree returns a map of path → entry for every blob under ref in repoDir.
// Uses -z for safety with weird filenames.
func lsTree(repoDir, ref string) (map[string]TreeEntry, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "-z", ref)
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w (%s)", ref, err, strings.TrimSpace(stderr.String()))
	}
	m := make(map[string]TreeEntry)
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// "100644 blob <sha>\t<path>"
		head, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			continue
		}
		fields := bytes.Fields(head)
		if len(fields) < 3 {
			continue
		}
		m[string(path)] = TreeEntry{Mode: string(fields[0]), SHA: string(fields[2])}
	}
	return m, nil
}

// reflinkSet returns paths from target whose (mode, SHA) match source's,
// restricted to regular file modes (100644 and 100755).
func reflinkSet(source, target map[string]TreeEntry) []string {
	out := make([]string, 0, len(target))
	for path, te := range target {
		if te.Mode != "100644" && te.Mode != "100755" {
			continue
		}
		se, ok := source[path]
		if !ok {
			continue
		}
		if se.Mode != te.Mode || se.SHA != te.SHA {
			continue
		}
		out = append(out, path)
	}
	return out
}

// filterSparseCheckout drops paths that Git's sparse-checkout rules would not
// materialize in repoDir. When sparse checkout is disabled, it returns tree
// unchanged.
func filterSparseCheckout(repoDir string, tree map[string]TreeEntry) (map[string]TreeEntry, error) {
	enabled, err := sparseCheckoutEnabled(repoDir)
	if err != nil || !enabled || len(tree) == 0 {
		return tree, err
	}

	paths := make([]string, 0, len(tree))
	for path := range tree {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var input bytes.Buffer
	for _, path := range paths {
		input.WriteString(path)
		input.WriteByte(0)
	}

	cmd := exec.Command("git", "sparse-checkout", "check-rules", "-z")
	cmd.Dir = repoDir
	cmd.Stdin = &input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git sparse-checkout check-rules: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	filtered := make(map[string]TreeEntry)
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		path := string(rec)
		if entry, ok := tree[path]; ok {
			filtered[path] = entry
		}
	}
	return filtered, nil
}

func sparseCheckoutEnabled(repoDir string) (bool, error) {
	cmd := exec.Command("git", "config", "--bool", "core.sparseCheckout")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// `git config --bool` exits 1 specifically when the key is unset.
		// Anything else (e.g. malformed value → exit 3) is a real error.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git config core.sparseCheckout: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// seedResult summarizes a reflink seeding pass.
type seedResult struct {
	Attempted int
	Reflinked int
	StopErr   error // error that caused early stop, if any
}

// reflinkAll attempts to clonefile every path in paths from srcRoot to
// dstRoot. Stops on the first ENOTSUP/EOPNOTSUPP/EXDEV (CoW unsupported
// for this src/dst pair); other errors per-file are recorded but the
// loop continues for them so the final checkout can fix things up.
//
// The work is parallelized across NumCPU workers (capped). Parent
// directories are pre-created once instead of MkdirAll'd per file.
// We rely on `git worktree add --no-checkout` leaving the working tree
// empty, so the destinations don't pre-exist and we skip the per-file
// stat/remove dance.
//
// Note: reflinkSet matched committed blob SHAs, but the source working
// tree may be dirty — we may copy modified content. finalizeCheckout
// runs `git checkout -f HEAD` afterwards, which rehashes each file and
// rewrites any whose content doesn't match the index. Clean reflinks
// survive with their stat-cache updated; dirty ones get overwritten.
func reflinkAll(srcRoot, dstRoot string, paths []string) seedResult {
	var r seedResult
	r.Attempted = len(paths)
	if os.Getenv("GIT_COW_WORKTREE_FORCE_NO_COW") != "" {
		r.StopErr = errCoWUnsupported
		return r
	}
	if len(paths) == 0 {
		return r
	}

	// Pre-create the union of parent directories.
	for _, d := range collectDirs(paths) {
		if err := os.MkdirAll(filepath.Join(dstRoot, d), 0o777); err != nil {
			r.StopErr = fmt.Errorf("mkdir %s: %w", d, err)
			return r
		}
	}

	workers := min(runtime.NumCPU(), 16, len(paths))

	work := make(chan string, workers*2)
	var wg sync.WaitGroup
	var reflinked atomic.Int64
	var stopOnce sync.Once
	var stopErr atomic.Pointer[error]

	for range workers {
		wg.Go(func() {
			for p := range work {
				if stopErr.Load() != nil {
					continue // drain
				}
				src := filepath.Join(srcRoot, p)
				dst := filepath.Join(dstRoot, p)
				if err := cowClone(src, dst); err != nil {
					if isCoWUnsupported(err) {
						stopOnce.Do(func() {
							e := err
							stopErr.Store(&e)
						})
						continue
					}
					// Per-file failure (missing source file, etc.):
					// leave dst absent and let checkout write it.
					continue
				}
				reflinked.Add(1)
			}
		})
	}

	for _, p := range paths {
		work <- p
	}
	close(work)
	wg.Wait()

	r.Reflinked = int(reflinked.Load())
	if p := stopErr.Load(); p != nil {
		r.StopErr = *p
	}
	return r
}

// collectDirs returns every distinct parent directory of paths.
// MkdirAll handles ordering, so we don't bother sorting.
func collectDirs(paths []string) []string {
	seen := make(map[string]struct{}, len(paths)/4+1)
	for _, p := range paths {
		d := filepath.Dir(p)
		for d != "." && d != "/" && d != "" {
			if _, ok := seen[d]; ok {
				break
			}
			seen[d] = struct{}{}
			d = filepath.Dir(d)
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	return out
}

// finalizeCheckout brings the new worktree's working tree in line with its
// index, then runs post-checkout with the same arguments `git worktree add`
// uses for a newly-created worktree. We force checkout.workers=0 so git
// parallelises file writes / stat-cache hashing across all CPUs.
func finalizeCheckout(repoDir, targetSHA string) error {
	cmd := exec.Command("git",
		"-c", "checkout.workers=0",
		"-c", "core.hooksPath=/dev/null",
		"checkout", "-f", "HEAD")
	cmd.Dir = repoDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	if err := runPostCheckoutHook(repoDir, targetSHA); err != nil {
		return err
	}
	return nil
}

func runPostCheckoutHook(repoDir, targetSHA string) error {
	cmd := exec.Command("git", "hook", "run", "--ignore-missing", "post-checkout", "--", zeroObjectIDFor(targetSHA), targetSHA, "1")
	cmd.Dir = repoDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("post-checkout hook: %w", err)
	}
	return nil
}

func zeroObjectIDFor(oid string) string {
	return strings.Repeat("0", len(oid))
}

// resolveRev resolves a commit-ish to its full SHA within repoDir.
func resolveRev(repoDir, rev string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", rev+"^{commit}")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isCoWUnsupported reports whether err indicates the filesystem/pair
// doesn't support CoW reflinks (as opposed to a transient or per-file
// failure).
func isCoWUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errCoWUnsupported) {
		return true
	}
	// Underlying syscall errors:
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		err = pe.Err
	}
	for _, target := range cowUnsupportedErrnos {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
