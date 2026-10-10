//go:build unix

package kvconf

import (
	"io/fs"
	"syscall"
)

// fileID is the inode: it changes when a file is replaced by a rename.
func fileID(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
