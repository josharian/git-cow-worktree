//go:build darwin

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

var errCoWUnsupported = errors.New("clonefile: copy-on-write unsupported")

// On macOS, clonefile returns ENOTSUP on filesystems that don't support
// reflinks. EXDEV doesn't apply (clonefile checks volumes itself).
var cowUnsupportedErrnos = []error{
	syscall.ENOTSUP,
	syscall.EXDEV,
	syscall.EOPNOTSUPP,
}

// APFS clones whole directory hierarchies in a single clonefile call,
// which is dramatically cheaper than one call per file.
const canCloneDirs = true

// cowClone reflinks src to dst. dst must not exist.
func cowClone(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}

// cowCloneDir recursively reflinks the directory src to dst, which must
// not exist.
func cowCloneDir(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
