package kvconf

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// quiet is how long a test waits to be sure no change is reported.
const quiet = 400 * time.Millisecond

type watchEnv struct {
	t    *testing.T
	dir  string
	path string
	out  chan struct{}
}

func (e *watchEnv) write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.dir, rel)
	must(e.t, os.MkdirAll(filepath.Dir(p), 0o700))
	must(e.t, os.WriteFile(p, []byte(content), 0o600))
}

// replace saves like most editors: write a temporary file, rename it over.
func (e *watchEnv) replace(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.dir, rel)
	tmp := p + ".tmp~"
	must(e.t, os.WriteFile(tmp, []byte(content), 0o600))
	must(e.t, os.Rename(tmp, p))
}

func (e *watchEnv) symlink(target, rel string) {
	e.t.Helper()
	p := filepath.Join(e.dir, rel)
	_ = os.Remove(p)
	if err := os.Symlink(filepath.Join(e.dir, target), p); err != nil {
		e.t.Skip("symlinks unavailable:", err)
	}
}

// fastWait is how long a change that needs no retry may take. It stays below
// retryMissing, so a broken fast path is not hidden by the periodic look.
const fastWait = retryMissing - 500*time.Millisecond

func (e *watchEnv) expectChange(why string) {
	e.t.Helper()
	e.expectChangeWithin(why, fastWait)
}

// expectChangeWithin is expectChange for a step that legitimately relies on
// the retry or recheck, which fires after retryMissing.
func (e *watchEnv) expectChangeWithin(why string, d time.Duration) {
	e.t.Helper()
	select {
	case <-e.out:
	case <-time.After(d):
		e.t.Fatalf("no change reported within %v after %s", d, why)
	}
}

func (e *watchEnv) expectQuiet(why string) {
	e.t.Helper()
	select {
	case <-e.out:
		e.t.Fatalf("unexpected change after %s", why)
	case <-time.After(quiet):
	}
}

// settle lets a step's events drain so the next expectation is its own.
func (e *watchEnv) settle() {
	time.Sleep(quiet / 2)
	select {
	case <-e.out:
	default:
	}
}

type backendCase struct {
	name string
	poll time.Duration
}

// slowWait is the deadline for a step that relies on the retry on this OS.
func slowWait() time.Duration {
	if runtime.GOOS == "windows" {
		return retryMissing + 3*time.Second
	}
	return fastWait
}

func backends() []backendCase {
	return []backendCase{{name: "native"}, {name: "poll", poll: 20 * time.Millisecond}}
}

