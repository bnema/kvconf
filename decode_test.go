package kvconf

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type mode string

type output struct {
	Scale   float64 `kv:"scale" min:"0.25" max:"4"`
	HDR     bool    `kv:"hdr"`
	Primary bool    `kv:"primary"`
	Bright  int     `kv:"sdr-brightness" min:"80" max:"1000"`
}

func (o *output) SetDefaults() { o.Scale, o.Bright = 1, 203 }

type testConfig struct {
	Name    string        `kv:"name,nonempty"`
	Enabled bool          `kv:"enabled"`
	Count   int           `kv:"count" min:"1" max:"10"`
	Size    uint16        `kv:"size"`
	Ratio   float32       `kv:"ratio"`
	After   time.Duration `kv:"lock.after,off" max:"1h"`
	Mode    mode          `kv:"mode" oneof:"auto|dark|light"`
	Tags    []string      `kv:"tags"`
	Ports   []int         `kv:"ports" sep:"|" min:"1" max:"65535"`
	Dir     Path          `kv:"dir"`
	Fade    struct {
		After    time.Duration `kv:"after"`
		Duration time.Duration `kv:"duration"`
	} `kv:"fade"`
	Inline struct {
		Flag bool `kv:"flag"`
	} `kv:",inline"`
	Binds   map[string]string `kv:"bind.*"`
	Outputs map[string]output `kv:"output.*"`
	Codes   map[string]int    `kv:"code.*" min:"0"`
	Ignored string            `kv:"-"`
	Rest    []Entry           `kv:"*"`
	Plain   string            // untagged: ignored
}

func defaults() testConfig {
	c := testConfig{Name: "x", Count: 5, Mode: "auto", After: 5 * time.Minute}
	return c
}

