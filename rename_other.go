//go:build !windows

package kvconf

import "os"

func rename(from, to string) error { return os.Rename(from, to) }

// syncDir makes a rename in dir survive a power loss. Some file systems
// cannot sync a directory; the rename is done anyway.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	_ = d.Sync()
	return d.Close()
}
