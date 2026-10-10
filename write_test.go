package kvconf

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func ptr(s string) *string { return &s }

func TestSet(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		values map[string]*string
		want   string
	}{
		{name: "empty file", in: "", values: map[string]*string{"a": ptr("1")}, want: "a = 1\n"},
		{name: "no final newline", in: "x = 1", values: map[string]*string{"a": ptr("1")}, want: "x = 1\na = 1\n"},
		{
			name:   "replace keeps spacing and comment",
			in:     "# top\n  a  =   1   # one\nb = 2\n",
			values: map[string]*string{"a": ptr("10")},
			want:   "# top\n  a  =   10   # one\nb = 2\n",
		},
		{
			name:   "replace the last assignment",
			in:     "a = 1\na = 2\n",
			values: map[string]*string{"a": ptr("3")},
			want:   "a = 1\na = 3\n",
		},
		{
			name:   "replace empty value",
			in:     "a =\n",
			values: map[string]*string{"a": ptr("1")},
			want:   "a = 1\n",
		},
		{
			name:   "enable commented assignment",
			in:     "# a = 1\nb = 2\n",
			values: map[string]*string{"a": ptr("5")},
			want:   "a = 5\nb = 2\n",
		},
		{
			name:   "insert beside the longest dotted parent",
			in:     "output.DP-1.scale = 1\noutput.DP-2.hdr = on\nother = 1\n",
			values: map[string]*string{"output.DP-2.scale": ptr("2")},
			want:   "output.DP-1.scale = 1\noutput.DP-2.hdr = on\noutput.DP-2.scale = 2\nother = 1\n",
		},
		{
			name:   "insert keeps CRLF",
			in:     "a.x = 1\r\nb = 2\r\n",
			values: map[string]*string{"a.y": ptr("3")},
			want:   "a.x = 1\r\na.y = 3\r\nb = 2\r\n",
		},
		{
			name:   "quote when needed",
			in:     "",
			values: map[string]*string{"a": ptr("x # y"), "b": ptr(" pad"), "c": ptr("")},
			want:   "a = \"x # y\"\nb = \" pad\"\nc = \"\"\n",
		},
		{
			name:   "remove every assignment",
			in:     "a = 1\nb = 2\na = 3\n",
			values: map[string]*string{"a": nil},
			want:   "b = 2\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Set([]byte(tt.in), tt.values)
			must(t, err)
			if string(got) != tt.want {
				t.Errorf("got\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestSetRejectsInjection(t *testing.T) {
	for _, key := range []string{"", "a = b", "a\nadmin", "bind.q = y\nadmin", "# a", " a"} {
		t.Run(key, func(t *testing.T) {
			if _, err := Set(nil, map[string]*string{key: ptr("v")}); err == nil {
				t.Fatalf("Set accepted key %q", key)
			}
		})
	}
}

func TestMarshalRejectsInjection(t *testing.T) {
	tests := []struct {
		name string
		v    any
	}{
		{"map name with newline", &marshalConfig{Binds: map[string]string{"x = 1\nadmin": "y"}}},
		{"map name with equals", &marshalConfig{Binds: map[string]string{"a=b": "y"}}},
		{"catch-all key", &marshalConfig{Rest: map[string]string{"a\nb": "y"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Marshal(tt.v); err == nil {
				t.Fatal("Marshal accepted an injected key")
			}
		})
	}
}

// upper marshals with a pointer receiver, like many TextMarshalers.
type upper string

func (u *upper) UnmarshalText(b []byte) error { *u = upper(strings.ToLower(string(b))); return nil }
func (u *upper) MarshalText() ([]byte, error) { return []byte(strings.ToUpper(string(*u))), nil }

func TestMarshalPointerReceiver(t *testing.T) {
	type cfg struct {
		U upper            `kv:"u"`
		M map[string]upper `kv:"m.*"`
	}
	data, err := Marshal(&cfg{U: "hello", M: map[string]upper{"a": "x"}})
	must(t, err)
	eq(t, string(data), "u = HELLO\nm.a = X\n")
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, v := range []string{"", "plain", "#ff0000", "a # b", " lead", "trail ", `"q"`, "it's", "a\nb", `back\slash`, "x\t#y", `it's "x # y"`, "don't # x"} {
		t.Run(v, func(t *testing.T) {
			entries, _, err := ParseBytes([]byte("k = "+Quote(v)+"\n"), Options{})
			if err != nil || len(entries) != 1 || entries[0].Value != v {
				t.Fatalf("Quote(%q) = %q reads back as %#v, %v", v, Quote(v), entries, err)
			}
		})
	}
}

type marshalConfig struct {
	Name  string        `kv:"name"`
	On    bool          `kv:"on"`
	After time.Duration `kv:"after,off"`
	List  []int         `kv:"list"`
	Dir   Path          `kv:"dir"`
	Sub   struct {
		N uint8 `kv:"n"`
	} `kv:"sub"`
	Binds map[string]string `kv:"bind.*"`
	Outs  map[string]output `kv:"output.*"`
	Rest  map[string]string `kv:"*"`
}

func TestMarshalRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	in := marshalConfig{
		Name: "a # b", On: true, List: []int{1, 2}, Dir: Path(dir),
		Binds: map[string]string{"z": "2", "a": "1"},
		Outs:  map[string]output{"DP-1": {Scale: 1.5, HDR: true, Bright: 100}},
		Rest:  map[string]string{"extra": "x"},
	}
	in.Sub.N = 7
	data, err := Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}
	want := `name = "a # b"
on = on
after = off
list = 1,2
dir = ` + Quote(dir) + `
sub.n = 7
bind.a = 1
bind.z = 2
output.DP-1.scale = 1.5
output.DP-1.hdr = on
output.DP-1.primary = off
output.DP-1.sdr-brightness = 100
extra = x
`
	if string(data) != want {
		t.Fatalf("Marshal =\n%s\nwant\n%s", data, want)
	}
	var out marshalConfig
	if w, err := Unmarshal(data, &out, Options{Strict: true}); err != nil || len(w) > 0 {
		t.Fatalf("Unmarshal: %v %v", w, err)
	}
	eq(t, out, in)
}

func TestWriteFile(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string // returns the path to write
		check func(t *testing.T, dir, path string)
	}{
		{
			name:  "new file and directory",
			setup: func(t *testing.T, dir string) string { return filepath.Join(dir, "sub", "config") },
			check: func(t *testing.T, dir, path string) {
				eq(t, readString(t, path), "a = 1\n")
				if runtime.GOOS != "windows" {
					info, _ := os.Stat(path)
					eq(t, info.Mode().Perm(), os.FileMode(0o600))
				}
			},
		},
		{
			name: "existing file keeps its mode",
			setup: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, "config")
				must(t, os.WriteFile(p, []byte("old"), 0o640))
				return p
			},
			check: func(t *testing.T, dir, path string) {
				eq(t, readString(t, path), "a = 1\n")
				if runtime.GOOS != "windows" {
					info, _ := os.Stat(path)
					eq(t, info.Mode().Perm(), os.FileMode(0o640))
				}
				entries, _ := os.ReadDir(dir)
				eq(t, len(entries), 1) // no temporary file left
			},
		},
		{
			name: "symlink is written through",
			setup: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "dotfiles", "config")
				must(t, os.MkdirAll(filepath.Dir(target), 0o700))
				must(t, os.WriteFile(target, []byte("old"), 0o600))
				link := filepath.Join(dir, "config")
				if err := os.Symlink(target, link); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
				return link
			},
			check: func(t *testing.T, dir, path string) {
				info, err := os.Lstat(path)
				must(t, err)
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("the link was replaced")
				}
				eq(t, readString(t, filepath.Join(dir, "dotfiles", "config")), "a = 1\n")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)
			must(t, WriteFile(path, []byte("a = 1\n"), 0o600))
			tt.check(t, dir, path)
		})
	}
}

func TestSetFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	must(t, os.WriteFile(path, []byte("# keep\na = 1\n"), 0o600))
	must(t, SetFile(path, map[string]*string{"a": ptr("2"), "b": ptr("3")}, 0o600))
	eq(t, readString(t, path), "# keep\na = 2\nb = 3\n")
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name  string
		setup func(t *testing.T) string
		opts  Options
		want  string
		err   string
	}{
		{name: "regular", setup: func(t *testing.T) string { return writeTemp(t, dir, "ok", "a = 1\n") }, want: "a = 1\n"},
		{name: "missing", setup: func(t *testing.T) string { return filepath.Join(dir, "none") }, err: "none"},
		{name: "directory", setup: func(t *testing.T) string { return dir }, err: "not a regular file"},
		{
			name:  "too large",
			setup: func(t *testing.T) string { return writeTemp(t, dir, "big", "0123456789") },
			opts:  Options{MaxBytes: 4},
			err:   "file too large",
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
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
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
	tests := []struct{ name, xdg string }{
		{name: "absolute XDG", xdg: filepath.Join(t.TempDir(), "cfg")},
		{name: "relative XDG is ignored", xdg: "relative"},
		{name: "no XDG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			got, err := DefaultPath("app", "config")
			must(t, err)
			var base string
			if filepath.IsAbs(tt.xdg) {
				base = tt.xdg
			} else if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
				home, _ := os.UserHomeDir()
				base = filepath.Join(home, ".config")
			} else {
				base, _ = os.UserConfigDir()
			}
			eq(t, got, filepath.Join(base, "app", "config"))
		})
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return string(data)
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	must(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
