package main

// fileStat is the subset of stat(2) that a Git index entry records. Git
// truncates every field to 32 bits and compares them verbatim, so we do
// the same: a field that doesn't round-trip would just make Git consider
// the file dirty and rehash it.
type fileStat struct {
	CtimeSec, CtimeNsec uint32
	MtimeSec, MtimeNsec uint32
	Dev, Ino            uint32
	Uid, Gid            uint32
	Size                uint32
	mode                uint32 // st_mode, for our own checks; not stored in the index
}

func (fs fileStat) IsRegular() bool {
	const sIFMT, sIFREG = 0o170000, 0o100000
	return fs.mode&sIFMT == sIFREG
}

func (fs fileStat) IsExecutable() bool {
	return fs.mode&0o111 != 0
}
