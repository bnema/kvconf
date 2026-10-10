package kvconf

import (
	"encoding"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Struct tags understood by Decode and Marshal:
//
//	kv:"name"            the key; a nested struct prefixes its keys with "name."
//	kv:",inline"         a nested struct whose keys are not prefixed
//	kv:"name,off"        "off" sets the zero value (durations, numbers, strings)
//	kv:"name,nonempty"   an empty value is rejected
//	kv:"bind.*"          a map: bind.<name> = value fills map[name]. A map of
//	                     structs takes bind.<name>.<field>; names may hold dots
//	kv:"*"               a []Entry or map[string]string that collects unknown keys
//	kv:"-"               ignored, like untagged fields
//	min:"1" max:"100"    bounds of a number or duration ("1s", "10m")
//	oneof:"a|b|c"        the allowed values
//	sep:"|"              the separator of a list (default ",")
//
// A value decodes into a string, a bool (on/off, true/false, yes/no, 1/0), an
// integer, a float, a time.Duration, a slice of those, or any type whose
// pointer implements encoding.TextUnmarshaler, such as [Path]. Named types
// work too: type Mode string with oneof is an enum.

// ValueError is an error message that is safe to show in a Warning: it
// does not contain the value. Return one from UnmarshalText to explain a
// rejected value; any other error is reported as "invalid <type>".
type ValueError string

func (e ValueError) Error() string { return string(e) }

// Validator is implemented by a config that checks itself after decoding,
// for rules that span several keys. Its error always fails the load.
type Validator interface{ Validate() error }

// Defaulter is implemented by a config, or by the element of a map of
// structs, that fills in its defaults. Load calls it before decoding; a new
// map element calls it before its first key is set.
type Defaulter interface{ SetDefaults() }

var (
	durationType   = reflect.TypeFor[time.Duration]()
	unmarshalType  = reflect.TypeFor[encoding.TextUnmarshaler]()
	marshalType    = reflect.TypeFor[encoding.TextMarshaler]()
	entrySliceType = reflect.TypeFor[[]Entry]()
	stringMapType  = reflect.TypeFor[map[string]string]()
	defaulterType  = reflect.TypeFor[Defaulter]()
	errTarget      = errors.New("kvconf: target must be a non-nil pointer to a struct")
)

type field struct {
	key      string // full key; the prefix with its trailing dot for a map
	index    []int
	typ      reflect.Type
	text     bool // *typ implements encoding.TextUnmarshaler
	marshal  bool // typ or *typ implements encoding.TextMarshaler
	duration bool
	off      bool
	nonempty bool
	hasMin   bool
	hasMax   bool
	min, max float64
	minText  string
	maxText  string
	oneof    []string
	sep      string
	item     *field       // element of a slice
	val      *field       // element of a map of values
	elem     *schema      // element of a map of structs
	defaults bool         // map element implements Defaulter
	elems    reflect.Type // []element of a map of structs
	slot     int          // index of a map field in schema.prefixes
}

type schema struct {
	keys     map[string]*field
	order    []*field // declaration order, for Marshal
	prefixes []*field // maps, longest prefix first
	catchAll *field
}

var schemas sync.Map // reflect.Type -> *schema or error

func schemaOf(t reflect.Type) (*schema, error) {
	if s, ok := schemas.Load(t); ok {
		if err, bad := s.(error); bad {
			return nil, err
		}
		return s.(*schema), nil
	}
	s, err := buildSchema(t)
	if err != nil {
		schemas.Store(t, err)
		return nil, err
	}
	schemas.Store(t, s)
	return s, nil
}

func buildSchema(t reflect.Type) (*schema, error) {
	s := &schema{keys: map[string]*field{}}
	if err := s.add(t, "", nil); err != nil {
		return nil, err
	}
	slices.SortStableFunc(s.prefixes, func(a, b *field) int { return len(b.key) - len(a.key) })
	for i, f := range s.prefixes {
		f.slot = i
	}
	return s, nil
}

func isText(t reflect.Type) bool { return reflect.PointerTo(t).Implements(unmarshalType) }

func isLeaf(t reflect.Type) bool { return t.Kind() != reflect.Struct || isText(t) }

func (s *schema) add(t reflect.Type, prefix string, index []int) error {
	for i := range t.NumField() {
		sf := t.Field(i)
		tag, ok := sf.Tag.Lookup("kv")
		if !ok || tag == "-" || !sf.IsExported() {
			continue
		}
		where := t.String() + "." + sf.Name
		name, flags, _ := strings.Cut(tag, ",")
		idx := append(slices.Clip(index), i)
		inline := false
		var opts []string
		for flag := range strings.SplitSeq(flags, ",") {
			switch flag {
			case "":
			case "inline":
				inline = true
			case "off", "nonempty":
				opts = append(opts, flag)
			default:
				return fmt.Errorf("kvconf: %s: unknown tag option %q", where, flag)
			}
		}
		switch {
		case name == "*":
			if sf.Type != entrySliceType && sf.Type != stringMapType {
				return fmt.Errorf("kvconf: %s: kv:\"*\" needs []kvconf.Entry or map[string]string", where)
			}
			if s.catchAll != nil {
				return fmt.Errorf("kvconf: %s: a second kv:\"*\" field", where)
			}
			s.catchAll = &field{index: idx, typ: sf.Type}
		case strings.HasSuffix(name, ".*"):
			if sf.Type.Kind() != reflect.Map || sf.Type.Key().Kind() != reflect.String {
				return fmt.Errorf("kvconf: %s: %q needs a map with string keys", where, name)
			}
			et := sf.Type.Elem()
			f := &field{}
			if isLeaf(et) {
				val, err := newField(where, et, sf.Tag, opts)
				if err != nil {
					return err
				}
				f.val = val
			} else {
				elem, err := buildSchema(et)
				if err != nil {
					return err
				}
				if elem.catchAll != nil || len(elem.prefixes) > 0 {
					return fmt.Errorf("kvconf: %s: a map element cannot hold maps or kv:\"*\"", where)
				}
				f = &field{elem: elem, elems: reflect.SliceOf(et), defaults: reflect.PointerTo(et).Implements(defaulterType)}
			}
			f.key, f.index, f.typ = prefix+strings.TrimSuffix(name, "*"), idx, sf.Type
			s.prefixes = append(s.prefixes, f)
			s.order = append(s.order, f)
		case !isLeaf(sf.Type):
			p := prefix
			if !inline {
				if name == "" {
					return fmt.Errorf("kvconf: %s: a nested struct needs a name or inline", where)
				}
				p += name + "."
			}
			if err := s.add(sf.Type, p, idx); err != nil {
				return err
			}
		default:
			if name == "" {
				return fmt.Errorf("kvconf: %s: empty key", where)
			}
			f, err := newField(where, sf.Type, sf.Tag, opts)
			if err != nil {
				return err
			}
			f.key, f.index = prefix+name, idx
			if _, dup := s.keys[f.key]; dup {
				return fmt.Errorf("kvconf: %s: key %q declared twice", where, f.key)
			}
			s.keys[f.key] = f
			s.order = append(s.order, f)
		}
	}
	return nil
}

// newField checks that t can be decoded and precomputes how.
func newField(where string, t reflect.Type, tag reflect.StructTag, opts []string) (*field, error) {
	f := &field{typ: t, text: isText(t), duration: t == durationType, sep: tag.Get("sep"),
		marshal: t.Implements(marshalType) || reflect.PointerTo(t).Implements(marshalType)}
	f.off = slices.Contains(opts, "off")
	f.nonempty = slices.Contains(opts, "nonempty")
	if f.sep == "" {
		f.sep = ","
	}
	if o := tag.Get("oneof"); o != "" {
		f.oneof = strings.Split(o, "|")
	}
	kind := t.Kind()
	if !f.text {
		switch kind {
		case reflect.String, reflect.Bool, reflect.Float32, reflect.Float64,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		case reflect.Slice:
			item, err := newField(where, t.Elem(), tag, nil)
			if err != nil {
				return nil, err
			}
			if item.item != nil {
				return nil, fmt.Errorf("kvconf: %s: nested lists are not supported", where)
			}
			// The item reads oneof, min and max from the same tag and
			// checks each element against them.
			f.item, f.oneof = item, nil
			return f, nil
		default:
			return nil, fmt.Errorf("kvconf: %s: unsupported type %s", where, t)
		}
	}
	bound := func(s string) (float64, error) {
		if f.duration {
			d, err := time.ParseDuration(s)
			return float64(d), err
		}
		return strconv.ParseFloat(s, 64)
	}
	var err error
	if f.minText = tag.Get("min"); f.minText != "" {
		f.hasMin = true
		if f.min, err = bound(f.minText); err != nil {
			return nil, fmt.Errorf("kvconf: %s: bad min %q", where, f.minText)
		}
	}
	if f.maxText = tag.Get("max"); f.maxText != "" {
		f.hasMax = true
		if f.max, err = bound(f.maxText); err != nil {
			return nil, fmt.Errorf("kvconf: %s: bad max %q", where, f.maxText)
		}
	}
	return f, nil
}

// Unmarshal parses data and decodes it into v, a pointer to a struct. Fill v
// with its defaults first: keys absent from data keep their value.
func Unmarshal(data []byte, v any, opts Options) ([]Warning, error) {
	entries, warnings, err := ParseBytes(data, opts)
	if err != nil {
		return warnings, err
	}
	more, err := Decode(entries, v, opts)
	return append(warnings, more...), err
}

// Decode sets the fields of v, a pointer to a struct, from entries in order,
// so a repeated key ends with its last valid value. An invalid value is
// reported and leaves its field unchanged, or fails the decode in strict
// mode. An unknown key is reported unless Options.AllowUnknown is set or a
// kv:"*" field collects it. Validate runs last when v implements Validator.
func Decode(entries []Entry, v any, opts Options) ([]Warning, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, errTarget
	}
	s, err := schemaOf(rv.Elem().Type())
	if err != nil {
		return nil, err
	}
	st := decodeState{root: rv.Elem(), entries: entries}
	p := problems{strict: opts.Strict}
	for i := range entries {
		st.next = i
		e := &entries[i]
		msg, known := s.apply(&st, e)
		if !known {
			if opts.AllowUnknown {
				continue
			}
			msg = "unknown key"
		}
		if msg != "" && !p.add(e.Line, e.Key, msg) {
			return p.list, p.err()
		}
	}
	st.flush(s)
	if val, ok := v.(Validator); ok {
		if err := val.Validate(); err != nil {
			return p.list, err
		}
	}
	return p.list, nil
}

