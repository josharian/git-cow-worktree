`git-cow-worktree` is a drop-in replacement for `git worktree add`.

It uses copy-on-write for the worktree, reducing disk usage.

It is entirely vibe-coded, but there's also not that much to it. Use at your own risk.

Everything below is LLM-written.

---

## Install

```sh
go install github.com/josharian/git-cow-worktree@latest
```

Git can dispatch `git cow-worktree ...` when the installed `git-cow-worktree` binary is on your `PATH`.

## Usage

```sh
git cow-worktree add [git-worktree-add flags] [--from <path>] [-v] <path> [<commit-ish>]
```

Examples:

```sh
git cow-worktree add ../repo-feature feature
git cow-worktree add -b topic ../repo-topic main
git cow-worktree add -v --from ../repo-main ../repo-topic topic
```

Most flags are inherited from `git worktree add`.

Added by `git-cow-worktree`:

- `--from <path>`: use a specific source worktree instead of auto-selecting one.
- `-v`, `--verbose`: print the chosen source, reflink count, and timings.

Special case:

- `--no-checkout`: passed through to `git worktree add`; no reflinking is attempted.

## Filesystem Support

Reflinking is supported on APFS on macOS and on Linux filesystems with `FICLONE` support, such as btrfs, XFS, and bcachefs. On unsupported filesystems or across devices, `git-cow-worktree` falls back to Git's normal checkout behavior.

## Scope

`git-cow-worktree` only replaces `git worktree add`. Other `git worktree` subcommands should be run with Git directly.

## How it works

It first runs `git worktree add --no-checkout`, so Git creates the new worktree metadata and index but leaves the working tree empty.

Then it picks a source worktree. Candidates are the current worktree, the main worktree, and a few recently modified other worktrees. It ignores the target worktree and unmaterialized worktrees. Candidates are scored by `git rev-list --left-right --count <source>...<target>`; fewer commits ahead/behind is better.

It runs `git ls-tree -r` on the source and target commits, then reflinks regular tracked files whose blob SHA and mode match exactly. Symlinks, submodules, missing files, changed files, and mode mismatches are left alone.

Finally it runs Git checkout in the new worktree. That fills in files that were not reflinked, overwrites anything stale or dirty, updates stat info, and makes partial reflink failure safe. If reflinks are unsupported or cross-device, it just becomes a normal checkout.
