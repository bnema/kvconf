package kvconf

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type liveConfig struct {
	Theme string        `kv:"theme" oneof:"dark|light"`
	After time.Duration `kv:"after"`
}

func (c *liveConfig) SetDefaults() { c.Theme, c.After = "dark", time.Minute }

type strictPair struct {
	A int `kv:"a"`
	B int `kv:"b"`
}

func (p *strictPair) Validate() error {
	if p.A > p.B {
		return errors.New("a must not exceed b")
	}
	return nil
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name     string
		content  *string // nil: no file
		opts     Options
		want     liveConfig
		warnings int
		err      string
	}{
		{name: "missing file gives defaults", want: liveConfig{"dark", time.Minute}},
		{name: "missing file required", opts: Options{RequireFile: true}, err: "config"},
		{name: "file over defaults", content: ptr("theme = light\n"), want: liveConfig{"light", time.Minute}},
		{name: "lenient bad value", content: ptr("theme = blue\nafter = 2m\n"), want: liveConfig{"dark", 2 * time.Minute}, warnings: 1},
		{name: "strict bad value", content: ptr("theme = blue\n"), opts: Options{Strict: true}, err: "line 1: theme: must be dark or light"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if tt.content != nil {
				must(t, os.WriteFile(path, []byte(*tt.content), 0o600))
			}
			cfg, warnings, err := Load[liveConfig](path, tt.opts)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			must(t, err)
			eq(t, *cfg, tt.want)
			eq(t, len(warnings), tt.warnings)
		})
	}
}

func TestLoadValidatesDefaults(t *testing.T) {
	_, _, err := Load[strictPair](filepath.Join(t.TempDir(), "none"), Options{})
	must(t, err)
	path := writeTemp(t, t.TempDir(), "config", "a = 3\nb = 2\n")
	if _, _, err := Load[strictPair](path, Options{}); err == nil || err.Error() != path+": a must not exceed b" {
		t.Fatalf("err = %v", err)
	}
}

// liveRecorder collects what a Live reports.
type liveRecorder struct {
	mu      sync.Mutex
	changes []Change[liveConfig]
	errs    []error
	signal  chan struct{}
}

func newRecorder() *liveRecorder { return &liveRecorder{signal: make(chan struct{}, 16)} }

func (r *liveRecorder) opts() LiveOptions[liveConfig] {
	return LiveOptions[liveConfig]{
		Watch: WatchOptions{Debounce: 20 * time.Millisecond},
		OnChange: func(c Change[liveConfig]) {
			r.mu.Lock()
			r.changes = append(r.changes, c)
			r.mu.Unlock()
			r.signal <- struct{}{}
		},
		OnError: func(err error) {
			r.mu.Lock()
			r.errs = append(r.errs, err)
			r.mu.Unlock()
			r.signal <- struct{}{}
		},
	}
}

func (r *liveRecorder) wait(t *testing.T, why string) {
	t.Helper()
	select {
	case <-r.signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("nothing reported after %s", why)
	}
}

func (r *liveRecorder) quiet(t *testing.T, why string) {
	t.Helper()
	select {
	case <-r.signal:
		t.Fatalf("unexpected report after %s", why)
	case <-time.After(quiet):
	}
}

func (r *liveRecorder) last() (Change[liveConfig], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var c Change[liveConfig]
	var err error
	if len(r.changes) > 0 {
		c = r.changes[len(r.changes)-1]
	}
	if len(r.errs) > 0 {
		err = r.errs[len(r.errs)-1]
	}
	return c, err
}