// decodeState holds the scratch values of one Decode call, so a key of a
// map does not allocate.
type decodeState struct {
	root    reflect.Value
	entries []Entry
	next    int        // index in entries of the entry being applied
	maps    []mapState // by field.slot; nil until a key reaches a map
}

// mapState decodes the keys of one kv:"x.*" field.
type mapState struct {
	m        reflect.Value     // the map field; a map of values is created on first use
	plain    map[string]string // m, for the map[string]string fast path
	hint     int               // upper bound of the keys reaching it, to size its storage once
	key, val reflect.Value     // scratch, reused for every entry
	// Struct elements are decoded in elems, then written back once by
	// flush, so each name is copied out of the map once, not once per key.
	elems reflect.Value // []element
	names []pendingName // names[i] is elems[i]
	index map[string]int
}

type pendingName struct {
	name string
	set  bool // at least one key was accepted
}

const (
	// linearNames is how many names are searched linearly before index
	// is built.
	linearNames = 16
	// maxElemHint caps the elements of a map of structs allocated up front.
	maxElemHint = 64
)

// mapFor returns the state of the map field f. The first call sets every
// map's hint from the entries left, so a config without map keys never
// counts. A key is counted for every prefix it starts with, so a key that
// apply passes on to a shorter prefix is counted there.
func (st *decodeState) mapFor(s *schema, f *field) *mapState {
	if st.maps == nil {
		st.maps = make([]mapState, len(s.prefixes))
		for i := st.next; i < len(st.entries); i++ {
			key := st.entries[i].Key
			for _, p := range s.prefixes {
				if len(key) > len(p.key) && key[:len(p.key)] == p.key {
					st.maps[p.slot].hint++
				}
			}
		}
	}
	ms := &st.maps[f.slot]
	if !ms.m.IsValid() {
		ms.m = st.root.FieldByIndex(f.index)
		if f.elem != nil {
			// A name sets some of its fields: guess half of them per name,
			// capped, as elements are whole structs. elems grows if needed.
			ms.hint = min(2*ms.hint/max(len(f.elem.order), 1)+1, maxElemHint)
		}
		if ms.m.IsNil() {
			// Created even if every key is rejected, as before a strict error.
			// A map of structs gets its names in flush: hint is only a guess.
			size := ms.hint
			if f.elem != nil {
				size = 0
			}
			ms.m.Set(reflect.MakeMapWithSize(f.typ, size))
		}
		if f.elem == nil {
			if f.typ == stringMapType && f.val.plain() {
				ms.plain = ms.m.Interface().(map[string]string)
			} else {
				ms.val = reflect.New(f.typ.Elem()).Elem()
			}
		}
		if ms.plain == nil {
			ms.key = reflect.New(f.typ.Key()).Elem()
		}
	}
	return ms
}

