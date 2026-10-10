package kvconf

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		opts     Options
		want     []Entry
		warnings []string
		err      string
	}{
		{name: "empty", in: "", want: []Entry{}},
		{
			name: "basic",
			in:   "# comment\n\n  a = 1\nb=two words  \n",
			want: []Entry{{"a", "1", 3}, {"b", "two words", 4}},
		},
		{
			name: "CRLF and BOM",
			in:   "\ufeffa = 1\r\nb = 2\r\n",
			want: []Entry{{"a", "1", 1}, {"b", "2", 2}},
		},
		{
			name: "empty value",
			in:   "a =\n",
			want: []Entry{{"a", "", 1}},
		},
		{
			name: "value with equals",
			in:   "cmd = env A=1 foo\n",
			want: []Entry{{"cmd", "env A=1 foo", 1}},
		},
		{
			name: "spaced comments keep colors",
			in:   "color = #ff8800 # orange\nx = a#b\ny = a #b\n",
			want: []Entry{{"color", "#ff8800", 1}, {"x", "a#b", 2}, {"y", "a", 3}},
		},
		{
			name: "tab before comment",
			in:   "a = 1\t# note\n",
			want: []Entry{{"a", "1", 1}},
		},
		{
			name: "line comments only",
			in:   "a = 1 # kept\n# skipped = 1\n",
			opts: Options{Comments: CommentLine},
			want: []Entry{{"a", "1 # kept", 1}},
		},
		{
			name: "any hash is a comment",
			in:   "a = x#y\n",
			opts: Options{Comments: CommentAny},
			want: []Entry{{"a", "x", 1}},
		},
		{
			name: "quotes",
			in: `a = "x # y"   # c
b = 'it''s'
c = 'lit\n'
d = "tab\t"
e = "a" "b"
f = "unterminated
g = spawn "foo bar"
`,
			want: []Entry{
				{"a", "x # y", 1},
				{"b", "'it''s'", 2},
				{"c", `lit\n`, 3},
				{"d", "tab\t", 4},
				{"e", `"a" "b"`, 5},
				{"f", `"unterminated`, 6},
				{"g", `spawn "foo bar"`, 7},
			},
		},
		{
			name: "apostrophe inside a word keeps the comment",
			in:   "title = it's fine   # a note\nb = say \"x # y\" # c\n",
			want: []Entry{{"title", "it's fine", 1}, {"b", `say "x # y"`, 2}},
		},
		{
			name: "raw values",
			in:   `a = "x"` + "\n",
			opts: Options{RawValues: true},
			want: []Entry{{"a", `"x"`, 1}},
		},
		{
			name:     "duplicate warns, both kept",
			in:       "a = 1\na = 2\n",
			want:     []Entry{{"a", "1", 1}, {"a", "2", 2}},
			warnings: []string{"line 2: a: overrides line 1"},
		},
		{
			name: "duplicates allowed",
			in:   "a = 1\na = 2\n",
			opts: Options{AllowDuplicates: true},
			want: []Entry{{"a", "1", 1}, {"a", "2", 2}},
		},
		{
			name:     "bad lines are skipped",
			in:       "nope\n= 1\nok = 1\n",
			want:     []Entry{{"ok", "1", 3}},
			warnings: []string{"line 1: expected key = value", "line 2: missing key"},
		},
		{
			name: "key with spaces",
			in:   "layout.LG Electronics 27GR95UM.overflow  = scroll\n",
			want: []Entry{{"layout.LG Electronics 27GR95UM.overflow", "scroll", 1}},
		},
		{
			name:     "strict stops at first problem",
			in:       "ok = 1\nnope\nbad\n",
			opts:     Options{Strict: true},
			warnings: []string{"line 2: expected key = value"},
			err:      "line 2: expected key = value",
		},
		{
			name:     "strict duplicate",
			in:       "a = 1\na = 2\n",
			opts:     Options{Strict: true},
			err:      "line 2: a: overrides line 1",
			warnings: []string{"line 2: a: overrides line 1"},
		},
		{
			name:     "long line",
			in:       "a = " + strings.Repeat("x", 20) + "\nb = 1\n",
			opts:     Options{MaxLine: 10},
			want:     []Entry{{"b", "1", 2}},
			warnings: []string{"line 1: line longer than 10 bytes"},
		},
		{
			name: "too large",
			in:   "a = 1234567890\n",
			opts: Options{MaxBytes: 8},
			err:  ErrTooLarge.Error(),
		},
		{
			name: "no size limit",
			in:   "a = 1234567890\n",
			opts: Options{MaxBytes: -1, MaxLine: -1},
			want: []Entry{{"a", "1234567890", 1}},
		},
		{
			name: "invalid UTF-8",
			in:   "a = \xff\n",
			err:  ErrNotUTF8.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings, err := Parse(strings.NewReader(tt.in), tt.opts)
			if tt.err != "" {
				if err == nil || err.Error() != tt.err {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			} else if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("entries = %#v\nwant      %#v", got, tt.want)
			}
			if msgs := warningStrings(warnings); !reflect.DeepEqual(msgs, tt.warnings) {
				t.Errorf("warnings = %q, want %q", msgs, tt.warnings)
			}
		})
	}
}

func warningStrings(ws []Warning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Error())
	}
	return out
}

func TestStrictErrorIsWarning(t *testing.T) {
	_, _, err := Parse(strings.NewReader("bad\n"), Options{Strict: true})
	var w Warning
	if !errors.As(err, &w) || w.Line != 1 {
		t.Fatalf("err = %#v, want a Warning on line 1", err)
	}
}

func TestChanged(t *testing.T) {
	tests := []struct {
		name     string
		old, cur map[string]string
		want     []string
	}{
		{name: "same", old: map[string]string{"a": "1"}, cur: map[string]string{"a": "1"}},
		{name: "added", old: map[string]string{}, cur: map[string]string{"b": "1", "a": "1"}, want: []string{"a", "b"}},
		{name: "removed", old: map[string]string{"a": "1"}, cur: nil, want: []string{"a"}},
		{name: "changed", old: map[string]string{"a": "1", "b": "1"}, cur: map[string]string{"a": "2", "b": "1"}, want: []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Changed(tt.old, tt.cur); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Changed = %q, want %q", got, tt.want)
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"a = 1\n", "a = \"x # y\" # c\n", "\ufeffa=b\r\n", "x\n=\n'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		entries, _, err := Parse(strings.NewReader(in), Options{})
		if err != nil {
			return
		}
		// Whatever was read can be written and read back the same.
		var data []byte
		for _, e := range entries {
			data = append(data, e.Key+" = "+Quote(e.Value)+"\n"...)
		}
		again, _, err := ParseBytes(data, Options{AllowDuplicates: true})
		if err != nil {
			t.Fatalf("re-parse: %v\n%s", err, data)
		}
		if !sameEntries(entries, again) {
			t.Fatalf("round trip:\n%#v\n%#v", entries, again)
		}
	})
}
