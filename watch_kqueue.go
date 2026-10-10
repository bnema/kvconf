//go:build darwin || ios || freebsd || netbsd || openbsd || dragonfly

package kvconf

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// kqueue watches with BSD/macOS kqueue. A vnode watch needs a descriptor
// per file, so it watches each directory (for files created, removed or
// renamed) and each file that matters (for writes in place). Events carry no
// name: the watcher compares file stamps to see what changed, and asks for a
// rewatch when a file was replaced, since its descriptor then points to the
// old one.
type kqueue struct {
	kq     int
	wakeR  int
	wakeW  int
	fds    map[int]string // descriptor -> directory it reports for
	events []syscall.Kevent_t
}

const kqueueFflags = syscall.NOTE_WRITE | syscall.NOTE_EXTEND | syscall.NOTE_ATTRIB |
	syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_REVOKE

func newNative() (backend, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, os.NewSyscallError("kqueue", err)
	}
	syscall.CloseOnExec(kq)
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		syscall.Close(kq)
		return nil, os.NewSyscallError("pipe", err)
	}
	for _, fd := range p {
		syscall.CloseOnExec(fd)
		_ = syscall.SetNonblock(fd, true)
	}
	k := &kqueue{kq: kq, wakeR: p[0], wakeW: p[1], fds: map[int]string{}, events: make([]syscall.Kevent_t, 64)}
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, k.wakeR, syscall.EVFILT_READ, syscall.EV_ADD|syscall.EV_CLEAR)
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		_ = k.close()
		return nil, os.NewSyscallError("kevent", err)
	}
	return k, nil
}

func (k *kqueue) set(targets []*target) (missing bool, err error) {
	// Descriptors are cheap to reopen and a replaced file needs a new one.
	for fd := range k.fds {
		syscall.Close(fd) // closing removes its kevents
	}
	clear(k.fds)
	var changes []syscall.Kevent_t
	watch := func(path, dir string) bool {
		fd, err := syscall.Open(path, openEventOnly|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		k.fds[fd] = dir
		var ev syscall.Kevent_t
		syscall.SetKevent(&ev, fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
		ev.Fflags = kqueueFflags
		changes = append(changes, ev)
		return true
	}
	for _, t := range targets {
		if !watch(t.dir, t.dir) {
			missing = true
			continue
		}
		for _, name := range t.names {
			watch(filepath.Join(t.dir, name), t.dir)
		}
		if t.all {
			entries, _ := os.ReadDir(t.dir)
			for _, e := range entries {
				if visible(e.Name()) && !e.IsDir() {
					watch(filepath.Join(t.dir, e.Name()), t.dir)
				}
			}
		}
	}
	if len(changes) > 0 {
		if _, err := syscall.Kevent(k.kq, changes, nil, nil); err != nil {
			return missing, os.NewSyscallError("kevent", err)
		}
	}
	return missing, nil
}

func (k *kqueue) wait(timeout time.Duration, evs []event) ([]event, error) {
	var ts *syscall.Timespec
	if timeout >= 0 {
		t := syscall.NsecToTimespec(int64(timeout))
		ts = &t
	}
	n, err := syscall.Kevent(k.kq, nil, k.events, ts)
	if errors.Is(err, syscall.EINTR) {
		return evs, nil
	}
	if err != nil {
		return evs, os.NewSyscallError("kevent", err)
	}
	for _, ev := range k.events[:n] {
		fd := int(ev.Ident)
		if fd == k.wakeR {
			var buf [64]byte
			for {
				if n, _ := syscall.Read(k.wakeR, buf[:]); n <= 0 {
					break
				}
			}
			continue
		}
		dir, ok := k.fds[fd]
		if !ok {
			continue
		}
		lost := ev.Fflags&(syscall.NOTE_DELETE|syscall.NOTE_RENAME|syscall.NOTE_REVOKE) != 0
		evs = append(evs, event{dir: dir, lost: lost})
	}
	return evs, nil
}

func (k *kqueue) wake() {
	_, _ = syscall.Write(k.wakeW, []byte{0})
}

func (k *kqueue) close() error {
	for fd := range k.fds {
		syscall.Close(fd)
	}
	syscall.Close(k.wakeR)
	syscall.Close(k.wakeW)
	return syscall.Close(k.kq)
}
