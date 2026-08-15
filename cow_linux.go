//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

var errCoWUnsupported = errors.New("ioctl FICLONE: copy-on-write unsupported")

var cowUnsupportedErrnos = []error{
	syscall.ENOTSUP,
	syscall.EOPNOTSUPP,
	syscall.EXDEV,
	syscall.EINVAL, // returned on some filesystems when reflink isn't applicable
}

// FICLONE works on regular files only, so on Linux we always clone
// file by file.
const canCloneDirs = false

// cowCloneDir is never called on Linux; see canCloneDirs.
func cowCloneDir(src, dst string) error {
	return errCoWUnsupported
}

// cowClone reflinks src to dst on a CoW-capable filesystem (btrfs, xfs
// with reflink, bcachefs, ...). dst must not exist. We refuse to follow
// symlinks at src to match darwin's CLONE_NOFOLLOW; reflinkSet already
// filters out symlink tree entries, so src being a symlink means the
// user replaced the working-tree file — let checkout sort it out.
func cowClone(src, dst string) error {
	sf, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(df.Fd()), int(sf.Fd())); err != nil {
		df.Close()
		_ = os.Remove(dst)
		return err
	}
	// Preserve mode (executable bit) from source. A silent chmod failure
	// would leave dst at 0o644 with matching content, so the final
	// `git checkout -f HEAD` wouldn't notice and rewrite it; bail loudly.
	st, err := sf.Stat()
	if err != nil {
		df.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := df.Chmod(st.Mode().Perm()); err != nil {
		df.Close()
		_ = os.Remove(dst)
		return err
	}
	return df.Close()
}
