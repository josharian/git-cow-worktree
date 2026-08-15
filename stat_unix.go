//go:build darwin || linux

package main

import "golang.org/x/sys/unix"

func lstatFields(path string) (fileStat, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return fileStat{}, err
	}
	return fileStat{
		CtimeSec:  uint32(st.Ctim.Sec),
		CtimeNsec: uint32(st.Ctim.Nsec),
		MtimeSec:  uint32(st.Mtim.Sec),
		MtimeNsec: uint32(st.Mtim.Nsec),
		Dev:       uint32(st.Dev),
		Ino:       uint32(st.Ino),
		Uid:       st.Uid,
		Gid:       st.Gid,
		Size:      uint32(st.Size),
		mode:      uint32(st.Mode),
	}, nil
}
