package main

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	commandName        = "git-cow-worktree"
	maxOtherCandidates = 8
	// How far down the ranking to look for a source worth cloning from.
	// Each rejection costs a pair of Git invocations against a worktree
	// that turned out to be useless, and a source this far from the top is
	// unlikely to be worth much anyway.
	maxSourceAttempts = 3
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

	var explicitSource *Worktree
	if opts.FromSpecified {
		wt, err := resolveFromPath(opts.FromPath)
		if err != nil {
			return err
		}
		explicitSource = &wt
	}

	targetAbs, err := osAbs(opts.TargetPath)
	if err != nil {
		return err
	}

	overallStart := time.Now()
	phase := func(name string, t0 time.Time) {
		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "%s: %-20s %v\n", commandName, name, time.Since(t0).Round(time.Millisecond))
		}
	}

	// Step 2: git worktree add --no-checkout [user flags], and, alongside
	// it, the analysis that feeds seeding. The two are independent: the
	// analysis reads the object store and the source worktree, and touches
	// nothing under the new worktree.
	//
	// What it does need is the target commit, which Git doesn't settle
	// until the worktree exists. An explicit <commit-ish> — or HEAD, when
	// one is omitted — resolves to that commit in all but the DWIM cases
	// (--guess-remote and friends), so we resolve it ourselves and check
	// the guess against the real HEAD below, redoing the analysis on the
	// rare miss.
	addArgs := append([]string{"worktree", "add", "--no-checkout"}, opts.Forward...)
	if opts.UseSeparator {
		addArgs = append(addArgs, "--")
	}
	addArgs = append(addArgs, opts.TargetPath)
	if opts.CommitIsh != "" {
		addArgs = append(addArgs, opts.CommitIsh)
	}
	t := time.Now()
	var addErr error
	var wg sync.WaitGroup
	wg.Go(func() { addErr = runGitStreaming(repoDir, addArgs...) })

	guessRev := cmp.Or(opts.CommitIsh, "HEAD")
	var a *analysis
	if sha, err := resolveRev(repoDir, guessRev); err == nil {
		a = analyze(repoDir, targetAbs, sha, explicitSource, opts.Verbose)
	}

	wg.Wait()
	if addErr != nil {
		return fmt.Errorf("git worktree add: %w", addErr)
	}
	phase("worktree add", t)

	// Resolve target SHA from inside the new worktree (HEAD is now set).
	targetSHA, err := resolveRev(targetAbs, "HEAD")
	if err != nil {
		return err
	}
	if a == nil || a.SHA != targetSHA {
		t = time.Now()
		a = analyze(repoDir, targetAbs, targetSHA, explicitSource, opts.Verbose)
		phase("analyze", t)
	}

	var seededIndex string
	if a.SrcOK {
		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "%s: source=%s (diff=%d commits)\n", commandName, a.Src.Path, a.Score)
		}
		var err error
		seededIndex, err = seed(a, targetAbs, phase, opts.Verbose)
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

// analysis is everything the clone plan needs that depends only on the
// repository and the target commit, and not on the new worktree existing.
// Keeping that boundary sharp is what lets the work overlap `git worktree
// add`.
type analysis struct {
	SHA        string // target commit the rest of this describes
	Src        Worktree
	SrcOK      bool
	Score      int // commits between Src and the target
	TargetTree *treeIndex
	SrcTree    *treeIndex
	Excluded   map[string]bool
	Err        error
}

