//go:build !darwin && !linux

package main

import "errors"

var errCoWUnsupported = errors.New("copy-on-write unsupported on this platform")

var cowUnsupportedErrnos = []error{}

func cowClone(src, dst string) error {
	return errCoWUnsupported
}
