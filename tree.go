package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// TreeEntry is one blob row of `git ls-tree -r`.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000"
	SHA  string
}

// IsRegular reports whether the entry is a plain file, i.e. something we
// can reflink. Symlinks and submodules are left to Git.
func (te TreeEntry) IsRegular() bool {
	return te.Mode == "100644" || te.Mode == "100755"
}

// treeIndex is the full recursive listing of a commit's tree: every blob,
// every subtree, and every submodule reference, keyed by path.
//
// Subtree SHAs are what make the cloning cheap: when a directory has the
// same SHA on both sides, its entire contents match, and on filesystems
// that can clone directories that's one syscall instead of one per file.
type treeIndex struct {
	Blobs    map[string]TreeEntry // regular files and symlinks
	Trees    map[string]string    // directory path -> tree SHA
	Gitlinks map[string]string    // submodule path -> commit SHA
}

// lsTree lists ref's tree in repoDir. Uses -z for safety with weird filenames.
func lsTree(repoDir, ref string) (*treeIndex, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "-t", "-z", ref)
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w (%s)", ref, err, strings.TrimSpace(stderr.String()))
	}
	ti := &treeIndex{
		Blobs:    make(map[string]TreeEntry),
		Trees:    make(map[string]string),
		Gitlinks: make(map[string]string),
	}
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
		mode, typ, sha := string(fields[0]), string(fields[1]), string(fields[2])
		switch typ {
		case "blob":
			ti.Blobs[string(path)] = TreeEntry{Mode: mode, SHA: sha}
		case "tree":
			ti.Trees[string(path)] = sha
		case "commit":
			ti.Gitlinks[string(path)] = sha
		}
	}
	return ti, nil
}

// filterSparseCheckout drops blobs that Git's sparse-checkout rules would not
// materialize in repoDir. When sparse checkout is disabled, it returns the
// tree unchanged.
func filterSparseCheckout(repoDir string, tree *treeIndex) (*treeIndex, error) {
	enabled, err := sparseCheckoutEnabled(repoDir)
	if err != nil || !enabled || len(tree.Blobs) == 0 {
		return tree, err
	}

	paths := make([]string, 0, len(tree.Blobs))
	for path := range tree.Blobs {
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

	filtered := &treeIndex{
		Blobs:    make(map[string]TreeEntry),
		Trees:    tree.Trees,
		Gitlinks: tree.Gitlinks,
	}
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		path := string(rec)
		if entry, ok := tree.Blobs[path]; ok {
			filtered.Blobs[path] = entry
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

// sourceExclusions returns paths in the source worktree that must not be
// cloned into the new one: tracked files whose working-tree copy no longer
// matches the commit we're cloning from, and untracked or ignored paths,
// which belong to the source worktree alone.
//
// Wholly-untracked directories come back as a single entry rather than one
// per file, which is what keeps this cheap in a repo with a big build cache.
// The dirty count it reports back is the tracked half alone, which is what
// tells a normal worktree apart from one that has no business being cloned
// from at all.
func sourceExclusions(worktree, ref string) (excluded map[string]bool, dirty int, err error) {
	excluded = make(map[string]bool)
	collect := func(args ...string) (int, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = worktree
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return 0, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		n := 0
		for rec := range bytes.SplitSeq(out, []byte{0}) {
			if len(rec) == 0 {
				continue
			}
			excluded[strings.TrimSuffix(string(rec), "/")] = true
			n++
		}
		return n, nil
	}
	// Tracked files whose working-tree copy differs from ref, staged or
	// not. This trusts the source worktree's stat cache, but only as a
	// filter: everything we clone is content-verified afterwards.
	dirty, err = collect("diff-index", "-z", "--name-only", ref)
	if err != nil {
		return nil, 0, err
	}
	// Untracked paths. Without --exclude-standard this covers ignored
	// files too, which is what we want: build output belongs to the
	// worktree that produced it.
	if _, err := collect("ls-files", "-z", "--others", "--directory", "--no-empty-directory"); err != nil {
		return nil, 0, err
	}
	return excluded, dirty, nil
}

// taint returns the set of directories that contain an excluded path, so
// they can be descended into rather than cloned wholesale.
func taint(excluded map[string]bool) map[string]bool {
	tainted := make(map[string]bool, len(excluded))
	for p := range excluded {
		for d := filepath.Dir(p); d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
			if tainted[d] {
				break // and so are all of its parents
			}
			tainted[d] = true
		}
	}
	return tainted
}
