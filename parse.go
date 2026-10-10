// Package kvconf reads, writes and live-reloads flat `key = value` config
// files with the standard library only.
//
//	# a comment
//	lock.after   = 5m
//	border.color = #ff8800          # a value may start with #
//	title        = "quoted # kept"  # quotes are removed
//	bind.cmd+return = spawn foot
//
// [Parse] turns a file into entries, [Decode] maps entries onto a struct
// with `kv` tags, [Watch] reports file changes with the native OS API
// (inotify, kqueue, ReadDirectoryChangesW, or polling), and [Live] ties them
// together into a config that reloads itself.
package kvconf

import (
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Entry is one `key = value` assignment.
type Entry struct {
	Key   string
	Value string // trimmed, comment removed, unquoted unless Options.RawValues
	Line  int    // 1-based
}

// Warning is a problem found in a file. It never contains a value, so it is
// safe to log even when the file holds secrets. In strict mode the first
// warning is returned as the error.
type Warning struct {
	Line int    // 0 when the problem is not tied to a line
	Key  string // empty when there is no key
	Msg  string
}

func (w Warning) Error() string {
	var b strings.Builder
	if w.Line > 0 {
		b.WriteString("line ")
		b.WriteString(strconv.Itoa(w.Line))
		b.WriteString(": ")
	}
	if w.Key != "" {
		b.WriteString(w.Key)
		b.WriteString(": ")
	}
	b.WriteString(w.Msg)
	return b.String()
}

// CommentStyle selects which '#' start a comment. A line whose first
// non-blank character is '#' is always a comment, and a '#' inside a quoted
// value never starts one.
type CommentStyle uint8

const (
	// CommentSpaced: a '#' preceded by a space or a tab ends the value, so
	// a value may start with '#' (colors) or contain one (a#b).
	CommentSpaced CommentStyle = iota
	// CommentLine: only whole-line comments; a value may contain any '#'.
	CommentLine
	// CommentAny: any '#' ends the value.
	CommentAny
)

// Default limits, used when the matching Options field is 0.
const (
	DefaultMaxBytes = 1 << 20
	DefaultMaxLine  = 64 << 10
)

// Options controls parsing and decoding. The zero value is lenient: a
// problem becomes a warning and the rest of the file still applies.
type Options struct {
	// Strict rejects the whole file on the first problem.
	Strict bool
	// Comments selects which '#' start a comment.
	Comments CommentStyle
	// AllowDuplicates lets a repeated key override the earlier one
	// silently. Otherwise the last one still wins, with a warning.
	AllowDuplicates bool
	// AllowUnknown ignores keys the target struct does not declare.
	AllowUnknown bool
	// RawValues keeps the quotes of "..." and '...' values.
	RawValues bool
	// RequireFile makes a missing file an error for Load and Live. By
	// default a missing file gives the defaults.
	RequireFile bool
	// MaxBytes bounds the file size: 0 means DefaultMaxBytes, a negative
	// value means no limit. A larger file is an error, even when lenient.
	MaxBytes int64
	// MaxLine bounds the length of a line: 0 means DefaultMaxLine, a
	// negative value means no limit.
	MaxLine int
}

func (o *Options) maxBytes() int64 {
	if o.MaxBytes == 0 {
		return DefaultMaxBytes
	}
	return o.MaxBytes
}

func (o *Options) maxLine() int {
	if o.MaxLine == 0 {
		return DefaultMaxLine
	}
	return o.MaxLine
}

var (
	// ErrTooLarge is returned when the input exceeds Options.MaxBytes.
	ErrTooLarge = errors.New("kvconf: file too large")
	// ErrNotUTF8 is returned when the input is not valid UTF-8.
	ErrNotUTF8 = errors.New("kvconf: file is not valid UTF-8")
)

// problems collects warnings, or stops at the first one in strict mode.
type problems struct {
	strict bool
	list   []Warning
}

// add records a warning and reports whether parsing may go on.
func (p *problems) add(line int, key, msg string) bool {
	p.list = append(p.list, Warning{Line: line, Key: key, Msg: msg})
	return !p.strict
}

func (p *problems) err() error {
	if p.strict && len(p.list) > 0 {
		return p.list[0]
	}
	return nil
}

// Parse reads r and returns its entries in file order: a repeated key
// appears once per assignment. A line that is not `key = value` is skipped
// with a warning, or fails the parse in strict mode.
func Parse(r io.Reader, opts Options) ([]Entry, []Warning, error) {
	data, err := readLimited(r, opts.maxBytes())
	if err != nil {
		return nil, nil, err
	}
	return ParseBytes(data, opts)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

// ParseBytes is Parse for data already in memory. The entries do not keep
// a reference to data.
func ParseBytes(data []byte, opts Options) ([]Entry, []Warning, error) {
	if m := opts.maxBytes(); m >= 0 && int64(len(data)) > m {
		return nil, nil, ErrTooLarge
	}
	if !utf8.Valid(data) {
		return nil, nil, ErrNotUTF8
	}
	// One copy: every key and value is a substring of it.
	s := strings.TrimPrefix(string(data), "\ufeff")
	entries := make([]Entry, 0, strings.Count(s, "="))
	var seen map[string]int
	if !opts.AllowDuplicates {
		seen = make(map[string]int, cap(entries))
	}
	p := problems{strict: opts.Strict}
	maxLine := opts.maxLine()
	for n := 1; s != ""; n++ {
		raw := s
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			raw, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		if maxLine >= 0 && len(raw) > maxLine {
			if !p.add(n, "", "line longer than "+strconv.Itoa(maxLine)+" bytes") {
				break
			}
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			if !p.add(n, "", "expected key = value") {
				break
			}
			continue
		}
		// A key may contain spaces: output names such as "LG Electronics 27GR95UM".
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			if !p.add(n, "", "missing key") {
				break
			}
			continue
		}
		value := stripComment(strings.TrimSpace(line[eq+1:]), opts.Comments)
		if !opts.RawValues {
			value = unquote(value)
		}
		if seen != nil {
			if prev, dup := seen[key]; dup && !p.add(n, key, "overrides line "+strconv.Itoa(prev)) {
				break
			}
			seen[key] = n
		}
		entries = append(entries, Entry{Key: key, Value: value, Line: n})
	}
	if err := p.err(); err != nil {
		return nil, p.list, err
	}
	return entries, p.list, nil
}

// stripComment removes a trailing comment from a trimmed value, ignoring a
// '#' inside quotes.
func stripComment(v string, style CommentStyle) string {
	if style == CommentLine || strings.IndexByte(v, '#') < 0 {
		return v
	}
	var quote byte
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case (c == '"' || c == '\'') && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t'):
			// Only a quote that starts a word opens a string: it's stays text.
			quote = c
		case c == '#' && (style == CommentAny || i > 0 && (v[i-1] == ' ' || v[i-1] == '\t')):
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// unquote removes the quotes of a value that is exactly one quoted string:
// "..." with Go escapes, or '...' taken literally. Anything else, such as a
// command line with quoted arguments, is kept as written.
func unquote(v string) string {
	if len(v) < 2 || v[0] != v[len(v)-1] {
		return v
	}
	switch v[0] {
	case '"':
		inner := v[1 : len(v)-1]
		if strings.IndexByte(inner, '\\') < 0 && strings.IndexByte(inner, '"') < 0 {
			return inner // the common case, without allocating
		}
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
	case '\'':
		if inner := v[1 : len(v)-1]; strings.IndexByte(inner, '\'') < 0 {
			return inner
		}
	}
	return v
}

// Values returns the effective value of each key: the last one wins.
func Values(entries []Entry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Key] = e.Value
	}
	return m
}

// Changed lists, sorted, the keys added, removed or changed between two
// Values maps.
func Changed(old, cur map[string]string) []string {
	var keys []string
	for k, v := range cur {
		if o, ok := old[k]; !ok || o != v {
			keys = append(keys, k)
		}
	}
	for k := range old {
		if _, ok := cur[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// sameEntries reports whether two files assign the same values in the same
// order, whatever their comments and spacing.
func sameEntries(a, b []Entry) bool {
	return slices.EqualFunc(a, b, func(x, y Entry) bool { return x.Key == y.Key && x.Value == y.Value })
}
