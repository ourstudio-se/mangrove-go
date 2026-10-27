package mangrove

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

/*
 * Result data is handled as JSON-like values that keep the order of object
 * keys, as GraphQL responses must list fields in the order they were
 * selected. A value is one of:
 *
 *   nil, bool, string, json.Number, []any, *Object
 */

// Object is a JSON object that remembers the order of its keys.
type Object struct {
	keys   []string
	values map[string]any
}

// NewObject returns an empty object with room for n keys.
func NewObject(n int) *Object {
	return &Object{keys: make([]string, 0, n), values: make(map[string]any, n)}
}

// Get returns the value of key, and whether the object has the key.
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	value, ok := o.values[key]
	return value, ok
}

// Has reports whether the object has key.
func (o *Object) Has(key string) bool {
	if o == nil {
		return false
	}
	_, ok := o.values[key]
	return ok
}

// Set sets the value of key, appending the key if it is new.
func (o *Object) Set(key string, value any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// Delete removes key.
func (o *Object) Delete(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			return
		}
	}
}

// Keys returns the keys in order. The slice must not be modified.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return o.keys
}

// Len returns the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// MarshalJSON encodes the object with its keys in order.
func (o *Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeValue(&buf, o); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalJSON decodes an object, keeping the order of its keys.
func (o *Object) UnmarshalJSON(data []byte) error {
	value, err := DecodeJSON(data)
	if err != nil {
		return err
	}
	object, ok := value.(*Object)
	if !ok {
		return fmt.Errorf("mangrove: expected a JSON object, got %s", typeName(value))
	}
	*o = *object
	return nil
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		return "number"
	case []any:
		return "array"
	case *Object:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func isObject(value any) bool {
	o, ok := value.(*Object)
	return ok && o != nil
}

func asObject(value any) *Object {
	o, _ := value.(*Object)
	return o
}

// cloneData returns a deep copy of a value.
func cloneData(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneData(item)
		}
		return out
	case *Object:
		if v == nil {
			return v
		}
		out := NewObject(len(v.keys))
		for _, key := range v.keys {
			out.Set(key, cloneData(v.values[key]))
		}
		return out
	default:
		return value
	}
}

func cloneObject(o *Object) *Object {
	return cloneData(o).(*Object)
}

// shallowCopy returns a copy of the object sharing its values.
func shallowCopy(o *Object) *Object {
	out := NewObject(o.Len())
	if o == nil {
		return out
	}
	for _, key := range o.keys {
		out.Set(key, o.values[key])
	}
	return out
}

