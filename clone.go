package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// clonePlan is what to reflink from the source worktree into the new one.
// Dirs are cloned whole, in one syscall each; Files individually. No Files
// entry lives under any Dirs entry.
type clonePlan struct {
	Dirs  []string
	Files []string
}

// planClones walks the target tree and decides, per path, how it can be
// materialized from the source worktree.
//
// A directory whose tree SHA matches on both sides has identical tracked
// contents, so it can be cloned in one go — provided nothing under it is
// excluded (dirty, untracked, ignored, or a submodule), since a directory
// clone copies whatever is actually on disk. Otherwise we descend and fall
// back to per-file clones for the blobs that match.
func planClones(src, tgt *treeIndex, excluded map[string]bool, cloneDirs bool) clonePlan {
	skip := make(map[string]bool, len(excluded)+len(src.Gitlinks)+len(tgt.Gitlinks))
	for path := range excluded {
		skip[path] = true
	}
	// A submodule's working tree belongs to its own repository; leave the
	// whole thing to Git.
	for path := range src.Gitlinks {
		skip[path] = true
	}
	for path := range tgt.Gitlinks {
		skip[path] = true
	}
	tainted := taint(skip)
	children := childPaths(tgt)

	var plan clonePlan
	var walk func(dir string)
	walk = func(dir string) {
		for _, c := range children[dir] {
			if _, isTree := tgt.Trees[c]; isTree {
				srcSHA, inSrc := src.Trees[c]
				if !inSrc {
					continue // nothing under here can match
				}
				if cloneDirs && !skip[c] && !tainted[c] && srcSHA == tgt.Trees[c] {
					plan.Dirs = append(plan.Dirs, c)
					continue
				}
				walk(c)
				continue
			}
			te, isBlob := tgt.Blobs[c]
			if !isBlob || !te.IsRegular() || skip[c] {
				continue
			}
			if src.Blobs[c] != te {
				continue
			}
			plan.Files = append(plan.Files, c)
		}
	}
	walk(".")
	return plan
}

// childPaths indexes a tree by parent directory. The root's children are
// keyed by ".", matching filepath.Dir.
func childPaths(ti *treeIndex) map[string][]string {
	children := make(map[string][]string)
	add := func(path string) {
		d := filepath.Dir(path)
		children[d] = append(children[d], path)
	}
	for path := range ti.Trees {
		add(path)
	}
	for path := range ti.Blobs {
		add(path)
	}
	for path := range ti.Gitlinks {
		add(path)
	}
	return children
}

// coverage answers, for a path, whether the plan cloned it.
type coverage struct {
	files map[string]bool
	dirs  map[string]bool
}

func (p clonePlan) coverage() coverage {
	c := coverage{
		files: make(map[string]bool, len(p.Files)),
		dirs:  make(map[string]bool, len(p.Dirs)),
	}
	for _, f := range p.Files {
		c.files[f] = true
	}
	for _, d := range p.Dirs {
		c.dirs[d] = true
	}
	return c
}

// covers reports whether path was cloned, either on its own or as part of
// a directory clone.
func (c coverage) covers(path string) bool {
	if c.files[path] {
		return true
	}
	for d := filepath.Dir(path); d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
		if c.dirs[d] {
			return true
		}
	}
	return false
}

// seedResult summarizes a reflink seeding pass.
type seedResult struct {
	Dirs    int   // directories cloned whole
	Files   int   // files cloned individually
	StopErr error // error that caused an early stop, if any
}

// clone reflinks plan from srcRoot into dstRoot. It stops at the first
// ENOTSUP/EOPNOTSUPP/EXDEV (CoW unsupported for this src/dst pair); other
// per-item errors are ignored, because the final checkout writes anything
// we failed to produce.
//
// We rely on `git worktree add --no-checkout` leaving the working tree
// empty, so destinations don't pre-exist and we can skip the per-file
// stat/remove dance. Parent directories are pre-created once rather than
// MkdirAll'd per item.
//
// Nothing here is trusted: validateClones re-hashes every cloned file
// before we tell Git it is up to date.
func (p clonePlan) clone(srcRoot, dstRoot string) seedResult {
	var r seedResult
	if os.Getenv("GIT_COW_WORKTREE_FORCE_NO_COW") != "" {
		r.StopErr = errCoWUnsupported
		return r
	}
	if len(p.Dirs) == 0 && len(p.Files) == 0 {
		return r
	}

	for _, d := range parentDirs(p.Dirs, p.Files) {
		if err := os.MkdirAll(filepath.Join(dstRoot, d), 0o777); err != nil {
			r.StopErr = fmt.Errorf("mkdir %s: %w", d, err)
			return r
		}
	}

	type item struct {
		path  string
		isDir bool
	}
	work := make(chan item)
	// Cloning is metadata-bound and the filesystem serializes much of it,
	// so a big worker pool burns CPU without buying wall-clock time.
	workers := min(runtime.NumCPU(), 8, len(p.Dirs)+len(p.Files))
	var wg sync.WaitGroup
	var dirs, files atomic.Int64
	var stopOnce sync.Once
	var stopErr atomic.Pointer[error]

	for range workers {
		wg.Go(func() {
			for it := range work {
				if stopErr.Load() != nil {
					continue // drain
				}
				src := filepath.Join(srcRoot, it.path)
				dst := filepath.Join(dstRoot, it.path)
				var err error
				if it.isDir {
					err = cowCloneDir(src, dst)
				} else {
					err = cowClone(src, dst)
				}
				switch {
				case err == nil:
					if it.isDir {
						dirs.Add(1)
					} else {
						files.Add(1)
					}
				case isCoWUnsupported(err):
					stopOnce.Do(func() {
						e := err
						stopErr.Store(&e)
					})
				default:
					// Per-item failure (source file missing, permissions,
					// a partially cloned directory): leave it to checkout.
				}
			}
		})
	}

	for _, d := range p.Dirs {
		work <- item{path: d, isDir: true}
	}
	for _, f := range p.Files {
		work <- item{path: f}
	}
	close(work)
	wg.Wait()

	r.Dirs = int(dirs.Load())
	r.Files = int(files.Load())
	if e := stopErr.Load(); e != nil {
		r.StopErr = *e
	}
	return r
}

// parentDirs returns every distinct parent directory of the given paths.
// MkdirAll handles ordering, so we don't bother sorting.
func parentDirs(paths ...[]string) []string {
	seen := make(map[string]struct{})
	for _, group := range paths {
		for _, p := range group {
			d := filepath.Dir(p)
			for d != "." && d != string(filepath.Separator) && d != "" {
				if _, ok := seen[d]; ok {
					break
				}
				seen[d] = struct{}{}
				d = filepath.Dir(d)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	return out
}

// String renders a plan for verbose output.
func (p clonePlan) String() string {
	return fmt.Sprintf("%d dirs, %d files", len(p.Dirs), len(p.Files))
}
