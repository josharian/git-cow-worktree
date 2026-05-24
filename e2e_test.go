package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// e2e tests exec real `git` against repos built in t.TempDir().
// They verify the core drop-in invariant: after `git-cow-worktree add` and
// `git worktree add` against equivalent setups, the resulting worktrees'
// tracked-file contents are byte-identical.

func TestE2E_DropInInvariant(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	scenarios := []struct {
		name  string
		setup func(t *testing.T, r *repo)
		// commitIsh for `add`. When empty, git-cow-worktree infers from -b or current
		// branch. We always use a branch name pointing at a specific commit
		// so target ref is deterministic.
		target string
		// Source picker: we use --from to make tests deterministic regardless
		// of mtime ordering. setup returns the path to use.
		fromName string // the branch name that has a worktree we want to use as source
	}{
		{
			name: "identity-source-equals-target",
			setup: func(t *testing.T, r *repo) {
				r.commit("a.txt", "alpha")
				r.commit("b.txt", "beta")
				r.commit("dir/c.txt", "gamma")
				r.branch("identity", "HEAD")
				r.worktree("src", "identity")
			},
			target:   "identity",
			fromName: "src",
		},
		{
			name: "source-one-commit-behind",
			setup: func(t *testing.T, r *repo) {
				r.commit("a.txt", "v1")
				r.commit("dir/b.txt", "shared")
				r.branch("base", "HEAD")
				r.worktree("src", "base")
				r.commit("a.txt", "v2")
				r.branch("target", "HEAD")
			},
			target:   "target",
			fromName: "src",
		},
		{
			name: "source-has-file-deleted-in-target",
			setup: func(t *testing.T, r *repo) {
				r.commit("keep.txt", "kept")
				r.commit("delete-me.txt", "doomed")
				r.branch("base", "HEAD")
				r.worktree("src", "base")
				r.rm("delete-me.txt")
				r.commitAll("remove file")
				r.branch("target", "HEAD")
			},
			target:   "target",
			fromName: "src",
		},
		{
			name: "source-missing-file-added-in-target",
			setup: func(t *testing.T, r *repo) {
				r.commit("keep.txt", "kept")
				r.branch("base", "HEAD")
				r.worktree("src", "base")
				r.commit("new.txt", "newcontent")
				r.branch("target", "HEAD")
			},
			target:   "target",
			fromName: "src",
		},
		{
			name: "executable-bit-flip",
			setup: func(t *testing.T, r *repo) {
				r.commitMode("script.sh", "#!/bin/sh\necho hi\n", 0o644)
				r.branch("base", "HEAD")
				r.worktree("src", "base")
				r.chmod("script.sh", 0o755)
				r.commitAll("make executable")
				r.branch("target", "HEAD")
			},
			target:   "target",
			fromName: "src",
		},
		{
			name: "symlink-in-tree",
			setup: func(t *testing.T, r *repo) {
				r.commit("real.txt", "realcontent")
				r.symlink("link.txt", "real.txt")
				r.commitAll("add symlink")
				r.branch("target", "HEAD")
				r.worktree("src", "target")
			},
			target:   "target",
			fromName: "src",
		},
		{
			name: "dirty-source-clean-target",
			setup: func(t *testing.T, r *repo) {
				r.commit("a.txt", "clean-content")
				r.branch("target", "HEAD")
				r.worktree("src", "target")
				// Dirty modification in source's working tree.
				r.writeAt("src", "a.txt", "DIRTY-LOCAL-CHANGES")
			},
			target:   "target",
			fromName: "src",
		},
		{
			name: "many-files-with-overlap",
			setup: func(t *testing.T, r *repo) {
				for i := range 30 {
					r.commit(filepath.Join("pkg", "f"+itoa(i)+".txt"), "shared-"+itoa(i))
				}
				r.branch("base", "HEAD")
				r.worktree("src", "base")
				// Change a handful of files.
				for i := range 5 {
					r.commit(filepath.Join("pkg", "f"+itoa(i)+".txt"), "changed-"+itoa(i))
				}
				r.branch("target", "HEAD")
			},
			target:   "target",
			fromName: "src",
		},
	}

	gitCowWorktree := buildGitCowWorktree(t)

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// Two parallel scratch repos to avoid cross-contamination of
			// worktrees between the git-cow-worktree side and the plain side.
			cowRepo := newRepo(t, "cow")
			sc.setup(t, cowRepo)
			plainRepo := newRepo(t, "plain")
			sc.setup(t, plainRepo)

			cowOut := filepath.Join(cowRepo.parent, "out-cow")
			plainOut := filepath.Join(plainRepo.parent, "out-plain")

			cowSource := filepath.Join(cowRepo.parent, sc.fromName)
			plainSource := filepath.Join(plainRepo.parent, sc.fromName)
			_ = plainSource // unused on plain side; plain just runs git worktree add

			// git-cow-worktree add -v --from <source> --detach <out> <target>
			// --detach avoids branch-collision when the source worktree
			// happens to be on the target branch.
			cmd := exec.Command(gitCowWorktree, "add", "-v", "--from", cowSource, "--detach", cowOut, sc.target)
			cmd.Dir = cowRepo.dir
			var cowStderr bytes.Buffer
			cmd.Stderr = &cowStderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("git-cow-worktree add: %v\n%s", err, cowStderr.String())
			}
			t.Logf("git-cow-worktree -v output:\n%s", cowStderr.String())

			// git worktree add --detach <out> <target>
			cmd2 := exec.Command("git", "worktree", "add", "--detach", plainOut, sc.target)
			cmd2.Dir = plainRepo.dir
			out, err := cmd2.CombinedOutput()
			if err != nil {
				t.Fatalf("git worktree add: %v\n%s", err, string(out))
			}

			// Diff the tracked-file trees. We ignore .git (it's a file
			// inside a worktree, pointing to different parent dirs).
			diffWorktrees(t, plainOut, cowOut)
		})
	}
}

