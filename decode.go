package kvconf

import (
	"encoding"
	"errors"
	"fmt"
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
	item     *field  // element of a slice
	val      *field  // element of a map of values
	elem     *schema // element of a map of structs
	defaults bool    // map element implements Defaulter
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
				f = &field{elem: elem, defaults: reflect.PointerTo(et).Implements(defaulterType)}
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
	f := &field{typ: t, text: isText(t), duration: t == durationType, sep: tag.Get("sep")}
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
			item.oneof, f.oneof = f.oneof, nil
			f.item = item
		default:
			return nil, fmt.Errorf("kvconf: %s: unsupported type %s", where, t)
		}
	}
	bound := func(s string) (float64, error) {
		if f.duration || f.item != nil && f.item.duration {
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
	if f.item != nil {
		f.item.hasMin, f.item.min, f.item.minText = f.hasMin, f.min, f.minText
		f.item.hasMax, f.item.max, f.item.maxText = f.hasMax, f.max, f.maxText
		f.hasMin, f.hasMax = false, false
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
	st := decodeState{root: rv.Elem()}
	p := problems{strict: opts.Strict}
	for i := range entries {
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
	st.flush()
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
	root reflect.Value
	// scratch key and element per map field, reused for every entry.
	keys, vals map[*field]reflect.Value
	// pending struct elements, written back to their maps once at the end
	// so each name is copied out of its map once, not once per key.
	pending map[*field]map[string]*pendingElem
}

type pendingElem struct {
	val reflect.Value
	set bool // at least one key was accepted
}

func (st *decodeState) scratch(f *field) (key, val reflect.Value) {
	if st.keys == nil {
		st.keys, st.vals = map[*field]reflect.Value{}, map[*field]reflect.Value{}
	}
	key, ok := st.keys[f]
	if !ok {
		key = reflect.New(f.typ.Key()).Elem()
		val = reflect.New(f.typ.Elem()).Elem()
		st.keys[f], st.vals[f] = key, val
	}
	return key, st.vals[f]
}

// element returns the addressable pending element name of the map field f.
func (st *decodeState) element(f *field, m reflect.Value, name string) *pendingElem {
	if st.pending == nil {
		st.pending = map[*field]map[string]*pendingElem{}
	}
	byName := st.pending[f]
	if byName == nil {
		byName = map[string]*pendingElem{}
		st.pending[f] = byName
	}
	if p, ok := byName[name]; ok {
		return p
	}
	p := &pendingElem{val: reflect.New(f.typ.Elem()).Elem()}
	key, _ := st.scratch(f)
	key.SetString(name)
	if cur := m.MapIndex(key); cur.IsValid() {
		p.val.Set(cur)
	} else if f.defaults {
		p.val.Addr().Interface().(Defaulter).SetDefaults()
	}
	byName[name] = p
	return p
}

// flush writes the pending struct elements back to their maps.
func (st *decodeState) flush() {
	for f, byName := range st.pending {
		m := st.root.FieldByIndex(f.index)
		key, _ := st.scratch(f)
		for name, p := range byName {
			if p.set {
				key.SetString(name)
				m.SetMapIndex(key, p.val)
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
		m := root.FieldByIndex(f.index)
		if m.IsNil() {
			m.Set(reflect.MakeMap(f.typ))
		}
		if sub != nil {
			// A name whose every key is rejected is not created.
			p := st.element(f, m, name)
			if msg = sub.set(p.val.FieldByIndex(sub.index), e.Value); msg == "" {
				p.set = true
			}
			return msg, true
		}
		if f.typ == stringMapType && f.val.plain() {
			(*(*map[string]string)(m.Addr().UnsafePointer()))[name] = e.Value
			return "", true
		}
		key, val := st.scratch(f)
		if msg = f.val.set(val, e.Value); msg == "" {
			key.SetString(name)
			m.SetMapIndex(key, val)
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
			return err
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
