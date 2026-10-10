//go:build !unix

package kvconf

import "os"

func openNonblocking(path string) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrNotRegular}
	}
	return os.Open(path)
}