// TestE2E_NoCowFilesystem forces the binary down the "CoW unsupported"
// path via the GIT_COW_WORKTREE_FORCE_NO_COW env var, verifying graceful
// degradation to a plain checkout.
func TestE2E_NoCowFilesystem(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	gitCowWorktree := buildGitCowWorktree(t)

	cowRepo := newRepo(t, "cow")
	cowRepo.commit("a.txt", "alpha")
	cowRepo.commit("b.txt", "beta")
	cowRepo.branch("target", "HEAD")
	cowRepo.worktree("src", "target")

	plainRepo := newRepo(t, "plain")
	plainRepo.commit("a.txt", "alpha")
	plainRepo.commit("b.txt", "beta")
	plainRepo.branch("target", "HEAD")
	plainRepo.worktree("src", "target")

	cowOut := filepath.Join(cowRepo.parent, "out-cow")
	plainOut := filepath.Join(plainRepo.parent, "out-plain")

	cmd := exec.Command(gitCowWorktree, "add", "-v", "--from", filepath.Join(cowRepo.parent, "src"), "--detach", cowOut, "target")
	cmd.Dir = cowRepo.dir
	cmd.Env = append(os.Environ(), "GIT_COW_WORKTREE_FORCE_NO_COW=1")
	var cowStderr bytes.Buffer
	cmd.Stderr = &cowStderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git-cow-worktree add: %v\n%s", err, cowStderr.String())
	}
	t.Logf("git-cow-worktree output:\n%s", cowStderr.String())
	if !strings.Contains(cowStderr.String(), "reflinked 0/") &&
		!strings.Contains(cowStderr.String(), "stopped reflinking") {
		t.Errorf("expected verbose output to show 0 reflinks or stop, got:\n%s", cowStderr.String())
	}

	cmd2 := exec.Command("git", "worktree", "add", "--detach", plainOut, "target")
	cmd2.Dir = plainRepo.dir
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, string(out))
	}
	diffWorktrees(t, plainOut, cowOut)
}

