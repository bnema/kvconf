package kvconf

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Set returns data with each key of values assigned, keeping every other
// line, comment and the spacing of edited lines. An existing assignment (the
// last one) is replaced; otherwise a commented-out `# key = ...` is enabled;
// otherwise the key goes after the last key sharing its longest dotted
// parent, or at the end. A nil value removes every assignment of the key.
// Keys are applied in sorted order, so the output is deterministic. A key
// that cannot be read back as written (empty, with '=' or a line break,
// starting with '#') is an error and nothing is changed.
func Set(data []byte, values map[string]*string) ([]byte, error) {
	keys := make([]string, 0, len(values))
	for k := range values {
		if !validKey(k) {
			return nil, fmt.Errorf("kvconf: invalid key %q", k)
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if v := values[k]; v == nil {
			data = removeKey(data, k)
		} else {
			data = setKey(data, k, *v)
		}
	}
	return data, nil
}

// lineKey returns the key of an assignment line, or "" for anything else.
func lineKey(line string) string {
	body := strings.TrimSpace(line)
	if body == "" || body[0] == '#' {
		return ""
	}
	k, _, ok := strings.Cut(body, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(k)
}

func removeKey(data []byte, key string) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	out := lines[:0]
	for _, l := range lines {
		if lineKey(l) != key {
			out = append(out, l)
		}
	}
	return []byte(strings.Join(out, ""))
}

func setKey(data []byte, key, value string) []byte {
	value = Quote(value)
	lines := strings.SplitAfter(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if lineKey(lines[i]) == key {
			prefix, rest, _ := strings.Cut(lines[i], "=")
			lines[i] = replaceValue(prefix+"=", rest, value)
			return []byte(strings.Join(lines, ""))
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		indent, body, ok := strings.Cut(lines[i], "#")
		if !ok || strings.TrimSpace(indent) != "" || lineKey(body) != key {
			continue
		}
		prefix, rest, _ := strings.Cut(strings.TrimLeft(body, " \t"), "=")
		lines[i] = replaceValue(indent+prefix+"=", rest, value)
		return []byte(strings.Join(lines, ""))
	}
	// output.DP-2.scale belongs beside output.DP-2.*, not merely output.*.
	for parent := key; ; {
		dot := strings.LastIndexByte(parent, '.')
		if dot < 0 {
			break
		}
		parent = parent[:dot]
		index := -1
		for i, l := range lines {
			if k := lineKey(l); k == parent || strings.HasPrefix(k, parent+".") {
				index = i
			}
		}
		if index < 0 {
			continue
		}
		eol := "\n"
		if strings.HasSuffix(lines[index], "\r\n") {
			eol = "\r\n"
		}
		if !strings.HasSuffix(lines[index], "\n") {
			lines[index] += eol
		}
		lines = slices.Insert(lines, index+1, key+" = "+value+eol)
		return []byte(strings.Join(lines, ""))
	}
	var b bytes.Buffer
	b.Grow(len(data) + len(key) + len(value) + 5)
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(key)
	b.WriteString(" = ")
	b.WriteString(value)
	b.WriteByte('\n')
	return b.Bytes()
}

// replaceValue keeps indentation, spacing and a trailing comment.
func replaceValue(prefix, rest, value string) string {
	plain := strings.TrimRight(rest, "\r\n")
	eol := rest[len(plain):]
	old := stripComment(strings.TrimSpace(plain), CommentSpaced)
	start := strings.Index(plain, old)
	if old == "" {
		start = len(plain) - len(strings.TrimLeft(plain, " \t"))
		if start == 0 {
			value = " " + value
		}
	}
	return prefix + plain[:start] + value + plain[start+len(old):] + eol
}

// WriteFile replaces path with data atomically: readers and watchers never
// see a half-written file. A symlinked path is written through to its target
// so dotfile links survive. A new file gets perm and its directory is created
// (0700); an existing file keeps its permissions.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// Replacing a dangling link would lose it: report it instead.
		if path, err = filepath.EvalSymlinks(path); err != nil {
			return err
		}
	}
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	} else if isNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
	} else {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if err := writeSync(tmp, data, perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func writeSync(f *os.File, data []byte, perm os.FileMode) error {
	// Restrict before writing: the content may be secret.
	if err := f.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// SetFile applies Set to the file at path and writes it back atomically. A
// missing file is created with perm.
func SetFile(path string, values map[string]*string, perm os.FileMode) error {
	data, err := ReadFile(path, Options{})
	if err != nil && !isNotExist(err) {
		return err
	}
	if data, err = Set(data, values); err != nil {
		return err
	}
	return WriteFile(path, data, perm)
}
