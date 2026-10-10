//go:build !windows

package kvconf

import "os"

func rename(from, to string) error { return os.Rename(from, to) }