// TestE2E_AutoPickSource verifies the drop-in invariant when git-cow-worktree
// auto-selects the source worktree (no --from flag).
func TestE2E_AutoPickSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	mkRepo := func() *repo {
		r := newRepo(t, "auto")
		r.commit("a.txt", "alpha")
		r.commit("dir/b.txt", "beta")
		r.branch("base", "HEAD")
		// Source candidate on base.
		r.worktree("src", "base")
		// Diverge to create target.
		r.commit("a.txt", "alpha-v2")
		r.branch("target", "HEAD")
		return r
	}

	cowRepo := mkRepo()
	plainRepo := mkRepo()

	cowOut := filepath.Join(cowRepo.parent, "out-cow")
	plainOut := filepath.Join(plainRepo.parent, "out-plain")

	cmd := exec.Command(gitCowWorktree, "add", "-v", "--detach", cowOut, "target")
	cmd.Dir = cowRepo.dir
	var sb bytes.Buffer
	cmd.Stderr = &sb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git-cow-worktree add: %v\n%s", err, sb.String())
	}
	t.Logf("git-cow-worktree output:\n%s", sb.String())
	if !strings.Contains(sb.String(), "source=") {
		t.Errorf("expected auto-picked source in verbose output, got:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), "reflinked ") {
		t.Errorf("expected reflink count in verbose output, got:\n%s", sb.String())
	}

	cmd2 := exec.Command("git", "worktree", "add", "--detach", plainOut, "target")
	cmd2.Dir = plainRepo.dir
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, string(out))
	}
	diffWorktrees(t, plainOut, cowOut)
}

func TestE2E_SparseCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	mkRepo := func(label string) *repo {
		r := newRepo(t, label)
		r.commit("root.txt", "root")
		r.commit("dir/a.txt", "alpha")
		r.commit("other/b.txt", "beta")
		r.branch("target", "HEAD")
		// Keep a full source worktree, then make the main worktree sparse.
		r.worktree("src", "target")
		r.sparseCheckout("dir")
		return r
	}

	cowRepo := mkRepo("cow")
	plainRepo := mkRepo("plain")

	cowOut := filepath.Join(cowRepo.parent, "out-cow")
	plainOut := filepath.Join(plainRepo.parent, "out-plain")

	cmd := exec.Command(gitCowWorktree, "add", "--from", filepath.Join(cowRepo.parent, "src"), "--detach", cowOut, "target")
	cmd.Dir = cowRepo.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git-cow-worktree add: %v\n%s", err, string(out))
	}

	cmd2 := exec.Command("git", "worktree", "add", "--detach", plainOut, "target")
	cmd2.Dir = plainRepo.dir
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, string(out))
	}

	diffWorktrees(t, plainOut, cowOut)
	if _, err := os.Stat(filepath.Join(cowOut, "other", "b.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("sparse-excluded file should not be materialized, got err=%v", err)
	}
}

// TestE2E_NoCheckoutPassthrough verifies that --no-checkout exec's git
// worktree add and produces an empty (--no-checkout) worktree.
func TestE2E_NoCheckoutPassthrough(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	r := newRepo(t, "main")
	r.commit("a.txt", "alpha")
	r.branch("target", "HEAD")

	out := filepath.Join(r.parent, "out")
	cmd := exec.Command(gitCowWorktree, "add", "-v", "--from", filepath.Join(r.parent, "ignored"), "--no-checkout", out, "target")
	cmd.Dir = r.dir
	if got, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git-cow-worktree add --no-checkout: %v\n%s", err, string(got))
	}

	// With --no-checkout, the working tree should NOT have a.txt
	if _, err := os.Stat(filepath.Join(out, "a.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected a.txt to be absent under --no-checkout, got err=%v", err)
	}
	// But .git should exist.
	if _, err := os.Stat(filepath.Join(out, ".git")); err != nil {
		t.Errorf("expected .git in worktree: %v", err)
	}
}

func TestE2E_OrphanPassthrough(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	r := newRepo(t, "main")
	r.commit("a.txt", "alpha")

	out := filepath.Join(r.parent, "orphan")
	cmd := exec.Command(gitCowWorktree, "add", "-v", "--from", filepath.Join(r.parent, "ignored"), "--orphan", out)
	cmd.Dir = r.dir
	if got, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git-cow-worktree add --orphan: %v\n%s", err, string(got))
	}
	if _, err := os.Stat(filepath.Join(out, ".git")); err != nil {
		t.Errorf("expected .git in orphan worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "a.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("orphan worktree should not check out tracked files, got err=%v", err)
	}
}

