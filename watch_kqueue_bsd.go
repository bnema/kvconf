//go:build freebsd || netbsd || openbsd || dragonfly

package kvconf

import "syscall"

const openEventOnly = syscall.O_RDONLY
