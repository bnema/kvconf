package kvconf

import (
	"encoding"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"
)

// Quote returns v as it must be written so Parse reads it back unchanged:
// as is when possible, else as a Go "..." string.
func Quote(v string) string {
	if needsQuote(v) {
		return strconv.Quote(v)
	}
	return v
}

func needsQuote(v string) bool {
	if v == "" {
		return true
	}
	first, last := v[0], v[len(v)-1]
	if first >= utf8.RuneSelf || last >= utf8.RuneSelf {
		if v != strings.TrimSpace(v) {
			return true
		}
	} else if asciiSpace(first) || asciiSpace(last) {
		return true
	}
	if first == '"' || first == '\'' {
		return true // it would be unquoted on reading
	}
	hash := false
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\n', '\r':
			return true
		case '#':
			hash = true
		}
	}
	// A '#' after a space ends the value unless it is inside a quoted word,
	// so quote any value where Parse would cut it.
	return hash && stripComment(v, CommentSpaced) != v
}

// asciiSpace reports the ASCII bytes that strings.TrimSpace removes.
func asciiSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// validKey reports whether key can be written and read back as one key.
// A leading U+FEFF would be read as the byte order mark of a first line.
func validKey(key string) bool {
	return key != "" && key[0] != '#' && key == strings.TrimSpace(key) &&
		!strings.HasPrefix(key, "\ufeff") && !strings.ContainsAny(key, "=\n\r")
}

func appendQuoted(b []byte, v string) []byte {
	if needsQuote(v) {
		return strconv.AppendQuote(b, v)
	}
	return append(b, v...)
}

// Marshal writes the tagged fields of v (a struct or a pointer to one) as
// `key = value` lines in declaration order; map entries are sorted by name.
func Marshal(v any) ([]byte, error) {
	rv := reflect.Indirect(reflect.ValueOf(v))
	if rv.Kind() != reflect.Struct {
		return nil, errors.New("kvconf: Marshal needs a struct")
	}
	s, err := schemaOf(rv.Type())
	if err != nil {
		return nil, err
	}
	// Size the output once: about 24 bytes of value per line.
	size, maxLen := 0, 0
	for _, f := range s.order {
		if f.elem == nil && f.val == nil {
			size += len(f.key) + 24
			continue
		}
		n := rv.FieldByIndex(f.index).Len()
		maxLen = max(maxLen, n)
		if f.elem == nil {
			size += n * (2*len(f.key) + 24)
			continue
		}
		for _, sub := range f.elem.order {
			size += n * (len(f.key) + len(sub.key) + 32)
		}
	}
	m := marshaler{b: make([]byte, 0, size), tmp: make([]byte, 0, 64)}
	// Map names and their sort order, reused for every map.
	names := make([]string, 0, maxLen)
	order := make([]int, 0, maxLen)
	for _, f := range s.order {
		fv := rv.FieldByIndex(f.index)
		if f.elem == nil && f.val == nil {
			if err := m.line(f.key, "", "", fv, f); err != nil {
				return nil, err
			}
			continue
		}
		// Copy the map out once, into reused values, then sort by name.
		n := fv.Len()
		names, order = names[:0], order[:0]
		vals := reflect.MakeSlice(reflect.SliceOf(f.typ.Elem()), n, n)
		key := reflect.New(f.typ.Key()).Elem()
		for it, i := fv.MapRange(), 0; it.Next(); i++ {
			key.SetIterKey(it)
			names = append(names, key.String())
			order = append(order, i)
			vals.Index(i).SetIterValue(it)
		}
		slices.SortFunc(order, func(a, b int) int { return strings.Compare(names[a], names[b]) })
		for _, i := range order {
			name, ev := names[i], vals.Index(i)
			// Checked in name order, so the error is the same on every run.
			if strings.ContainsAny(name, "=\n\r") || name != strings.TrimSpace(name) {
				return nil, fmt.Errorf("kvconf: %s: invalid map name %q", f.key, name)
			}
			if f.elem == nil {
				if err := m.line(f.key, name, "", ev, f.val); err != nil {
					return nil, err
				}
				continue
			}
			for _, sub := range f.elem.order {
				if err := m.line(f.key, name, sub.key, ev.FieldByIndex(sub.index), sub); err != nil {
					return nil, err
				}
			}
		}
	}
	if s.catchAll != nil {
		fv := rv.FieldByIndex(s.catchAll.index)
		if es, ok := fv.Interface().([]Entry); ok {
			for _, e := range es {
				if err := m.raw(e.Key, e.Value); err != nil {
					return nil, err
				}
			}
		} else {
			kv := fv.Interface().(map[string]string)
			for _, k := range slices.Sorted(maps.Keys(kv)) {
				if err := m.raw(k, kv[k]); err != nil {
					return nil, err
				}
			}
		}
	}
	return m.b, nil
}

type marshaler struct {
	b   []byte
	tmp []byte // the value being formatted, reused
}

func (m *marshaler) raw(key, value string) error {
	if !validKey(key) {
		return fmt.Errorf("kvconf: invalid key %q", key)
	}
	m.b = append(m.b, key...)
	m.b = append(m.b, " = "...)
	m.b = appendQuoted(m.b, value)
	m.b = append(m.b, '\n')
	return nil
}

// line writes prefix+name[.sub] = value.
func (m *marshaler) line(prefix, name, sub string, v reflect.Value, f *field) error {
	var err error
	if m.tmp, err = appendValue(m.tmp[:0], v, f); err != nil {
		key := prefix + name
		if sub != "" {
			key += "." + sub
		}
		return fmt.Errorf("kvconf: %s: %w", key, err)
	}
	m.b = append(m.b, prefix...)
	m.b = append(m.b, name...)
	if sub != "" {
		m.b = append(m.b, '.')
		m.b = append(m.b, sub...)
	}
	m.b = append(m.b, " = "...)
	// tmp is only read here, before its next reuse.
	m.b = appendQuoted(m.b, unsafe.String(unsafe.SliceData(m.tmp), len(m.tmp)))
	m.b = append(m.b, '\n')
	return nil
}

func appendValue(b []byte, v reflect.Value, f *field) ([]byte, error) {
	if f.off && v.IsZero() {
		return append(b, "off"...), nil
	}
	if f.marshal {
		if !v.CanAddr() { // a map element: copy it to call a pointer method
			p := reflect.New(v.Type())
			p.Elem().Set(v)
			v = p.Elem()
		}
		var tm encoding.TextMarshaler
		if v.Type().Implements(marshalType) {
			tm = v.Interface().(encoding.TextMarshaler)
		} else {
			tm = v.Addr().Interface().(encoding.TextMarshaler)
		}
		text, err := tm.MarshalText()
		return append(b, text...), err
	}
	switch v.Kind() {
	case reflect.String:
		return append(b, v.String()...), nil
	case reflect.Bool:
		if v.Bool() {
			return append(b, "on"...), nil
		}
		return append(b, "off"...), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if f.duration {
			return append(b, time.Duration(v.Int()).String()...), nil
		}
		return strconv.AppendInt(b, v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.AppendUint(b, v.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.AppendFloat(b, v.Float(), 'f', -1, v.Type().Bits()), nil
	case reflect.Slice:
		for i := range v.Len() {
			if i > 0 {
				b = append(b, f.sep...)
			}
			var err error
			if b, err = appendValue(b, v.Index(i), f.item); err != nil {
				return b, err
			}
		}
		return b, nil
	}
	return b, fmt.Errorf("unsupported type %s", v.Type())
}