// element returns the index in ms.elems of the pending element name.
func (ms *mapState) element(f *field, name string) int {
	if ms.index != nil {
		if i, ok := ms.index[name]; ok {
			return i
		}
	} else {
		for i := range ms.names {
			if ms.names[i].name == name {
				return i
			}
		}
	}
	i := len(ms.names)
	if !ms.elems.IsValid() {
		ms.elems = reflect.MakeSlice(f.elems, ms.hint, ms.hint)
		ms.names = make([]pendingName, 0, ms.hint)
	} else if i == ms.elems.Len() {
		grown := reflect.MakeSlice(f.elems, 2*i, 2*i)
		reflect.Copy(grown, ms.elems)
		ms.elems = grown
	}
	ms.names = append(ms.names, pendingName{name: name})
	if ms.index != nil {
		ms.index[name] = i
	} else if len(ms.names) > linearNames {
		ms.index = make(map[string]int, ms.hint)
		for j, p := range ms.names {
			ms.index[p.name] = j
		}
	}
	ev := ms.elems.Index(i)
	ms.key.SetString(name)
	if cur := ms.m.MapIndex(ms.key); cur.IsValid() {
		ev.Set(cur)
	} else if f.defaults {
		ev.Addr().Interface().(Defaulter).SetDefaults()
	}
	return i
}