func TestE2E_ReasonFlag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	r := newRepo(t, "main")
	r.commit("a.txt", "alpha")
	r.branch("target", "HEAD")

	out := filepath.Join(r.parent, "locked")
	cmd := exec.Command(gitCowWorktree, "add", "--lock", "--reason", "because", "--detach", out, "target")
	cmd.Dir = r.dir
	if got, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git-cow-worktree add --lock --reason: %v\n%s", err, string(got))
	}
	if _, err := os.Stat(filepath.Join(out, "a.txt")); err != nil {
		t.Errorf("expected checked out file in locked worktree: %v", err)
	}
}

func TestE2E_InvalidFromFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	r := newRepo(t, "main")
	r.commit("a.txt", "alpha")
	r.branch("target", "HEAD")

	out := filepath.Join(r.parent, "out")
	cmd := exec.Command(gitCowWorktree, "add", "--from", filepath.Join(r.parent, "missing"), "--detach", out, "target")
	cmd.Dir = r.dir
	got, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected invalid --from to fail, got success:\n%s", string(got))
	}
	if !strings.Contains(string(got), "--from") {
		t.Fatalf("expected --from error, got:\n%s", string(got))
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("target worktree should not be created after invalid --from, got err=%v", statErr)
	}
}

func TestE2E_PostCheckoutHookArgs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	r := newRepo(t, "main")
	r.commit("a.txt", "alpha")
	r.branch("target", "HEAD")
	r.worktree("src", "target")

	hookLog := filepath.Join(r.parent, "hook.log")
	hookPath := filepath.Join(r.dir, ".git", "hooks", "post-checkout")
	hook := "#!/bin/sh\n" +
		"printf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> " + shellQuote(hookLog) + "\n"
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(r.parent, "out")
	cmd := exec.Command(gitCowWorktree, "add", "--from", filepath.Join(r.parent, "src"), "--detach", out, "target")
	cmd.Dir = r.dir
	if got, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git-cow-worktree add: %v\n%s", err, string(got))
	}

	gotBytes, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(gotBytes)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one post-checkout hook invocation, got %d:\n%s", len(lines), string(gotBytes))
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 3 {
		t.Fatalf("unexpected hook log line: %q", lines[0])
	}
	if fields[1] != gitOutput(t, out, "rev-parse", "HEAD") {
		t.Fatalf("new HEAD = %s, want target HEAD", fields[1])
	}
	if fields[0] != zeroObjectIDFor(fields[1]) {
		t.Fatalf("old HEAD = %s, want %s", fields[0], zeroObjectIDFor(fields[1]))
	}
	if fields[2] != "1" {
		t.Fatalf("checkout flag = %s, want 1", fields[2])
	}
}

func TestE2E_PostCheckoutHookArgsSHA256(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	gitCowWorktree := buildGitCowWorktree(t)

	r := newRepoSHA256(t, "main")
	r.commit("a.txt", "alpha")
	r.branch("target", "HEAD")
	r.worktree("src", "target")

	hookLog := filepath.Join(r.parent, "hook.log")
	hookPath := filepath.Join(r.dir, ".git", "hooks", "post-checkout")
	hook := "#!/bin/sh\n" +
		"printf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> " + shellQuote(hookLog) + "\n"
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(r.parent, "out")
	cmd := exec.Command(gitCowWorktree, "add", "--from", filepath.Join(r.parent, "src"), "--detach", out, "target")
	cmd.Dir = r.dir
	if got, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git-cow-worktree add: %v\n%s", err, string(got))
	}

	gotBytes, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(gotBytes)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one post-checkout hook invocation, got %d:\n%s", len(lines), string(gotBytes))
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 3 {
		t.Fatalf("unexpected hook log line: %q", lines[0])
	}
	targetSHA := gitOutput(t, out, "rev-parse", "HEAD")
	if len(targetSHA) != 64 {
		t.Fatalf("target object ID length = %d, want 64", len(targetSHA))
	}
	if fields[1] != targetSHA {
		t.Fatalf("new HEAD = %s, want %s", fields[1], targetSHA)
	}
	if fields[0] != zeroObjectIDFor(targetSHA) {
		t.Fatalf("old HEAD = %s, want %s", fields[0], zeroObjectIDFor(targetSHA))
	}
	if fields[2] != "1" {
		t.Fatalf("checkout flag = %s, want 1", fields[2])
	}
}

