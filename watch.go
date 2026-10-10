package kvconf

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// DefaultDebounce is the quiet time Watch waits for after a change.
const DefaultDebounce = 100 * time.Millisecond

// retryMissing is how often a missing directory is looked for again, in
// case its creation was not seen.
const retryMissing = 2 * time.Second

// WatchOptions controls Watch.
type WatchOptions struct {
	// Debounce is the quiet time after a change before it is reported:
	// every write restarts it, so a file is never read half written. 0
	// means DefaultDebounce; a negative value reports each change at once.
	Debounce time.Duration
	// Dir watches every file directly inside the path, a directory, instead
	// of the path itself. Hidden files (".swp") and backups ("~") are
	// ignored and subdirectories are not watched.
	Dir bool
	// Poll, when positive, checks the files at this interval instead of
	// using the native OS notifications. Native watching also falls back to
	// polling every second where it is not available.
	Poll time.Duration
	// Ready, when not nil, is closed once the watches are installed: a
	// change after that point is reported.
	Ready chan<- struct{}
}

func (o *WatchOptions) debounce() time.Duration {
	switch {
	case o.Debounce == 0:
		return DefaultDebounce
	case o.Debounce < 0:
		return 0
	}
	return o.Debounce
}

// Watch reports on out each time the file at path changes, is created,
// replaced or removed, once it has been quiet for the debounce time. It
// watches the parent directory, so editors that save by renaming a new file
// over the old one are seen. It follows symlinks and watches each link of
// the chain, so a dotfile-managed config is seen at its target and a
// retargeted link is followed. A missing directory is waited for.
//
// The send never blocks: give out a buffer of 1, and a pending value already
// means "changed". Watch returns nil when ctx ends, or an error when the OS
// refuses to watch.
func Watch(ctx context.Context, path string, opts WatchOptions, out chan<- struct{}) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	var b backend
	if opts.Poll > 0 {
		b = newPoller(opts.Poll)
	} else if b, err = newNative(); err != nil {
		return err
	} else if b == nil {
		b = newPoller(time.Second)
	}
	defer b.close() //nolint:errcheck // nothing to report once watching ends
	stop := context.AfterFunc(ctx, b.wake)
	defer stop()

	w := &watcher{path: path, dir: opts.Dir, stamps: map[string]stamp{}}
	missing, err := w.rewatch(b)
	if err != nil {
		return err
	}
	if opts.Ready != nil {
		close(opts.Ready)
	}
	debounce := opts.debounce()
	var pending, retry time.Time
	if missing {
		retry = time.Now().Add(retryMissing)
	}
	var evs []event
	for ctx.Err() == nil {
		timeout := time.Duration(-1)
		for _, t := range [...]time.Time{pending, retry} {
			if !t.IsZero() {
				if d := max(time.Until(t), 0); timeout < 0 || d < timeout {
					timeout = d
				}
			}
		}
		if evs, err = b.wait(timeout, evs[:0]); err != nil {
			return err
		}
		relevant, rewatch, recheck := w.classify(evs)
		now := time.Now()
		if !retry.IsZero() && !now.Before(retry) {
			rewatch, retry = true, time.Time{}
		}
		if recheck && retry.IsZero() {
			// A removed directory may linger, unreported, until its last
			// handle closes (Windows): look once more later.
			retry = now.Add(retryMissing)
		}
		if rewatch {
			changed := false
			if missing, changed, err = w.rewatchChanged(b); err != nil {
				return err
			}
			relevant = relevant || changed
			if missing && retry.IsZero() {
				retry = now.Add(retryMissing)
			}
		}
		if relevant {
			pending = now.Add(debounce)
		}
		if !pending.IsZero() && !now.Before(pending) {
			pending = time.Time{}
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

// backend is the native notification API of one OS.
type backend interface {
	// set replaces the watched directories with targets. It reports
	// whether a directory vanished before it could be watched.
	set(targets []*target) (missing bool, err error)
	// wait appends events to evs. It returns after events arrive, after
	// timeout (forever when negative), or after wake.
	wait(timeout time.Duration, evs []event) ([]event, error)
	// wake makes the current or next wait return. Safe for concurrent use.
	wake()
	close() error
}

// target is one watched directory and the names that matter in it.
type target struct {
	dir string
	// names are files whose changes are reported.
	names []string
	// all reports every visible file (Dir mode).
	all bool
	// next is the missing child on the way to the wanted directory: its
	// creation means the watches must move down.
	next string
}

func (t *target) has(name string) bool {
	return slices.Contains(t.names, name) || t.all && visible(name)
}

// event is a change in a watched directory. An empty name means "something
// changed in dir" (kqueue, polling, overflow); an empty dir means anything
// may have changed. lost means the watch itself vanished.
type event struct {
	dir, name string
	lost      bool
	// moved: the name was created, removed or renamed, so a link may now
	// point elsewhere. A plain write leaves links alone.
	moved bool
}

func visible(name string) bool {
	return name != "" && name[0] != '.' && name[len(name)-1] != '~'
}

type watcher struct {
	path    string
	dir     bool
	targets []*target
	chain   []string
	stamps  map[string]stamp
}

// chain lists path and every symlink target it goes through.
func chain(path string) []string {
	out := []string{filepath.Clean(path)}
	for len(out) <= 40 { // the kernel's symlink limit
		cur := out[len(out)-1]
		link, err := os.Readlink(cur)
		if err != nil {
			break
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(cur), link)
		}
		link = filepath.Clean(link)
		if slices.Contains(out, link) {
			break // a loop: the file is unreadable anyway
		}
		out = append(out, link)
	}
	return out
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// plan computes the targets for the current state of the file system.
func (w *watcher) plan() (targets []*target, files []string) {
	byDir := map[string]*target{}
	get := func(dir string) *target {
		t := byDir[dir]
		if t == nil {
			t = &target{dir: dir}
			byDir[dir] = t
			targets = append(targets, t)
		}
		return t
	}
	add := func(dir, name string, all bool) {
		if !isDir(dir) {
			// Watch the closest existing ancestor for the next missing child.
			child := dir
			for parent := filepath.Dir(child); ; parent = filepath.Dir(child) {
				if parent == child {
					return // nothing exists, not even the root
				}
				if isDir(parent) {
					get(parent).next = normName(filepath.Base(child))
					return
				}
				child = parent
			}
		}
		t := get(dir)
		t.all = t.all || all
		if name != "" && !slices.Contains(t.names, name) {
			t.names = append(t.names, name)
		}
	}
	if w.dir {
		files = []string{w.path}
		add(w.path, "", true)
	} else {
		files = chain(w.path)
		for _, f := range files {
			add(filepath.Dir(f), normName(filepath.Base(f)), false)
		}
	}
	return targets, files
}

func (w *watcher) rewatch(b backend) (missing bool, err error) {
	missing, _, err = w.rewatchChanged(b)
	return missing, err
}

// rewatchChanged installs the watches again and reports whether the watched
// files changed meanwhile: a link was retargeted, a missing directory
// appeared, or a file changed while it was not watched.
func (w *watcher) rewatchChanged(b backend) (missing, changed bool, err error) {
	targets, files := w.plan()
	changed = w.chain != nil && !slices.Equal(files, w.chain) || !sameTargets(targets, w.targets)
	w.targets, w.chain = targets, files
	missing, err = b.set(targets)
	for _, t := range targets {
		missing = missing || t.next != ""
	}
	old := w.stamps
	w.stamps = make(map[string]stamp, len(old))
	for _, t := range targets {
		w.stampsChanged(t)
	}
	changed = changed || !maps.Equal(old, w.stamps)
	return missing, changed, err
}

func sameTargets(a, b []*target) bool {
	return slices.EqualFunc(a, b, func(x, y *target) bool {
		return x.dir == y.dir && x.all == y.all && x.next == y.next && slices.Equal(x.names, y.names)
	})
}

// classify reports whether evs touch a watched file, and whether the
// watches must be installed again.
func (w *watcher) classify(evs []event) (relevant, rewatch, recheck bool) {
	relinked := false
	for _, ev := range evs {
		if ev.lost || ev.dir == "" {
			relevant, rewatch, recheck = true, true, true
			continue
		}
		t := w.find(ev.dir)
		if t == nil {
			continue
		}
		if ev.name == "" {
			if w.stampsChanged(t) {
				// kqueue watches files by descriptor: a replaced file needs a new one.
				relevant, rewatch = true, true
			}
			continue
		}
		name := normName(ev.name)
		if t.next != "" && name == t.next {
			relevant, rewatch = true, true
		}
		// Some systems (Windows, network file systems) do not report a
		// watched directory that was removed: check it on removals.
		if ev.moved && !rewatch && !isDir(t.dir) {
			relevant, rewatch = true, true
		}
		if t.has(name) {
			relevant = true
			relinked = relinked || ev.moved
		}
	}
	if relinked && !rewatch && !w.dir && !slices.Equal(chain(w.path), w.chain) {
		rewatch = true // a link points elsewhere now
	}
	return relevant, rewatch, recheck || relinked
}

func (w *watcher) find(dir string) *target {
	for _, t := range w.targets {
		if t.dir == dir {
			return t
		}
	}
	return nil
}

// stamp identifies a version of a file without reading it.
type stamp struct {
	size  int64
	mtime int64
	mode  fs.FileMode
	id    uint64
}

func stampOf(info fs.FileInfo) stamp {
	return stamp{size: info.Size(), mtime: info.ModTime().UnixNano(), mode: info.Mode(), id: fileID(info)}
}

// stampsChanged refreshes the stamps of the files that matter in t and
// reports whether any was created, changed or removed.
func (w *watcher) stampsChanged(t *target) bool {
	changed := false
	check := func(name string, info fs.FileInfo) {
		key := t.dir + string(filepath.Separator) + name
		old, had := w.stamps[key]
		if info == nil {
			if had {
				delete(w.stamps, key)
				changed = true
			}
			return
		}
		if s := stampOf(info); !had || old != s {
			w.stamps[key] = s
			changed = true
		}
	}
	names := t.names
	if t.next != "" {
		names = append(slices.Clip(names), t.next)
	}
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(t.dir, name))
		if err != nil {
			info = nil
		}
		check(name, info)
	}
	if t.all {
		entries, _ := os.ReadDir(t.dir)
		prefix := t.dir + string(filepath.Separator)
		live := map[string]bool{}
		for _, e := range entries {
			if !visible(e.Name()) || e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			live[e.Name()] = true
			check(e.Name(), info)
		}
		for key := range w.stamps {
			if rest, ok := strings.CutPrefix(key, prefix); ok && !strings.ContainsRune(rest, filepath.Separator) &&
				!live[rest] && !slices.Contains(names, rest) {
				delete(w.stamps, key)
				changed = true
			}
		}
	}
	return changed
}

// normName folds the case of names on Windows, whose file names ignore it.
func normName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}
