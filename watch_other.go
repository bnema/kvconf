//go:build !linux && !windows && !darwin && !ios && !freebsd && !netbsd && !openbsd && !dragonfly

package kvconf

// newNative returns nil: this OS has no native backend, Watch polls.
func newNative() (backend, error) { return nil, nil }
