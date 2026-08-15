package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	commandName        = "git-cow-worktree"
	maxOtherCandidates = 8
)

// Options collected from argv after light parsing.
type Options struct {
	Verbose       bool
	FromSpecified bool
	FromPath      string   // --from <path>; empty means auto-pick
	NoCheckout    bool     // final --no-checkout state (force passthrough)
	Orphan        bool     // final --orphan state (force passthrough)
	Forward       []string // flags+args forwarded to our internal `git worktree add`
	GitArgs       []string // sanitized args forwarded to passthrough `git worktree add`
	UseSeparator  bool     // whether positional args were introduced by --
	TargetPath    string   // positional <path>
	CommitIsh     string   // positional <commit-ish>, may be empty
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "add" {
		fmt.Fprintf(os.Stderr, "usage: %s add [-b <new-branch>] [-B <branch>] [--from <path>] [-v] <path> [<commit-ish>]\n", commandName)
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, commandName+":", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}

	// --no-checkout and --orphan cannot use our seed-then-checkout flow.
	if opts.NoCheckout || opts.Orphan {
		return execGitWorktreeAdd(opts.GitArgs)
	}

	if opts.TargetPath == "" {
		return errors.New("missing <path> argument")
	}

	repoDir, err := os.Getwd()
	if err != nil {
		return err
	}

	var explicitSource Worktree
	if opts.FromSpecified {
		explicitSource, err = resolveFromPath(opts.FromPath)
		if err != nil {
			return err
		}
	}

	overallStart := time.Now()
	phase := func(name string, t0 time.Time) {
		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "%s: %-20s %v\n", commandName, name, time.Since(t0).Round(time.Millisecond))
		}
	}

	// Step 2: git worktree add --no-checkout [user flags].
	addArgs := append([]string{"worktree", "add", "--no-checkout"}, opts.Forward...)
	if opts.UseSeparator {
		addArgs = append(addArgs, "--")
	}
	addArgs = append(addArgs, opts.TargetPath)
	if opts.CommitIsh != "" {
		addArgs = append(addArgs, opts.CommitIsh)
	}
	t := time.Now()
	if err := runGitStreaming(repoDir, addArgs...); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	phase("worktree add", t)

	targetAbs, err := osAbs(opts.TargetPath)
	if err != nil {
		return err
	}

	// Resolve target SHA from inside the new worktree (HEAD is now set).
	targetSHA, err := resolveRev(targetAbs, "HEAD")
	if err != nil {
		return err
	}

	// Step 3: pick source.
	t = time.Now()
	var src Worktree
	var srcOK bool
	var scoreVal int
	if opts.FromSpecified {
		src = explicitSource
		srcOK = true
		scoreVal, _ = commitDistance(repoDir, src.HEAD, targetSHA)
	} else {
		src, srcOK, scoreVal = pickAutoSource(repoDir, targetAbs, targetSHA, opts.Verbose)
	}
	phase("pick source", t)
	var seededIndex string
	if srcOK {
		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "%s: source=%s (diff=%d commits)\n", commandName, src.Path, scoreVal)
		}
		var err error
		seededIndex, err = seed(src, targetAbs, targetSHA, phase, opts.Verbose)
		if err != nil {
			// Seeding is an optimization: anything that goes wrong here
			// leaves the working tree to the checkout below.
			if opts.Verbose {
				fmt.Fprintf(os.Stderr, "%s: seeding skipped: %v\n", commandName, err)
			}
		}
	} else if opts.Verbose {
		fmt.Fprintf(os.Stderr, "%s: no viable source worktree; using plain checkout\n", commandName)
	}

	// Step 6: finalize.
	t = time.Now()
	if err := finalizeCheckout(targetAbs, targetSHA); err != nil {
		if seededIndex == "" {
			return err
		}
		// The index we wrote is the only thing here Git didn't produce
		// itself. Discard it and let Git check the worktree out from
		// scratch rather than leaving the user a half-populated one.
		// A checkout that died mid-flight leaves its lock behind; in a
		// worktree this new, nobody else can own it.
		fmt.Fprintf(os.Stderr, "%s: checkout failed against seeded index (%v); retrying from scratch\n", commandName, err)
		for _, path := range []string{seededIndex, seededIndex + ".lock"} {
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				return err
			}
		}
		if err := finalizeCheckout(targetAbs, targetSHA); err != nil {
			return err
		}
	}
	phase("checkout", t)

	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "%s: %-20s %v\n", commandName, "total", time.Since(overallStart).Round(time.Millisecond))
	}
	return nil
}