func TestDecode(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		name     string
		in       string
		opts     Options
		check    func(t *testing.T, c *testConfig)
		warnings []string
		err      string
	}{
		{
			name: "scalars",
			in: `name = vev
enabled = on
count = 7
size = 512
ratio = 0.5
lock.after = 10m
mode = dark
flag = yes
`,
			check: func(t *testing.T, c *testConfig) {
				want := defaults()
				want.Name, want.Enabled, want.Count, want.Size, want.Ratio = "vev", true, 7, 512, 0.5
				want.After, want.Mode, want.Inline.Flag = 10*time.Minute, "dark", true
				want.Rest = nil
				if !reflect.DeepEqual(*c, want) {
					t.Errorf("got %+v\nwant %+v", *c, want)
				}
			},
		},
		{
			name:  "absent keys keep defaults",
			in:    "",
			check: func(t *testing.T, c *testConfig) { eq(t, c.Count, 5); eq(t, c.After, 5*time.Minute) },
		},
		{
			name:  "off is zero",
			in:    "lock.after = off\n",
			check: func(t *testing.T, c *testConfig) { eq(t, c.After, time.Duration(0)) },
		},
		{
			name: "nested struct",
			in:   "fade.after = 1s\nfade.duration = 250ms\n",
			check: func(t *testing.T, c *testConfig) {
				eq(t, c.Fade.After, time.Second)
				eq(t, c.Fade.Duration, 250*time.Millisecond)
			},
		},
		{
			name: "lists",
			in:   "tags = a, b ,,c\nports = 80|443\n",
			check: func(t *testing.T, c *testConfig) {
				eq(t, c.Tags, []string{"a", "b", "c"})
				eq(t, c.Ports, []int{80, 443})
			},
		},
		{
			name:  "empty list",
			in:    "tags =\n",
			check: func(t *testing.T, c *testConfig) { eq(t, c.Tags, []string{}) },
		},
		{
			name:  "path",
			in:    "dir = ~/shots/../pics\n",
			check: func(t *testing.T, c *testConfig) { eq(t, c.Dir, Path(filepath.Join(home, "pics"))) },
		},
		{
			name: "map of strings",
			in:   "bind.cmd+q = close\nbind.cmd+return = spawn foot\n",
			check: func(t *testing.T, c *testConfig) {
				eq(t, c.Binds, map[string]string{"cmd+q": "close", "cmd+return": "spawn foot"})
			},
		},
		{
			name: "map of structs with dotted names and defaults",
			in:   "output.DP-1.scale = 1.5\noutput.eDP.1.hdr = on\noutput.DP-1.primary = on\n",
			check: func(t *testing.T, c *testConfig) {
				eq(t, c.Outputs, map[string]output{
					"DP-1":  {Scale: 1.5, Primary: true, Bright: 203},
					"eDP.1": {Scale: 1, HDR: true, Bright: 203},
				})
			},
		},
		{
			name:     "map of structs rejects bad value without creating the entry",
			in:       "output.DP-1.scale = 9\n",
			check:    func(t *testing.T, c *testConfig) { eq(t, len(c.Outputs), 0) },
			warnings: []string{"line 1: output.DP-1.scale: must be at most 4"},
		},
		{
			name:  "map of ints",
			in:    "code.foo = 3\n",
			check: func(t *testing.T, c *testConfig) { eq(t, c.Codes, map[string]int{"foo": 3}) },
		},
		{
			name: "catch-all keeps unknown keys in order",
			in:   "zz = 1\naa = 2\noutput.DP-1.nope = 3\n",
			check: func(t *testing.T, c *testConfig) {
				eq(t, c.Rest, []Entry{{"zz", "1", 1}, {"aa", "2", 2}, {"output.DP-1.nope", "3", 3}})
			},
		},
		{
			name: "invalid values keep the previous value",
			in: `count = 0
count = 11
count = x
enabled = maybe
mode = blue
lock.after = 2h
lock.after = soon
name =
size = -1
ratio = big
ports = 80|0
dir = rel/path
`,
			check: func(t *testing.T, c *testConfig) {
				want := defaults()
				if !reflect.DeepEqual(*c, want) {
					t.Errorf("got %+v\nwant %+v", *c, want)
				}
			},
			warnings: []string{
				"line 2: count: overrides line 1",
				"line 3: count: overrides line 2",
				"line 7: lock.after: overrides line 6",
				"line 1: count: must be at least 1",
				"line 2: count: must be at most 10",
				"line 3: count: must be an integer",
				"line 4: enabled: must be on or off",
				"line 5: mode: must be auto, dark or light",
				"line 6: lock.after: must be at most 1h",
				"line 7: lock.after: must be a duration such as 500ms, 5m or 1h30m",
				"line 8: name: must not be empty",
				"line 9: size: must be a non-negative integer",
				"line 10: ratio: must be a number",
				"line 11: ports: item 2: must be at least 1",
				"line 12: dir: must be an absolute path or start with ~/",
			},
		},
		{
			name:  "last valid value wins",
			in:    "count = 2\ncount = 3\n",
			opts:  Options{AllowDuplicates: true},
			check: func(t *testing.T, c *testConfig) { eq(t, c.Count, 3) },
		},
		{
			name:     "strict fails on first bad value",
			in:       "count = 2\ncount = 99\nmode = x\n",
			opts:     Options{Strict: true, AllowDuplicates: true},
			warnings: []string{"line 2: count: must be at most 10"},
			err:      "line 2: count: must be at most 10",
		},
		{
			name:     "messages never contain the value",
			in:       "count = hunter2\n",
			warnings: []string{"line 1: count: must be an integer"},
			check:    func(t *testing.T, c *testConfig) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaults()
			warnings, err := Unmarshal([]byte(tt.in), &c, tt.opts)
			if tt.err != "" {
				if err == nil || err.Error() != tt.err {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got := warningStrings(warnings); !reflect.DeepEqual(got, tt.warnings) {
				t.Errorf("warnings =\n%q\nwant\n%q", got, tt.warnings)
			}
			if tt.check != nil {
				tt.check(t, &c)
			}
		})
	}
}

type plain struct {
	A int `kv:"a"`
}

type validated struct {
	Min int `kv:"min"`
	Max int `kv:"max"`
}

func (v *validated) Validate() error {
	if v.Min > v.Max {
		return errors.New("min must not exceed max")
	}
	return nil
}