// flush writes the pending struct elements back to their maps.
func (st *decodeState) flush(s *schema) {
	for slot := range st.maps {
		ms, f := &st.maps[slot], s.prefixes[slot]
		if f.elem == nil || !ms.m.IsValid() {
			continue
		}
		for i, p := range ms.names {
			if p.set {
				ms.key.SetString(p.name)
				ms.m.SetMapIndex(ms.key, ms.elems.Index(i))
			}
		}
	}
}

// apply sets the field of e. msg explains a rejected value; known is false
// when no field takes the key.
func (s *schema) apply(st *decodeState, e *Entry) (msg string, known bool) {
	root := st.root
	if f := s.keys[e.Key]; f != nil {
		return f.set(root.FieldByIndex(f.index), e.Value), true
	}
	for _, f := range s.prefixes {
		rest, ok := strings.CutPrefix(e.Key, f.key)
		if !ok || rest == "" {
			continue
		}
		name, sub := rest, (*field)(nil)
		if f.elem != nil {
			// Names may contain dots: the longest field suffix wins.
			for _, ef := range f.elem.order {
				n, ok := strings.CutSuffix(rest, ef.key)
				if ok && len(n) > 1 && n[len(n)-1] == '.' && (sub == nil || len(ef.key) > len(sub.key)) {
					name, sub = n[:len(n)-1], ef
				}
			}
			if sub == nil {
				continue
			}
		}
		ms := st.mapFor(s, f)
		if sub != nil {
			// A name whose every key is rejected is not created.
			i := ms.element(f, name)
			if msg = sub.set(ms.elems.Index(i).FieldByIndex(sub.index), e.Value); msg == "" {
				ms.names[i].set = true
			}
			return msg, true
		}
		if ms.plain != nil {
			ms.plain[name] = e.Value
			return "", true
		}
		if msg = f.val.set(ms.val, e.Value); msg == "" {
			ms.key.SetString(name)
			ms.m.SetMapIndex(ms.key, ms.val)
		}
		return msg, true
	}
	if f := s.catchAll; f != nil {
		dst := root.FieldByIndex(f.index)
		if f.typ == entrySliceType {
			list := dst.Addr().Interface().(*[]Entry)
			*list = append(*list, *e)
		} else {
			m := dst.Addr().Interface().(*map[string]string)
			if *m == nil {
				*m = map[string]string{}
			}
			(*m)[e.Key] = e.Value
		}
		return "", true
	}
	return "", false
}