// analyze settles on a source worktree to clone targetSHA from and reads
// what planning needs from both sides. If explicit is non-nil it is used as
// the source, whatever shape it turns out to be in.
//
// Being nearest in commits doesn't make a worktree worth cloning from. One
// that Git is midway through checking out — including one this command is
// creating in another terminal — is at the target commit and yet holds
// almost nothing, so it would contribute nothing but the cost of finding
// that out. Reject those and move down the ranking.
//
// The target tree is the same whichever source wins, so it is read once,
// alongside the first candidate.
func analyze(repoDir, targetAbs, targetSHA string, explicit *Worktree, verbose bool) *analysis {
	a := &analysis{SHA: targetSHA}

	var ranked []scoredSource
	if explicit != nil {
		score, _ := commitDistance(repoDir, explicit.HEAD, targetSHA)
		ranked = []scoredSource{{WT: *explicit, Score: score}}
	} else {
		ranked = rankAutoSources(repoDir, targetAbs, targetSHA, verbose)
	}
	if len(ranked) == 0 {
		return a
	}

	var targetErr error
	var wg sync.WaitGroup
	wg.Go(func() { a.TargetTree, targetErr = lsTree(repoDir, targetSHA) })

	for i, c := range ranked {
		if i == maxSourceAttempts {
			break
		}
		r, err := readSource(c.WT)
		switch {
		case err != nil:
			if verbose {
				fmt.Fprintf(os.Stderr, "%s: skipping source %s: %v\n", commandName, c.WT.Path, err)
			}
			continue
		case explicit == nil && !r.usable():
			if verbose {
				fmt.Fprintf(os.Stderr, "%s: skipping source %s: %d of %d tracked files differ from its HEAD\n",
					commandName, c.WT.Path, r.Dirty, len(r.Tree.Blobs))
			}
			continue
		}
		a.Src, a.Score, a.SrcOK = c.WT, c.Score, true
		a.SrcTree, a.Excluded = r.Tree, r.Excluded
		break
	}

	wg.Wait()
	a.Err = targetErr
	return a
}

// sourceReport is a candidate source worktree seen up close.
type sourceReport struct {
	Tree     *treeIndex
	Excluded map[string]bool
	Dirty    int // tracked paths whose working-tree copy differs from HEAD
}

func readSource(wt Worktree) (*sourceReport, error) {
	var r sourceReport
	var treeErr, exclErr error
	var wg sync.WaitGroup
	wg.Go(func() { r.Tree, treeErr = lsTree(wt.Path, wt.HEAD) })
	wg.Go(func() { r.Excluded, r.Dirty, exclErr = sourceExclusions(wt.Path, wt.HEAD) })
	wg.Wait()
	return &r, cmp.Or(treeErr, exclErr)
}

// usable reports whether the worktree holds enough of its own commit to be
// worth cloning from. A worktree Git hasn't finished checking out reads as
// almost entirely dirty, because every file not yet written differs from
// HEAD; so does one a user has left in a comparable state, and neither is a
// good source. Ordinary local work doesn't come close to the threshold.
func (r *sourceReport) usable() bool {
	return r.Dirty*2 <= len(r.Tree.Blobs)
}

// seed reflinks as much of the new worktree as it can from src, and writes
// an index vouching for what it reflinked so the checkout that follows
// leaves those files — and their shared storage — alone. It returns the
// path of the index it wrote, if any.
//
// Every step is best-effort. Returning an error, or returning nothing,
// means the caller's checkout writes the worktree the ordinary way.
func seed(a *analysis, targetAbs string, phase func(string, time.Time), verbose bool) (string, error) {
	if a.Err != nil {
		return "", a.Err
	}

	// Sparse checkout is worktree-local configuration, so unlike the rest of
	// the analysis this has to wait for the new worktree to exist.
	t := time.Now()
	targetTree := a.TargetTree
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
	phase("sparse check", t)

	// GIT_COW_WORKTREE_NO_DIR_CLONE exercises the file-by-file path, which
	// is what Linux always takes, on a filesystem that can clone directories.
	cloneDirs := canCloneDirs && !sparse && os.Getenv("GIT_COW_WORKTREE_NO_DIR_CLONE") == ""

	t = time.Now()
	plan := planClones(a.SrcTree, targetTree, a.Excluded, cloneDirs)
	phase("plan", t)

	t = time.Now()
	result := plan.clone(a.Src.Path, targetAbs)
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

func rankAutoSources(repoDir, targetAbs, targetSHA string, verbose bool) []scoredSource {
	all, err := listWorktrees(repoDir)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "%s: list worktrees: %v\n", commandName, err)
		}
		return nil
	}
	pool := candidatePool(all, repoDir, targetAbs, maxOtherCandidates)
	if len(pool) == 0 {
		return nil
	}
	return rankSources(repoDir, pool, targetSHA)
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
