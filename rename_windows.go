package kvconf

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// rename retries while another process (an editor, an indexer, our own
// watcher) holds the target open, which Windows reports as a sharing error.
func rename(from, to string) error {
	const (
		errorAccessDenied     = syscall.Errno(5)
		errorSharingViolation = syscall.Errno(32)
	)
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if !errors.Is(err, errorAccessDenied) && !errors.Is(err, errorSharingViolation) {
			return err
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Millisecond)
	}
	return err
}
