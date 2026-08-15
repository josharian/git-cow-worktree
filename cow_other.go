//go:build !darwin && !linux

package main

import "errors"

var errCoWUnsupported = errors.New("copy-on-write unsupported on this platform")

var cowUnsupportedErrnos = []error{}

const canCloneDirs = false

func cowClone(src, dst string) error {
	return errCoWUnsupported
}

func cowCloneDir(src, dst string) error {
	return errCoWUnsupported
}