func TestDecodeOptionsAndValidate(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		target   any
		opts     Options
		warnings []string
		err      string
	}{
		{name: "unknown key warns", in: "b = 1\n", target: &plain{}, warnings: []string{"line 1: b: unknown key"}},
		{name: "unknown key allowed", in: "b = 1\n", target: &plain{}, opts: Options{AllowUnknown: true}},
		{name: "unknown key strict", in: "b = 1\n", target: &plain{}, opts: Options{Strict: true},
			warnings: []string{"line 1: b: unknown key"}, err: "line 1: b: unknown key"},
		{name: "validate passes", in: "min = 1\nmax = 2\n", target: &validated{}},
		{name: "validate fails", in: "min = 3\nmax = 2\n", target: &validated{}, err: "min must not exceed max"},
		{name: "not a pointer", target: plain{}, err: errTarget.Error()},
		{name: "nil pointer", target: (*plain)(nil), err: errTarget.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := Unmarshal([]byte(tt.in), tt.target, tt.opts)
			if gotErr := errString(err); gotErr != tt.err {
				t.Fatalf("err = %q, want %q", gotErr, tt.err)
			}
			if got := warningStrings(warnings); !reflect.DeepEqual(got, tt.warnings) {
				t.Errorf("warnings = %q, want %q", got, tt.warnings)
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestSchemaErrors(t *testing.T) {
	tests := []struct {
		name   string
		target any
		want   string
	}{
		{"unknown option", &struct {
			A int `kv:"a,bogus"`
		}{}, `unknown tag option "bogus"`},
		{"unsupported type", &struct {
			A chan int `kv:"a"`
		}{}, "unsupported type chan int"},
		{"duplicate key", &struct {
			A int `kv:"a"`
			B int `kv:"a"`
		}{}, `key "a" declared twice`},
		{"map without string keys", &struct {
			A map[int]int `kv:"a.*"`
		}{}, "needs a map with string keys"},
		{"bad catch-all", &struct {
			A int `kv:"*"`
		}{}, `needs []kvconf.Entry or map[string]string`},
		{"unnamed struct", &struct {
			A struct{} `kv:""`
		}{}, "needs a name or inline"},
		{"bad bound", &struct {
			A int `kv:"a" min:"x"`
		}{}, `bad min "x"`},
		{"nested list", &struct {
			A [][]int `kv:"a"`
		}{}, "nested lists are not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Unmarshal(nil, tt.target, Options{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// leaky mimics a third-party type whose errors quote the input.
type leaky string

func (l *leaky) UnmarshalText(b []byte) error { return fmt.Errorf("bad input %q", b) }

// explained returns a ValueError, which is shown.
type explained string

func (e *explained) UnmarshalText([]byte) error { return ValueError("must be a colour") }

func TestDecodeNeverLeaksValues(t *testing.T) {
	type cfg struct {
		L     leaky     `kv:"l"`
		E     explained `kv:"e"`
		Ratio float64   `kv:"ratio" min:"0" max:"1"`
		Any   float64   `kv:"any"`
	}
	tests := []struct{ in, want string }{
		{"l = hunter2", "line 1: l: invalid kvconf.leaky"},
		{"e = hunter2", "line 1: e: must be a colour"},
		{"ratio = NaN", "line 1: ratio: must be a finite number"},
		{"ratio = +Inf", "line 1: ratio: must be a finite number"},
		{"any = NaN", "line 1: any: must be a finite number"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var c cfg
			w, err := Unmarshal([]byte(tt.in+"\n"), &c, Options{})
			must(t, err)
			eq(t, warningStrings(w), []string{tt.want})
			if strings.Contains(tt.want, "hunter2") {
				t.Fatal("value leaked")
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		in   string
		want bool
		err  bool
	}{
		{"on", true, false}, {"ON", true, false}, {"true", true, false}, {"Yes", true, false}, {"1", true, false},
		{"off", false, false}, {"False", false, false}, {"no", false, false}, {"0", false, false},
		{"", false, true}, {"2", false, true}, {"enable", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseBool(tt.in)
			if (err != nil) != tt.err || got != tt.want {
				t.Fatalf("ParseBool(%q) = %v, %v", tt.in, got, err)
			}
		})
	}
}

func eq[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}