func TestWatch(t *testing.T) {
	tests := []struct {
		name string
		rel  string // watched path, relative to the temp dir
		dir  bool
		// setup runs before the watch starts.
		setup func(e *watchEnv)
		// steps run in order once the watch is ready.
		steps func(e *watchEnv)
	}{
		{
			name:  "write in place",
			rel:   "config",
			setup: func(e *watchEnv) { e.write("config", "a = 1\n") },
			steps: func(e *watchEnv) {
				e.write("config", "a = 2\n")
				e.expectChange("an in-place write")
			},
		},
		{
			name:  "atomic replace",
			rel:   "config",
			setup: func(e *watchEnv) { e.write("config", "a = 1\n") },
			steps: func(e *watchEnv) {
				e.replace("config", "a = 2\n")
				e.expectChange("a rename over the file")
				e.replace("config", "a = 3\n")
				e.expectChange("a second rename")
			},
		},
		{
			name: "create then remove",
			rel:  "config",
			steps: func(e *watchEnv) {
				e.write("config", "a = 1\n")
				e.expectChange("creation")
				must(e.t, os.Remove(e.path))
				e.expectChange("removal")
			},
		},
		{
			name:  "other files are ignored",
			rel:   "config",
			setup: func(e *watchEnv) { e.write("config", "a = 1\n") },
			steps: func(e *watchEnv) {
				e.write("other", "x")
				e.write(".config.swp", "x")
				e.expectQuiet("writing other files")
			},
		},
		{
			name: "missing directory appears",
			rel:  "a/b/config",
			steps: func(e *watchEnv) {
				e.write("a/b/config", "a = 1\n")
				// Windows may only find the new directories on the retry.
				e.expectChangeWithin("the directories and file appeared", slowWait())
				e.settle()
				e.write("a/b/config", "a = 2\n")
				e.expectChange("a write once the directory exists")
			},
		},
		{
			name:  "directory removed and recreated",
			rel:   "d/config",
			setup: func(e *watchEnv) { e.write("d/config", "a = 1\n") },
			steps: func(e *watchEnv) {
				must(e.t, os.RemoveAll(filepath.Join(e.dir, "d")))
				// Windows reports a removed directory only after the watch on
				// it is closed: the recheck finds it.
				e.expectChangeWithin("the directory removal", slowWait())
				e.settle()
				e.write("d/config", "a = 2\n")
				e.expectChangeWithin("the recreated file", slowWait())
			},
		},
		{
			name: "symlink target changes",
			rel:  "config",
			setup: func(e *watchEnv) {
				e.write("dotfiles/config", "a = 1\n")
				e.symlink("dotfiles/config", "config")
			},
			steps: func(e *watchEnv) {
				e.replace("dotfiles/config", "a = 2\n")
				e.expectChange("a save at the link target")
			},
		},
		{
			name: "symlink retargeted",
			rel:  "config",
			setup: func(e *watchEnv) {
				e.write("one/config", "a = 1\n")
				e.write("two/config", "a = 2\n")
				e.symlink("one/config", "config")
			},
			steps: func(e *watchEnv) {
				e.symlink("two/config", "config")
				e.expectChange("retargeting the link")
				e.settle()
				e.write("two/config", "a = 3\n")
				e.expectChange("a write at the new target")
				e.settle()
				e.write("one/config", "a = 4\n")
				e.expectQuiet("a write at the old target")
			},
		},
		{
			name:  "directory renamed away and replaced",
			rel:   "app/config",
			setup: func(e *watchEnv) { e.write("app/config", "a = 1\n"); e.write("new/config", "a = 2\n") },
			steps: func(e *watchEnv) {
				must(e.t, os.Rename(filepath.Join(e.dir, "app"), filepath.Join(e.dir, "app.old")))
				must(e.t, os.Rename(filepath.Join(e.dir, "new"), filepath.Join(e.dir, "app")))
				e.expectChange("the directory swap")
				e.settle()
				e.write("app/config", "a = 3\n")
				e.expectChange("a write in the new directory")
				e.settle()
				e.write("app.old/config", "a = 4\n")
				e.expectQuiet("a write in the old directory")
			},
		},
		{
			name: "symlinked parent directory retargeted",
			rel:  "app/config",
			setup: func(e *watchEnv) {
				e.write("one/config", "a = 1\n")
				e.write("two/config", "a = 2\n")
				e.symlink("one", "app")
			},
			steps: func(e *watchEnv) {
				e.write("one/config", "a = 3\n")
				e.expectChange("a write through the linked directory")
				e.settle()
				e.symlink("two", "app")
				e.expectChange("retargeting the directory link")
				e.settle()
				e.write("two/config", "a = 4\n")
				e.expectChange("a write in the new target")
				e.settle()
				e.write("one/config", "a = 5\n")
				e.expectQuiet("a write in the old target")
			},
		},
		{
			name:  "directory mode ignores subdirectories",
			rel:   "scripts",
			dir:   true,
			setup: func(e *watchEnv) { e.write("scripts/a.sh", "1") },
			steps: func(e *watchEnv) {
				must(e.t, os.Mkdir(filepath.Join(e.dir, "scripts", "sub"), 0o700))
				e.expectQuiet("creating a subdirectory")
			},
		},
		{
			name:  "directory mode",
			rel:   "scripts",
			dir:   true,
			setup: func(e *watchEnv) { e.write("scripts/a.sh", "1") },
			steps: func(e *watchEnv) {
				e.write("scripts/b.sh", "1")
				e.expectChange("a new file")
				e.settle()
				e.write("scripts/a.sh", "2")
				e.expectChange("a modified file")
				e.settle()
				e.write("scripts/.hidden", "1")
				e.write("scripts/a.sh~", "1")
				e.expectQuiet("hidden and backup files")
			},
		},
	}
	for _, bc := range backends() {
		for _, tt := range tests {
			t.Run(bc.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				if runtime.GOOS == "darwin" {
					dir, _ = filepath.EvalSymlinks(dir) // /var is a link to /private/var
				}
				e := &watchEnv{t: t, dir: dir, path: filepath.Join(dir, tt.rel), out: make(chan struct{}, 1)}
				if tt.setup != nil {
					tt.setup(e)
				}
				ctx, cancel := context.WithCancel(context.Background())
				ready := make(chan struct{})
				errc := make(chan error, 1)
				go func() {
					errc <- Watch(ctx, e.path, WatchOptions{Dir: tt.dir, Poll: bc.poll, Debounce: 20 * time.Millisecond, Ready: ready}, e.out)
				}()
				select {
				case <-ready:
				case err := <-errc:
					t.Fatalf("Watch: %v", err)
				}
				tt.steps(e)
				cancel()
				select {
				case err := <-errc:
					must(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("Watch did not return after cancel")
				}
			})
		}
	}
}

// Every write restarts the debounce: a burst longer than the debounce is
// reported once, after its last write.
func TestWatchDebounce(t *testing.T) {
	const (
		debounce = 200 * time.Millisecond
		writes   = 10
		gap      = 50 * time.Millisecond
	)
	for _, bc := range backends() {
		t.Run(bc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			must(t, os.WriteFile(path, nil, 0o600))
			out := make(chan struct{}, 10)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{})
			go func() {
				_ = Watch(ctx, path, WatchOptions{Poll: bc.poll, Debounce: debounce, Ready: ready}, out)
			}()
			<-ready
			var lastWrite time.Time
			for i := range writes {
				lastWrite = time.Now()
				must(t, os.WriteFile(path, []byte{byte('a' + i)}, 0o600))
				time.Sleep(gap)
			}
			select {
			case <-out:
				if d := time.Since(lastWrite); d < debounce {
					t.Fatalf("reported %v after the last write, before the %v debounce", d, debounce)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no change reported")
			}
			select {
			case <-out:
				t.Fatal("a burst of writes was reported more than once")
			case <-time.After(quiet):
			}
		})
	}
}

// An atomic save is one change: nothing is reported again when the watcher
// looks at the files later. This guards Windows, where the recheck after a
// rename would otherwise report the same save a second time.
func TestWatchReportsASaveOnce(t *testing.T) {
	for _, bc := range backends() {
		t.Run(bc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			must(t, os.WriteFile(path, []byte("a = 1\n"), 0o600))
			out := make(chan struct{}, 10)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{})
			go func() {
				_ = Watch(ctx, path, WatchOptions{Poll: bc.poll, Debounce: 20 * time.Millisecond, Ready: ready}, out)
			}()
			<-ready
			must(t, WriteFile(path, []byte("a = 2\n"), 0o600))
			select {
			case <-out:
			case <-time.After(slowWait()):
				t.Fatal("no change reported")
			}
			select {
			case <-out:
				t.Fatal("one save was reported twice")
			case <-time.After(retryMissing + 500*time.Millisecond):
			}
		})
	}
}