// set decodes raw into dst, leaving dst unchanged on failure. The message
// never contains raw: values may be secrets.
func (f *field) set(dst reflect.Value, raw string) string {
	if f.off && raw == "off" {
		dst.SetZero()
		return ""
	}
	if f.nonempty && raw == "" {
		return "must not be empty"
	}
	if err := f.decode(dst, raw); err != nil {
		return err.Error()
	}
	return ""
}

func (f *field) decode(dst reflect.Value, raw string) error {
	if f.oneof != nil && !slices.Contains(f.oneof, raw) {
		return errors.New("must be " + orList(f.oneof))
	}
	if f.text {
		p := reflect.New(f.typ)
		if err := p.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(raw)); err != nil {
			// Third-party errors often quote the input: show only ours.
			var ve ValueError
			if errors.As(err, &ve) {
				return ve
			}
			return errors.New("invalid " + f.typ.String())
		}
		dst.Set(p.Elem())
		return nil
	}
	switch f.typ.Kind() {
	case reflect.String:
		dst.SetString(raw)
	case reflect.Bool:
		b, err := ParseBool(raw)
		if err != nil {
			return err
		}
		dst.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var n int64
		var err error
		if f.duration {
			var d time.Duration
			if d, err = time.ParseDuration(raw); err != nil {
				return errors.New("must be a duration such as 500ms, 5m or 1h30m")
			}
			n = int64(d)
		} else if n, err = strconv.ParseInt(raw, 10, f.typ.Bits()); err != nil {
			return errors.New("must be an integer")
		}
		if err := f.check(float64(n)); err != nil {
			return err
		}
		dst.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, f.typ.Bits())
		if err != nil {
			return errors.New("must be a non-negative integer")
		}
		if err := f.check(float64(n)); err != nil {
			return err
		}
		dst.SetUint(n)
	case reflect.Float32, reflect.Float64:
		x, err := strconv.ParseFloat(raw, f.typ.Bits())
		if err != nil {
			return errors.New("must be a number")
		}
		if err := f.check(x); err != nil {
			return err
		}
		dst.SetFloat(x)
	case reflect.Slice:
		n := 0
		for part := range strings.SplitSeq(raw, f.sep) {
			if strings.TrimSpace(part) != "" {
				n++
			}
		}
		list := reflect.MakeSlice(f.typ, n, n)
		i := 0
		for part := range strings.SplitSeq(raw, f.sep) {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			if err := f.item.decode(list.Index(i), part); err != nil {
				return fmt.Errorf("item %d: %w", i+1, err)
			}
			i++
		}
		dst.Set(list)
	}
	return nil
}

// plain reports a string field without any constraint.
func (f *field) plain() bool {
	return f.typ.Kind() == reflect.String && !f.text && !f.off && !f.nonempty && f.oneof == nil
}

func (f *field) check(x float64) error {
	if x != x || (f.hasMin || f.hasMax) && math.IsInf(x, 0) {
		return errors.New("must be a finite number")
	}
	if f.hasMin && x < f.min {
		return errors.New("must be at least " + f.minText)
	}
	if f.hasMax && x > f.max {
		return errors.New("must be at most " + f.maxText)
	}
	return nil
}

func orList(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// ParseBool accepts on/off, true/false, yes/no and 1/0, in any case.
func ParseBool(s string) (bool, error) {
	switch {
	case s == "on" || s == "1" || strings.EqualFold(s, "on") || strings.EqualFold(s, "true") || strings.EqualFold(s, "yes"):
		return true, nil
	case s == "off" || s == "0" || strings.EqualFold(s, "off") || strings.EqualFold(s, "false") || strings.EqualFold(s, "no"):
		return false, nil
	}
	return false, errors.New("must be on or off")
}