// seed reflinks as much of the new worktree as it can from src, and writes
// an index vouching for what it reflinked so the checkout that follows
// leaves those files — and their shared storage — alone. It returns the
// path of the index it wrote, if any.
//
// Every step is best-effort. Returning an error, or returning nothing,
// means the caller's checkout writes the worktree the ordinary way.
func seed(src Worktree, targetAbs, targetSHA string, phase func(string, time.Time), verbose bool) (string, error) {
	t := time.Now()
	targetTree, err := lsTree(targetAbs, targetSHA)
	if err != nil {
		return "", err
	}
	sparse, err := sparseCheckoutEnabled(targetAbs)
	if err != nil {
		return "", err
	}
	if sparse {
		// Only some of a directory's files get materialized, so cloning
		// whole directories would import files the user excluded.
		targetTree, err = filterSparseCheckout(targetAbs, targetTree)
		if err != nil {
			return "", err
		}
	}
	sourceTree, err := lsTree(src.Path, src.HEAD)
	if err != nil {
		return "", err
	}
	excluded, err := sourceExclusions(src.Path, src.HEAD)
	if err != nil {
		return "", err
	}
	phase("inspect trees", t)

	// GIT_COW_WORKTREE_NO_DIR_CLONE exercises the file-by-file path, which
	// is what Linux always takes, on a filesystem that can clone directories.
	cloneDirs := canCloneDirs && !sparse && os.Getenv("GIT_COW_WORKTREE_NO_DIR_CLONE") == ""

	t = time.Now()
	plan := planClones(sourceTree, targetTree, excluded, cloneDirs)
	phase("plan", t)

	t = time.Now()
	result := plan.clone(src.Path, targetAbs)
	phase("clone", t)
	if verbose {
		fmt.Fprintf(os.Stderr, "%s: planned %s; cloned %d dirs, %d files\n",
			commandName, plan, result.Dirs, result.Files)
	}
	if result.StopErr != nil {
		return "", fmt.Errorf("stopped cloning: %w", result.StopErr)
	}
	if result.Dirs == 0 && result.Files == 0 {
		return "", nil
	}

	t = time.Now()
	entries := validateClones(targetAbs, targetTree, plan)
	phase("validate", t)
	if verbose {
		fmt.Fprintf(os.Stderr, "%s: verified %d files\n", commandName, len(entries))
	}
	if len(entries) == 0 {
		return "", nil
	}

	t = time.Now()
	path, err := indexPath(targetAbs)
	if err != nil {
		return "", err
	}
	if err := writeIndex(path, entries); err != nil {
		return "", err
	}
	phase("write index", t)
	return path, nil
}

func resolveFromPath(fromPath string) (Worktree, error) {
	abs, err := osAbs(fromPath)
	if err != nil {
		return Worktree{}, fmt.Errorf("--from invalid: %w", err)
	}
	sha, err := resolveRev(abs, "HEAD")
	if err != nil {
		return Worktree{}, fmt.Errorf("--from %s: %w", abs, err)
	}
	return Worktree{Path: abs, HEAD: sha}, nil
}

func pickAutoSource(repoDir, targetAbs, targetSHA string, verbose bool) (Worktree, bool, int) {
	all, err := listWorktrees(repoDir)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "%s: list worktrees: %v\n", commandName, err)
		}
		return Worktree{}, false, 0
	}
	pool := candidatePool(all, repoDir, targetAbs, maxOtherCandidates)
	if len(pool) == 0 {
		return Worktree{}, false, 0
	}
	src, score, ok := pickSource(repoDir, pool, targetSHA)
	if !ok {
		return Worktree{}, false, 0
	}
	return src, true, score
}

func parseArgs(args []string) (Options, error) {
	var o Options
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.GitArgs = append(o.GitArgs, a)
			o.UseSeparator = true
			positional = append(positional, args[i+1:]...)
			o.GitArgs = append(o.GitArgs, args[i+1:]...)
			i = len(args)
		case a == "-v" || a == "--verbose":
			o.Verbose = true
			// Not forwarded to git: `git worktree add` doesn't accept -v.
		case a == "--from":
			if i+1 >= len(args) {
				return o, errors.New("--from requires a path")
			}
			o.FromPath = args[i+1]
			o.FromSpecified = true
			i++
		case strings.HasPrefix(a, "--from="):
			o.FromPath = strings.TrimPrefix(a, "--from=")
			if o.FromPath == "" {
				return o, errors.New("--from requires a path")
			}
			o.FromSpecified = true
		case a == "--checkout":
			o.NoCheckout = false
			o.GitArgs = append(o.GitArgs, a)
		case a == "--no-checkout":
			o.NoCheckout = true
			o.GitArgs = append(o.GitArgs, a)
		case a == "--orphan":
			o.Orphan = true
			o.GitArgs = append(o.GitArgs, a)
		case a == "--no-orphan":
			o.Orphan = false
			o.GitArgs = append(o.GitArgs, a)
		case strings.HasPrefix(a, "-"):
			arity := gitWorktreeAddFlagArity(a)
			if arity == 1 {
				if i+1 >= len(args) {
					return o, fmt.Errorf("%s requires an argument", a)
				}
				o.Forward = append(o.Forward, a, args[i+1])
				o.GitArgs = append(o.GitArgs, a, args[i+1])
				i++
			} else {
				o.Forward = append(o.Forward, a)
				o.GitArgs = append(o.GitArgs, a)
			}
		default:
			positional = append(positional, a)
			o.GitArgs = append(o.GitArgs, a)
		}
	}
	if len(positional) > 0 {
		o.TargetPath = positional[0]
	}
	if len(positional) > 1 {
		o.CommitIsh = positional[1]
	}
	if len(positional) > 2 {
		return o, fmt.Errorf("unexpected positional argument: %s", positional[2])
	}
	return o, nil
}

func gitWorktreeAddFlagArity(arg string) int {
	switch arg {
	case "-b", "-B", "--reason":
		return 1
	}
	return 0
}

func execGitWorktreeAdd(args []string) error {
	cmd := exec.Command("git", append([]string{"worktree", "add"}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runGitStreaming(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
