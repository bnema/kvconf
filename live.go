package kvconf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync/atomic"
)

// Load reads the config file at path into a new T: it starts from the
// defaults (SetDefaults when *T implements Defaulter), then applies the file.
// A missing file gives the defaults, unless opts.RequireFile is set.
func Load[T any](path string, opts Options) (*T, []Warning, error) {
	cfg, _, warnings, err := load[T](path, opts)
	return cfg, warnings, err
}

func load[T any](path string, opts Options) (*T, []Entry, []Warning, error) {
	data, err := ReadFile(path, opts)
	if err != nil {
		if isNotExist(err) && !opts.RequireFile {
			cfg := newDefault[T]()
			if v, ok := any(cfg).(Validator); ok {
				if err := v.Validate(); err != nil {
					return nil, nil, nil, err
				}
			}
			return cfg, nil, nil, nil
		}
		return nil, nil, nil, err
	}
	return decodeFile[T](path, data, opts)
}

func decodeFile[T any](path string, data []byte, opts Options) (*T, []Entry, []Warning, error) {
	cfg := newDefault[T]()
	entries, warnings, err := ParseBytes(data, opts)
	if err != nil {
		return nil, nil, warnings, fmt.Errorf("%s: %w", path, err)
	}
	more, err := Decode(entries, cfg, opts)
	warnings = append(warnings, more...)
	if err != nil {
		return nil, nil, warnings, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, entries, warnings, nil
}

func newDefault[T any]() *T {
	cfg := new(T)
	if d, ok := any(cfg).(Defaulter); ok {
		d.SetDefaults()
	}
	return cfg
}

// Change describes a reload.
type Change[T any] struct {
	Old, New *T
	// Keys lists, sorted, the keys added, removed or changed.
	Keys     []string
	Warnings []Warning
}

// LiveOptions controls a Live config.
type LiveOptions[T any] struct {
	Options
	Watch WatchOptions
	// OnChange is called from Run after each reload that changed a value.
	OnChange func(Change[T])
	// OnError is called from Run when a reload fails; the current config
	// stays in place. A removed file reports an error wrapping
	// fs.ErrNotExist.
	OnError func(error)
}

// Live is a config that reloads itself when its file changes. Get is safe
// for concurrent use; a config it returned is never modified, a reload
// replaces it.
type Live[T any] struct {
	path    string
	opts    LiveOptions[T]
	cur     atomic.Pointer[T]
	entries []Entry // owned by Run
	running atomic.Bool
}

// Open loads the config file at path and returns it with the warnings of
// the first load. Call Run to follow its changes.
func Open[T any](path string, opts LiveOptions[T]) (*Live[T], []Warning, error) {
	cfg, entries, warnings, err := load[T](path, opts.Options)
	if err != nil {
		return nil, warnings, err
	}
	l := &Live[T]{path: path, opts: opts, entries: entries}
	l.cur.Store(cfg)
	return l, warnings, nil
}

// Get returns the current config. Do not modify it.
func (l *Live[T]) Get() *T { return l.cur.Load() }

// Path returns the path of the config file.
func (l *Live[T]) Path() string { return l.path }

// Run watches the file and reloads it until ctx ends. A reload that fails,
// or a removed file, keeps the current config. A save that changes no value
// (a comment, spacing, the same file written again) reloads nothing. Run
// returns nil when ctx ends, or the error of the watcher. Call it once.
func (l *Live[T]) Run(ctx context.Context) error {
	if !l.running.CompareAndSwap(false, true) {
		return errors.New("kvconf: Live.Run called twice")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	changed := make(chan struct{}, 1)
	ready := make(chan struct{})
	wopts := l.opts.Watch
	userReady := wopts.Ready
	wopts.Ready = ready
	errc := make(chan error, 1)
	go func() { errc <- Watch(ctx, l.path, wopts, changed) }()
	select {
	case <-ready:
		// A save between Open and the watch would go unseen: check once.
		l.reload()
		if userReady != nil {
			close(userReady)
		}
	case err := <-errc:
		return err
	}
	for {
		select {
		case <-changed:
			l.reload()
		case err := <-errc:
			return err
		}
	}
}

func (l *Live[T]) reload() {
	data, err := ReadFile(l.path, l.opts.Options)
	if isNotExist(err) {
		// Keep the current config: a removed file is often about to be
		// replaced, and an empty config is rarely what the user meant.
		if l.entries != nil {
			l.report(fmt.Errorf("%s: %w", l.path, fs.ErrNotExist))
		}
		return
	}
	if err != nil {
		l.report(err)
		return
	}
	cfg, entries, warnings, err := decodeFile[T](l.path, data, l.opts.Options)
	if err != nil {
		l.report(err)
		return
	}
	if sameEntries(entries, l.entries) {
		return
	}
	// Compare every assignment, not the last one: a repeated key whose
	// last value is invalid decodes to an earlier one.
	keys := changedKeys(l.entries, entries)
	l.entries = entries
	old := l.cur.Swap(cfg)
	if l.opts.OnChange != nil {
		l.opts.OnChange(Change[T]{Old: old, New: cfg, Keys: keys, Warnings: warnings})
	}
}

// changedKeys lists, sorted, the keys whose assignments differ.
func changedKeys(old, cur []Entry) []string {
	group := func(es []Entry) map[string][]string {
		m := make(map[string][]string, len(es))
		for _, e := range es {
			m[e.Key] = append(m[e.Key], e.Value)
		}
		return m
	}
	a, b := group(old), group(cur)
	var keys []string
	for k, v := range b {
		if !slices.Equal(a[k], v) {
			keys = append(keys, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

func (l *Live[T]) report(err error) {
	if l.opts.OnError != nil && !errors.Is(err, context.Canceled) {
		l.opts.OnError(err)
	}
}
