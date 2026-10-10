//go:build !linux && !windows && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package kvconf

import "time"

// newNative polls every second: this OS has no native backend.
func newNative() (backend, error) { return newPoller(time.Second), nil }