// EncodeJSON encodes a value, keeping the order of object keys.
func EncodeJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeValue(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeValue(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		encodeString(buf, v)
	case json.Number:
		if v == "" {
			buf.WriteString("0")
		} else {
			buf.WriteString(string(v))
		}
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeValue(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case *Object:
		if v == nil {
			buf.WriteString("null")
			return nil
		}
		buf.WriteByte('{')
		for i, key := range v.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeString(buf, key)
			buf.WriteByte(':')
			if err := encodeValue(buf, v.values[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		// Anything else (e.g. values produced by custom hooks) is encoded the
		// standard way.
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(encoded)
	}
	return nil
}

const hexDigits = "0123456789abcdef"

func encodeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			buf.WriteString(s[start:i])
			switch b {
			case '"', '\\':
				buf.WriteByte('\\')
				buf.WriteByte(b)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			default:
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[b>>4])
				buf.WriteByte(hexDigits[b&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf.WriteString(s[start:i])
			buf.WriteString(`�`)
			i += size
			start = i
			continue
		}
		i += size
	}
	buf.WriteString(s[start:])
	buf.WriteByte('"')
}

// DecodeJSON decodes JSON into ordered values. Numbers are kept as
// json.Number.
func DecodeJSON(data []byte) (any, error) {
	d := decoder{data: data}
	d.skipSpace()
	value, err := d.value()
	if err != nil {
		return nil, err
	}
	d.skipSpace()
	if d.pos != len(d.data) {
		return nil, d.errorf("unexpected data after JSON value")
	}
	return value, nil
}

type decoder struct {
	data []byte
	pos  int
}

var errUnexpectedEnd = errors.New("mangrove: unexpected end of JSON input")

func (d *decoder) errorf(format string, args ...any) error {
	return fmt.Errorf("mangrove: invalid JSON at offset %d: %s", d.pos, fmt.Sprintf(format, args...))
}

func (d *decoder) skipSpace() {
	for d.pos < len(d.data) {
		switch d.data[d.pos] {
		case ' ', '\t', '\n', '\r':
			d.pos++
		default:
			return
		}
	}
}

func (d *decoder) value() (any, error) {
	if d.pos >= len(d.data) {
		return nil, errUnexpectedEnd
	}
	switch c := d.data[d.pos]; {
	case c == '{':
		return d.object()
	case c == '[':
		return d.array()
	case c == '"':
		return d.string()
	case c == 't':
		return true, d.literal("true")
	case c == 'f':
		return false, d.literal("false")
	case c == 'n':
		return nil, d.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	default:
		return nil, d.errorf("unexpected character %q", c)
	}
}

func (d *decoder) literal(word string) error {
	if !bytes.HasPrefix(d.data[d.pos:], []byte(word)) {
		return d.errorf("expected %s", word)
	}
	d.pos += len(word)
	return nil
}

func (d *decoder) number() (any, error) {
	start := d.pos
	if d.data[d.pos] == '-' {
		d.pos++
	}
	digits := func() int {
		n := 0
		for d.pos < len(d.data) && d.data[d.pos] >= '0' && d.data[d.pos] <= '9' {
			d.pos++
			n++
		}
		return n
	}
	if digits() == 0 {
		return nil, d.errorf("invalid number")
	}
	if d.pos < len(d.data) && d.data[d.pos] == '.' {
		d.pos++
		if digits() == 0 {
			return nil, d.errorf("invalid number")
		}
	}
	if d.pos < len(d.data) && (d.data[d.pos] == 'e' || d.data[d.pos] == 'E') {
		d.pos++
		if d.pos < len(d.data) && (d.data[d.pos] == '+' || d.data[d.pos] == '-') {
			d.pos++
		}
		if digits() == 0 {
			return nil, d.errorf("invalid number")
		}
	}
	return json.Number(d.data[start:d.pos]), nil
}

func (d *decoder) string() (string, error) {
	start := d.pos
	d.pos++
	escaped := false
	for d.pos < len(d.data) {
		switch c := d.data[d.pos]; {
		case c == '"':
			d.pos++
			if !escaped {
				return string(d.data[start+1 : d.pos-1]), nil
			}
			var s string
			if err := json.Unmarshal(d.data[start:d.pos], &s); err != nil {
				return "", err
			}
			return s, nil
		case c == '\\':
			escaped = true
			d.pos += 2
		case c < 0x20:
			return "", d.errorf("control character in string")
		default:
			d.pos++
		}
	}
	return "", errUnexpectedEnd
}

func (d *decoder) array() (any, error) {
	d.pos++
	out := []any{}
	d.skipSpace()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		return out, nil
	}
	for {
		d.skipSpace()
		item, err := d.value()
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		d.skipSpace()
		if d.pos >= len(d.data) {
			return nil, errUnexpectedEnd
		}
		switch d.data[d.pos] {
		case ',':
			d.pos++
		case ']':
			d.pos++
			return out, nil
		default:
			return nil, d.errorf("expected , or ]")
		}
	}
}

func (d *decoder) object() (any, error) {
	d.pos++
	out := NewObject(4)
	d.skipSpace()
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return out, nil
	}
	for {
		d.skipSpace()
		if d.pos >= len(d.data) {
			return nil, errUnexpectedEnd
		}
		if d.data[d.pos] != '"' {
			return nil, d.errorf("expected a key")
		}
		key, err := d.string()
		if err != nil {
			return nil, err
		}
		d.skipSpace()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return nil, d.errorf("expected :")
		}
		d.pos++
		d.skipSpace()
		value, err := d.value()
		if err != nil {
			return nil, err
		}
		out.Set(key, value)
		d.skipSpace()
		if d.pos >= len(d.data) {
			return nil, errUnexpectedEnd
		}
		switch d.data[d.pos] {
		case ',':
			d.pos++
		case '}':
			d.pos++
			return out, nil
		default:
			return nil, d.errorf("expected , or }")
		}
	}
}

// FromGo converts plain Go values (maps, slices, numbers) into ordered
// values. Map keys are sorted, as Go maps have no order.
func FromGo(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return DecodeJSON(encoded)
}
