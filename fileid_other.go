//go:build !unix

package kvconf

import "io/fs"

// fileID is unknown here: size and modification time identify a version.
func fileID(fs.FileInfo) uint64 { return 0 }