func TestParseArgsCompatibility(t *testing.T) {
	opts, err := parseArgs([]string{"--lock", "--reason", "because", "--detach", "out", "target"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.TargetPath != "out" || opts.CommitIsh != "target" {
		t.Fatalf("unexpected positionals: path=%q commit=%q", opts.TargetPath, opts.CommitIsh)
	}
	if !equalStringSlices(opts.Forward, []string{"--lock", "--reason", "because", "--detach"}) {
		t.Fatalf("unexpected forwarded args: %v", opts.Forward)
	}

	opts, err = parseArgs([]string{"-v", "--from", "ignored", "--no-checkout", "out", "target"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.NoCheckout || !opts.Verbose || !opts.FromSpecified {
		t.Fatalf("unexpected parsed flags: %+v", opts)
	}
	if !equalStringSlices(opts.GitArgs, []string{"--no-checkout", "out", "target"}) {
		t.Fatalf("unexpected passthrough args: %v", opts.GitArgs)
	}

	opts, err = parseArgs([]string{"--checkout", "out", "target"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.NoCheckout || len(opts.Forward) != 0 {
		t.Fatalf("--checkout should be normalized out of optimized path: %+v", opts)
	}

	opts, err = parseArgs([]string{"--", "-dash", "target"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.UseSeparator || opts.TargetPath != "-dash" || opts.CommitIsh != "target" {
		t.Fatalf("separator not preserved in parse: %+v", opts)
	}
	if !equalStringSlices(opts.GitArgs, []string{"--", "-dash", "target"}) {
		t.Fatalf("separator not preserved in passthrough args: %v", opts.GitArgs)
	}
}

func TestPathCanonicalizationResolvesSymlink(t *testing.T) {
	realParent := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	realRepo := filepath.Join(realParent, "repo")
	if err := os.MkdirAll(filepath.Join(realRepo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRepo := filepath.Join(linkParent, "repo")

	if !samePath(realRepo, linkedRepo) {
		t.Fatalf("samePath(%q, %q) = false, want true", realRepo, linkedRepo)
	}
	if !pathHasPrefix(filepath.Join(linkedRepo, "sub"), realRepo) {
		t.Fatalf("pathHasPrefix(%q, %q) = false, want true", filepath.Join(linkedRepo, "sub"), realRepo)
	}
}

// --- helpers ---

func buildGitCowWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "git-cow-worktree")
	cmd := exec.Command("go", "build", "-o", out, ".")
	if got, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, string(got))
	}
	return out
}

type repo struct {
	t      *testing.T
	parent string // tempdir containing dir and any worktrees
	dir    string // main repo path
}

func newRepo(t *testing.T, label string) *repo {
	t.Helper()
	return newRepoWithGitInitArgs(t, label, false)
}

func newRepoSHA256(t *testing.T, label string) *repo {
	t.Helper()
	return newRepoWithGitInitArgs(t, label, true, "--object-format=sha256")
}

func newRepoWithGitInitArgs(t *testing.T, label string, skipOnInitError bool, initArgs ...string) *repo {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "repo-"+label)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &repo{t: t, parent: parent, dir: dir}
	args := append([]string{"git", "init", "-q", "-b", "main"}, initArgs...)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		if skipOnInitError {
			t.Skipf("%s: %v\n%s", strings.Join(args, " "), err, string(out))
		}
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	r.run("git", "config", "user.email", "test@example.com")
	r.run("git", "config", "user.name", "Test")
	r.run("git", "config", "commit.gpgsign", "false")
	return r
}

func (r *repo) run(args ...string) {
	r.t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) commit(path, content string) {
	r.commitMode(path, content, 0o644)
}

func (r *repo) commitMode(path, content string, mode os.FileMode) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		r.t.Fatal(err)
	}
	r.run("git", "add", path)
	r.run("git", "commit", "-qm", "add "+path)
}

func (r *repo) commitAll(msg string) {
	r.run("git", "add", "-A")
	r.run("git", "commit", "-qm", msg)
}

func (r *repo) rm(path string) {
	r.t.Helper()
	r.run("git", "rm", "-q", path)
}

func (r *repo) chmod(path string, mode os.FileMode) {
	r.t.Helper()
	if err := os.Chmod(filepath.Join(r.dir, path), mode); err != nil {
		r.t.Fatal(err)
	}
	// git records the executable bit even if the filesystem doesn't, but
	// we want to be explicit:
	exec := "+x"
	if mode&0o111 == 0 {
		exec = "-x"
	}
	r.run("git", "update-index", "--chmod="+exec, path)
}

func (r *repo) symlink(linkPath, target string) {
	r.t.Helper()
	full := filepath.Join(r.dir, linkPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		r.t.Fatal(err)
	}
	r.run("git", "add", linkPath)
}

func (r *repo) branch(name, ref string) {
	r.run("git", "branch", "-f", name, ref)
}

func (r *repo) worktree(name, ref string) {
	r.t.Helper()
	path := filepath.Join(r.parent, name)
	cmd := exec.Command("git", "worktree", "add", "-q", path, ref)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git worktree add %s %s: %v\n%s", path, ref, err, string(out))
	}
}

func (r *repo) sparseCheckout(paths ...string) {
	r.t.Helper()
	r.run("git", "sparse-checkout", "init", "--cone")
	args := append([]string{"git", "sparse-checkout", "set"}, paths...)
	r.run(args...)
}

// writeAt writes to a path inside an existing worktree under r.parent
// without committing — i.e., produces a dirty working tree.
func (r *repo) writeAt(worktreeName, path, content string) {
	r.t.Helper()
	full := filepath.Join(r.parent, worktreeName, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// diffWorktrees asserts that two worktree paths have identical tracked
// content (everything except their .git entries).
func diffWorktrees(t *testing.T, a, b string) {
	t.Helper()
	mapA := snapshot(t, a)
	mapB := snapshot(t, b)

	keysA := sortedKeys(mapA)
	keysB := sortedKeys(mapB)

	if !equalStringSlices(keysA, keysB) {
		t.Errorf("file sets differ\nonly in %s: %v\nonly in %s: %v",
			a, diff(keysA, keysB), b, diff(keysB, keysA))
		return
	}
	for _, k := range keysA {
		if mapA[k] != mapB[k] {
			t.Errorf("file %s differs:\n  %s: %s\n  %s: %s", k, a, mapA[k], b, mapB[k])
		}
	}
}

type fileSig struct {
	mode    string // "f" regular / "l" symlink / "d" dir; suffix executable bit for regular
	content string // for regular: sha256 hex; for symlink: target
}

func (s fileSig) String() string {
	return s.mode + ":" + s.content
}

func snapshot(t *testing.T, root string) map[string]fileSig {
	t.Helper()
	m := make(map[string]fileSig)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		// Skip .git (either dir in main repo, or file in worktree).
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			tgt, err := os.Readlink(path)
			if err != nil {
				return err
			}
			m[rel] = fileSig{mode: "l", content: tgt}
		case info.IsDir():
			// Directories are implicit in the file set; skip.
		case info.Mode().IsRegular():
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				return err
			}
			f.Close()
			tag := "f"
			if info.Mode().Perm()&0o111 != 0 {
				tag = "fx"
			}
			m[rel] = fileSig{mode: tag, content: bytesToHex(h.Sum(nil))}
		default:
			m[rel] = fileSig{mode: "?", content: info.Mode().String()}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diff(a, b []string) []string {
	bset := make(map[string]struct{}, len(b))
	for _, s := range b {
		bset[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := bset[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	return string(buf[pos:])
}

func bytesToHex(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xf]
	}
	return string(out)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