func TestLive(t *testing.T) {
	tests := []struct {
		name    string
		initial *string
		steps   func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder)
	}{
		{
			name:    "reload on change",
			initial: ptr("theme = dark\n"),
			steps: func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder) {
				old := l.Get()
				must(t, WriteFile(path, []byte("theme = light\n"), 0o600))
				r.wait(t, "a change")
				c, _ := r.last()
				eq(t, c.Keys, []string{"theme"})
				eq(t, c.Old, old)
				eq(t, l.Get().Theme, "light")
				eq(t, old.Theme, "dark") // a published config is never modified
			},
		},
		{
			name:    "comment and spacing only",
			initial: ptr("theme = light\n"),
			steps: func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder) {
				must(t, os.WriteFile(path, []byte("# note\ntheme   =   light   # same\n"), 0o600))
				r.quiet(t, "a save that changes no value")
			},
		},
		{
			name:    "broken file keeps the config",
			initial: ptr("theme = light\n"),
			steps: func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder) {
				must(t, os.WriteFile(path, []byte("theme = light\n\xff\n"), 0o600))
				r.wait(t, "an invalid file")
				_, err := r.last()
				if !errors.Is(err, ErrNotUTF8) {
					t.Fatalf("err = %v", err)
				}
				eq(t, l.Get().Theme, "light")
			},
		},
		{
			name:    "removed file keeps the config",
			initial: ptr("theme = light\n"),
			steps: func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder) {
				must(t, os.Remove(path))
				r.wait(t, "a removal")
				_, err := r.last()
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("err = %v", err)
				}
				eq(t, l.Get().Theme, "light")
				must(t, os.WriteFile(path, []byte("theme = dark\n"), 0o600))
				r.wait(t, "the file came back")
				eq(t, l.Get().Theme, "dark")
			},
		},
		{
			name: "file created after start",
			steps: func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder) {
				eq(t, l.Get().Theme, "dark")
				must(t, os.WriteFile(path, []byte("after = 5s\n"), 0o600))
				r.wait(t, "creation")
				eq(t, *l.Get(), liveConfig{"dark", 5 * time.Second})
			},
		},
		{
			name:    "repeated key with an invalid last value",
			initial: ptr("after = 5s\nafter = bad\n"),
			steps: func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder) {
				eq(t, l.Get().After, 5*time.Second)
				must(t, WriteFile(path, []byte("after = 6s\nafter = bad\n"), 0o600))
				r.wait(t, "a change of the valid value")
				eq(t, l.Get().After, 6*time.Second)
				c, _ := r.last()
				eq(t, c.Keys, []string{"after"})
			},
		},
		{
			name:    "warnings are reported with the change",
			initial: ptr("after = 1s\n"),
			steps: func(t *testing.T, path string, l *Live[liveConfig], r *liveRecorder) {
				must(t, WriteFile(path, []byte("after = 2s\ntheme = blue\n"), 0o600))
				r.wait(t, "a change with a bad value")
				c, _ := r.last()
				eq(t, warningStrings(c.Warnings), []string{"line 2: theme: must be dark or light"})
				eq(t, l.Get().After, 2*time.Second)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config")
			if tt.initial != nil {
				must(t, os.WriteFile(path, []byte(*tt.initial), 0o600))
			}
			r := newRecorder()
			opts := r.opts()
			ready := make(chan struct{})
			opts.Watch.Ready = ready
			l, _, err := Open(path, opts)
			must(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- l.Run(ctx) }()
			<-ready
			tt.steps(t, path, l, r)
			cancel()
			must(t, <-done)
		})
	}
}

func TestLiveSeesSaveBetweenOpenAndRun(t *testing.T) {
	path := writeTemp(t, t.TempDir(), "config", "theme = dark\n")
	r := newRecorder()
	l, _, err := Open(path, r.opts())
	must(t, err)
	must(t, os.WriteFile(path, []byte("theme = light\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Run(ctx) }()
	r.wait(t, "a save before Run")
	eq(t, l.Get().Theme, "light")
}

func TestLiveRunTwice(t *testing.T) {
	l, _, err := Open(filepath.Join(t.TempDir(), "config"), LiveOptions[liveConfig]{})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	if err := l.Run(ctx); err == nil {
		t.Fatal("a second Run was accepted")
	}
}

func TestLiveConcurrentGet(t *testing.T) {
	path := writeTemp(t, t.TempDir(), "config", "after = 1s\n")
	r := newRecorder()
	l, _, err := Open(path, r.opts())
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Run(ctx) }()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if l.Get().Theme == "" {
						t.Error("saw a half-built config")
					}
				}
			}
		})
	}
	for i := range 5 {
		must(t, WriteFile(path, []byte("after = "+time.Duration(i+2).String()+"\n"), 0o600))
		r.wait(t, "a change")
	}
	close(stop)
	wg.Wait()
}
