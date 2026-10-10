//go:build !unix

package kvconf

import "os"

func openNonblocking(path string) (*os.File, error) {
	return os.Open(path)
}
