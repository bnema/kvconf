//go:build unix

package kvconf

import (
	"os"
	"syscall"
)

// openNonblocking opens path without blocking on a FIFO swapped in for the
// config file.
func openNonblocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
