package kvconf

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name  string
		setup func(t *testing.T) string
		opts  Options
		want  string
		err   error
	}{
		{name: "regular", setup: func(t *testing.T) string { return writeTemp(t, dir, "ok", "a = 1\n") }, want: "a = 1\n"},
		{name: "missing", setup: func(t *testing.T) string { return filepath.Join(dir, "none") }, err: fs.ErrNotExist},
		{name: "directory", setup: func(t *testing.T) string { return dir }, err: ErrNotRegular},
		{
			name:  "too large",
			setup: func(t *testing.T) string { return writeTemp(t, dir, "big", "0123456789") },
			opts:  Options{MaxBytes: 4},
			err:   ErrTooLarge,
		},
		{
			name:  "at the limit",
			setup: func(t *testing.T) string { return writeTemp(t, dir, "limit", "0123") },
			opts:  Options{MaxBytes: 4},
			want:  "0123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := ReadFile(tt.setup(t), tt.opts)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			}
			must(t, err)
			eq(t, string(data), tt.want)
		})
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	abs := filepath.Join(home, "x")
	tests := []struct {
		in, want string
		err      bool
	}{
		{in: "~/x", want: abs},
		{in: "~", want: home},
		{in: abs + string(filepath.Separator) + ".", want: abs},
		{in: "", err: true},
		{in: "rel", err: true},
		{in: "~user/x", err: true},
		{in: "/a\x00b", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ExpandPath(tt.in)
			if (err != nil) != tt.err || got != tt.want {
				t.Fatalf("ExpandPath(%q) = %q, %v", tt.in, got, err)
			}
		})
	}
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	// The platform config dir, spelled out rather than recomputed.
	native := filepath.Join(home, ".config")
	switch runtime.GOOS {
	case "darwin":
		native = filepath.Join(home, "Library", "Application Support")
	case "windows":
		native = filepath.Join(home, "AppData")
	}
	tests := []struct{ name, xdg, want string }{
		{name: "absolute XDG", xdg: xdg, want: filepath.Join(xdg, "app", "config")},
		{name: "relative XDG is ignored", xdg: "relative", want: filepath.Join(native, "app", "config")},
		{name: "no XDG", want: filepath.Join(native, "app", "config")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("AppData", filepath.Join(home, "AppData"))
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			got, err := DefaultPath("app", "config")
			must(t, err)
			eq(t, got, tt.want)
		})
	}
}
