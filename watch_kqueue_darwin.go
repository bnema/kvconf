//go:build darwin || ios

package kvconf

import "syscall"

// openEventOnly opens a file for notifications only: it does not keep a
// volume from being unmounted.
const openEventOnly = syscall.O_EVTONLY
