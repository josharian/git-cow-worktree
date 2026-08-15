//go:build !darwin && !linux

package main

// Without CoW there is nothing to vouch for, so this is never reached; it
// exists to keep the package building everywhere.
func lstatFields(path string) (fileStat, error) {
	return fileStat{}, errCoWUnsupported
}
