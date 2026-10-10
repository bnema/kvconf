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

func (e *watchEnv) expectChange(why string) {
	e.t.Helper()
	select {
	case <-e.out:
	case <-time.After(5 * time.Second):
		e.t.Fatalf("no change reported after %s", why)
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
				e.expectChange("the directories and file appeared")
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
				e.expectChange("the directory removal")
				e.settle()
				e.write("d/config", "a = 2\n")
				e.expectChange("the recreated file")
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

func TestWatchDebounce(t *testing.T) {
	for _, bc := range backends() {
		t.Run(bc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			must(t, os.WriteFile(path, nil, 0o600))
			out := make(chan struct{}, 10)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{})
			go func() {
				_ = Watch(ctx, path, WatchOptions{Poll: bc.poll, Debounce: 300 * time.Millisecond, Ready: ready}, out)
			}()
			<-ready
			for i := range 5 {
				must(t, os.WriteFile(path, []byte{byte('a' + i)}, 0o600))
				time.Sleep(50 * time.Millisecond)
			}
			select {
			case <-out:
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
// looks at the files later.
func TestWatchReportsASaveOnce(t *testing.T) {
	for _, bc := range backends() {
		t.Run(bc.name, func(t *testing.T) {
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
			case <-time.After(5 * time.Second):
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

func TestWatchCancelBeforeEvents(t *testing.T) {
	for _, bc := range backends() {
		t.Run(bc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			done := make(chan error, 1)
			go func() {
				done <- Watch(ctx, filepath.Join(t.TempDir(), "config"), WatchOptions{Poll: bc.poll}, make(chan struct{}, 1))
			}()
			select {
			case err := <-done:
				must(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Watch ignored a cancelled context")
			}
		})
	}
}
