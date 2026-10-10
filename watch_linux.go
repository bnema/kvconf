package kvconf

import (
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// inotify watches directories with Linux inotify. Its descriptor goes
// through the Go runtime poller, so a wait costs no thread and is woken by a
// read deadline.
type inotify struct {
	f     *os.File
	fd    int
	dirs  map[int32]string // watch descriptor -> directory
	wds   map[string]int32
	buf   []byte
	woken atomic.Bool
	// names interns event names: the same few files change again and again.
	names map[string]string
}

const movedMask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO

const inotifyMask = syscall.IN_CLOSE_WRITE | syscall.IN_MODIFY | syscall.IN_ATTRIB |
	syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO |
	syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF | syscall.IN_ONLYDIR

func newNative() (backend, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, os.NewSyscallError("inotify_init1", err)
	}
	return &inotify{
		f:     os.NewFile(uintptr(fd), "inotify"),
		fd:    fd,
		dirs:  map[int32]string{},
		wds:   map[string]int32{},
		buf:   make([]byte, 16<<10),
		names: map[string]string{},
	}, nil
}

func (in *inotify) set(targets []*target) (missing bool, err error) {
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t.dir] = true
	}
	// Keep unchanged watches: removing one queues an IN_IGNORED.
	for dir, wd := range in.wds {
		if !want[dir] {
			_, _ = syscall.InotifyRmWatch(in.fd, uint32(wd))
			delete(in.wds, dir)
			delete(in.dirs, wd)
		}
	}
	for dir := range want {
		// Always add: a directory renamed away and replaced keeps its path
		// but not its inode, and adding returns the watch of the current
		// one (the same descriptor when nothing moved).
		wd, err := syscall.InotifyAddWatch(in.fd, dir, inotifyMask)
		if old, ok := in.wds[dir]; ok && (err != nil || old != int32(wd)) {
			_, _ = syscall.InotifyRmWatch(in.fd, uint32(old))
			delete(in.wds, dir)
			delete(in.dirs, old)
		}
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			missing = true
			continue
		}
		if err != nil {
			return missing, os.NewSyscallError("inotify_add_watch", err)
		}
		in.wds[dir] = int32(wd)
		in.dirs[int32(wd)] = dir
	}
	return missing, nil
}

func (in *inotify) wait(timeout time.Duration, evs []event) ([]event, error) {
	var deadline time.Time
	if timeout >= 0 {
		deadline = time.Now().Add(timeout)
	}
	if err := in.f.SetReadDeadline(deadline); err != nil {
		return evs, err
	}
	if in.woken.Load() {
		return evs, nil
	}
	n, err := in.f.Read(in.buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return evs, nil
	}
	if err != nil {
		return evs, err
	}
	for off := 0; off+syscall.SizeofInotifyEvent <= n; {
		ev := (*syscall.InotifyEvent)(unsafe.Pointer(&in.buf[off]))
		size := syscall.SizeofInotifyEvent + int(ev.Len)
		if off+size > n {
			break
		}
		name := in.buf[off+syscall.SizeofInotifyEvent : off+size]
		off += size
		if ev.Mask&syscall.IN_Q_OVERFLOW != 0 {
			evs = append(evs, event{}) // events were lost: check everything
			continue
		}
		dir, ok := in.dirs[ev.Wd]
		if !ok {
			continue // a watch we removed
		}
		if ev.Mask&(syscall.IN_IGNORED|syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF) != 0 {
			// The watch follows the inode, not the path: drop it.
			if ev.Mask&syscall.IN_IGNORED == 0 {
				_, _ = syscall.InotifyRmWatch(in.fd, uint32(ev.Wd))
			}
			delete(in.dirs, ev.Wd)
			if in.wds[dir] == ev.Wd {
				delete(in.wds, dir)
			}
			evs = append(evs, event{})
			continue
		}
		for len(name) > 0 && name[len(name)-1] == 0 {
			name = name[:len(name)-1]
		}
		if len(name) > 0 {
			evs = append(evs, event{dir: dir, name: in.intern(name), moved: ev.Mask&movedMask != 0})
		}
	}
	return evs, nil
}

// intern returns name as a string without allocating once it was seen.
func (in *inotify) intern(name []byte) string {
	if s, ok := in.names[string(name)]; ok { // no allocation for the lookup
		return s
	}
	s := string(name)
	if len(in.names) < 1024 { // a directory full of churn cannot grow it forever
		in.names[s] = s
	}
	return s
}

func (in *inotify) wake() {
	in.woken.Store(true)
	_ = in.f.SetReadDeadline(time.Now())
}

func (in *inotify) close() error { return in.f.Close() }
