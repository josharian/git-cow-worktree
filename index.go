package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The index we write exists for exactly one reader: the `git checkout -f
// HEAD` that follows. Git decides whether to rewrite a working-tree file by
// comparing it against the cached stat data of its index entry, so a fresh
// worktree — whose index `git worktree add --no-checkout` leaves absent
// entirely — makes checkout rewrite every file we just cloned, undoing the
// sharing. Handing it stat data for the files we verified makes it skip
// them and write only the rest.
//
// Entries we omit are not lost: checkout builds the real index from the
// target tree and treats anything missing here as a file to write. So the
// worst case for a bad entry is wasted work, and the worst case for a
// missing one is a normal checkout.

const (
	indexSignature  = "DIRC"
	indexVersion    = 2
	indexEntryAlign = 8
	maxNameLenField = 0xFFF // longer names are marked with this and NUL-terminated
)

// writeIndex writes entries as a version 2 Git index at path. Entries may
// arrive in any order; Git requires them sorted by name.
func writeIndex(path string, entries []indexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	oidLen := len(entries[0].OID)
	var sum hash.Hash
	switch oidLen {
	case sha1.Size:
		sum = sha1.New()
	case sha256.Size:
		sum = sha256.New()
	default:
		return fmt.Errorf("unsupported object ID length %d", oidLen)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	var buf bytes.Buffer
	buf.WriteString(indexSignature)
	binary.Write(&buf, binary.BigEndian, uint32(indexVersion))
	binary.Write(&buf, binary.BigEndian, uint32(len(entries)))
	for _, e := range entries {
		if len(e.OID) != oidLen {
			return fmt.Errorf("%s: object ID length %d, want %d", e.Path, len(e.OID), oidLen)
		}
		start := buf.Len()
		for _, v := range [...]uint32{
			e.Stat.CtimeSec, e.Stat.CtimeNsec,
			e.Stat.MtimeSec, e.Stat.MtimeNsec,
			e.Stat.Dev, e.Stat.Ino,
			e.Mode,
			e.Stat.Uid, e.Stat.Gid,
			e.Stat.Size,
		} {
			binary.Write(&buf, binary.BigEndian, v)
		}
		buf.Write(e.OID)
		binary.Write(&buf, binary.BigEndian, uint16(min(len(e.Path), maxNameLenField)))
		buf.WriteString(e.Path)
		// At least one NUL, then pad the entry to an 8-byte boundary.
		buf.WriteByte(0)
		for (buf.Len()-start)%indexEntryAlign != 0 {
			buf.WriteByte(0)
		}
	}
	sum.Write(buf.Bytes())
	buf.Write(sum.Sum(nil))

	if os.Getenv("GIT_COW_WORKTREE_CORRUPT_INDEX") != "" {
		// Test hook: an index Git will refuse, to exercise the recovery
		// path in run(). Nothing else claims an entry count this absurd.
		binary.BigEndian.PutUint32(buf.Bytes()[8:], 0x7fffffff)
	}

	// Write through index.lock, as Git does, so a concurrent Git process
	// fails loudly instead of racing us, and so a partial write can never
	// become the index.
	lock := path + ".lock"
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return fmt.Errorf("create %s: %w", lock, err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		os.Remove(lock)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(lock)
		return err
	}
	if err := os.Rename(lock, path); err != nil {
		os.Remove(lock)
		return err
	}
	return nil
}

// indexPath returns the path of worktree's index file.
func indexPath(worktree string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = worktree
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --absolute-git-dir: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return filepath.Join(strings.TrimSpace(string(out)), "index"), nil
}
