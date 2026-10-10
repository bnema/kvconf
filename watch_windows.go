package kvconf

import (
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// These functions are missing from package syscall: they are loaded from
// kernel32 directly.
var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW           = kernel32.NewProc("CreateEventW")
	procSetEvent               = kernel32.NewProc("SetEvent")
	procResetEvent             = kernel32.NewProc("ResetEvent")
	procWaitForMultipleObjects = kernel32.NewProc("WaitForMultipleObjects")
	procGetOverlappedResult    = kernel32.NewProc("GetOverlappedResult")
)

const (
	waitObject0       = 0
	waitFailed        = 0xFFFFFFFF
	infinite          = 0xFFFFFFFF
	maximumWait       = 64 // MAXIMUM_WAIT_OBJECTS
	errorIOIncomplete = syscall.Errno(996)
	errorIOPending    = syscall.Errno(997)

	notifyMask = syscall.FILE_NOTIFY_CHANGE_FILE_NAME | syscall.FILE_NOTIFY_CHANGE_DIR_NAME |
		syscall.FILE_NOTIFY_CHANGE_SIZE | syscall.FILE_NOTIFY_CHANGE_LAST_WRITE |
		syscall.FILE_NOTIFY_CHANGE_CREATION | syscall.FILE_NOTIFY_CHANGE_ATTRIBUTES
)

// dirWatch is one directory with its pending overlapped ReadDirectoryChangesW.
// It lives on the heap, which Go never moves, so the kernel may write into
// ov and buf until the read completes or is cancelled and drained.
type dirWatch struct {
	dir     string
	handle  syscall.Handle
	ov      syscall.Overlapped
	buf     []uint32 // DWORD aligned, as the API requires
	pending bool
}

type winWatch struct {
	wakeEv  syscall.Handle
	dirs    []*dirWatch
	handles []syscall.Handle
}

func createEvent() (syscall.Handle, error) {
	h, _, err := procCreateEventW.Call(0, 1, 0, 0) // manual reset, not signaled
	if h == 0 {
		return 0, os.NewSyscallError("CreateEventW", err)
	}
	return syscall.Handle(h), nil
}

func newNative() (backend, error) {
	ev, err := createEvent()
	if err != nil {
		return nil, err
	}
	return &winWatch{wakeEv: ev}, nil
}

func (w *winWatch) set(targets []*target) (missing bool, err error) {
	w.closeDirs()
	for _, t := range targets {
		if len(w.dirs) == maximumWait-1 {
			return missing, errors.New("kvconf: too many directories to watch")
		}
		d, err := openDir(t.dir)
		if err != nil {
			// Missing, or removed while a handle keeps it "delete pending"
			// (access denied): look again later.
			missing = true
			continue
		}
		if err := d.read(); err != nil {
			d.close()
			missing = true
			continue
		}
		w.dirs = append(w.dirs, d)
	}
	return missing, nil
}

func openDir(dir string) (*dirWatch, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.FILE_LIST_DIRECTORY,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, &os.PathError{Op: "CreateFile", Path: dir, Err: err}
	}
	ev, err := createEvent()
	if err != nil {
		syscall.CloseHandle(h)
		return nil, err
	}
	d := &dirWatch{dir: dir, handle: h, buf: make([]uint32, 16<<10)}
	d.ov.HEvent = ev
	return d, nil
}

func (d *dirWatch) read() error {
	err := syscall.ReadDirectoryChanges(d.handle, (*byte)(unsafe.Pointer(&d.buf[0])),
		uint32(len(d.buf)*4), false, notifyMask, nil, &d.ov, 0)
	if err != nil && !errors.Is(err, errorIOPending) {
		return err
	}
	d.pending = true
	return nil
}

// result returns the size of the completed read.
func (d *dirWatch) result(wait bool) (uint32, error) {
	var n uint32
	b := uintptr(0)
	if wait {
		b = 1
	}
	r, _, err := procGetOverlappedResult.Call(uintptr(d.handle), uintptr(unsafe.Pointer(&d.ov)), uintptr(unsafe.Pointer(&n)), b)
	if r == 0 {
		return 0, err
	}
	return n, nil
}

func (d *dirWatch) close() {
	if d.pending {
		_ = syscall.CancelIoEx(d.handle, &d.ov)
		_, _ = d.result(true) // the kernel is done with ov and buf
		d.pending = false
	}
	syscall.CloseHandle(d.handle)
	syscall.CloseHandle(d.ov.HEvent)
}

func (w *winWatch) closeDirs() {
	for _, d := range w.dirs {
		d.close()
	}
	w.dirs = w.dirs[:0]
}

func (w *winWatch) wait(timeout time.Duration, evs []event) ([]event, error) {
	w.handles = append(w.handles[:0], w.wakeEv)
	for _, d := range w.dirs {
		w.handles = append(w.handles, d.ov.HEvent)
	}
	ms := uintptr(infinite)
	if timeout >= 0 {
		ms = uintptr((timeout + time.Millisecond - 1) / time.Millisecond)
	}
	r, _, err := procWaitForMultipleObjects.Call(uintptr(len(w.handles)),
		uintptr(unsafe.Pointer(&w.handles[0])), 0, ms)
	switch {
	case r == waitFailed:
		return evs, os.NewSyscallError("WaitForMultipleObjects", err)
	case r == uintptr(syscall.WAIT_TIMEOUT):
		return evs, nil
	case r == waitObject0:
		procResetEvent.Call(uintptr(w.wakeEv))
		return evs, nil
	}
	i := int(r - waitObject0 - 1)
	if i < 0 || i >= len(w.dirs) {
		return evs, nil
	}
	d := w.dirs[i]
	n, err := d.result(false)
	d.pending = false
	if err != nil {
		if errors.Is(err, errorIOIncomplete) {
			d.pending = true
			return evs, nil
		}
		// The directory was removed or became unreadable.
		return append(evs, event{}), nil
	}
	if n == 0 {
		evs = append(evs, event{dir: d.dir}) // the buffer overflowed
	}
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&d.buf[0])), len(d.buf)*4)
	for off := uint32(0); n > 0 && int(off)+int(unsafe.Sizeof(syscall.FileNotifyInformation{})) <= int(n); {
		info := (*syscall.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
		name := unsafe.Slice(&info.FileName, info.FileNameLength/2)
		moved := info.Action != syscall.FILE_ACTION_MODIFIED
		evs = append(evs, event{dir: d.dir, name: syscall.UTF16ToString(name), moved: moved})
		if info.NextEntryOffset == 0 {
			break
		}
		off += info.NextEntryOffset
	}
	if err := d.read(); err != nil {
		evs = append(evs, event{})
	}
	return evs, nil
}

func (w *winWatch) wake() { procSetEvent.Call(uintptr(w.wakeEv)) }

func (w *winWatch) close() error {
	w.closeDirs()
	return syscall.CloseHandle(w.wakeEv)
}
