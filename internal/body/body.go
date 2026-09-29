// Package body builds JSON request bodies from command-line fields.
//
// Keys are dotted paths into the body: content.progress sets a nested field,
// content.step_labels[] appends to an array, actions[0].id indexes into one,
// and a backslash escapes a literal dot. -f values are always strings; -F
// values are typed the way gh api types them.
package body

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// MaxInput caps what one stdin or file argument may hold. The largest body
// the API accepts is a 256 KiB HTML email; anything past a MiB is a mistake,
// better caught before it is uploaded.
const MaxInput = 1 << 20

// Read resolves an argument that names its content elsewhere: - or @- for
// stdin, @path for a file. ok is false for any other argument, which the
// caller then uses literally.
func Read(arg string, stdin io.Reader) (data []byte, ok bool, err error) {
	var r io.Reader
	switch {
	case arg == "-" || arg == "@-":
		r = stdin
	case strings.HasPrefix(arg, "@"):
		f, err := os.Open(arg[1:]) // #nosec G304 -- the user names the file on their own command line
		if err != nil {
			return nil, true, err
		}
		defer f.Close()
		r = f
	default:
		return nil, false, nil
	}
	data, err = io.ReadAll(io.LimitReader(r, MaxInput+1))
	if err == nil && len(data) > MaxInput {
		err = fmt.Errorf("%s is larger than %d bytes", arg, MaxInput)
	}
	return data, true, err
}

// ParseJSON reads a --data argument: a literal object, @path to read a file,
// or - / @- to read stdin. The result is always a JSON object.
func ParseJSON(arg string, stdin io.Reader) (map[string]any, error) {
	data, ok, err := Read(arg, stdin)
	if err != nil {
		return nil, fmt.Errorf("--data: %w", err)
	}
	if !ok {
		data = []byte(arg)
	}
	v, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("--data: %w", err)
	}
	obj, isObj := v.(map[string]any)
	if !isObj {
		return nil, errors.New("--data: must be a JSON object")
	}
	return obj, nil
}

// ApplyField parses one key=value field and sets it on obj. typed selects
// -F semantics: true/false/null, JSON numbers, {...} and [...] literals, and
// @path / @- file reads. Otherwise the value is always a string.
func ApplyField(obj map[string]any, field string, typed bool, stdin io.Reader) error {
	key, raw, ok := strings.Cut(field, "=")
	if !ok || key == "" {
		return fmt.Errorf("field %q: want key=value", field)
	}
	var value any = raw
	if typed {
		v, err := typedValue(raw, stdin)
		if err != nil {
			return fmt.Errorf("field %q: %w", key, err)
		}
		value = v
	}
	if err := Set(obj, key, value); err != nil {
		return fmt.Errorf("field %q: %w", key, err)
	}
	return nil
}

// typedValue converts a -F value to its JSON type.
func typedValue(raw string, stdin io.Reader) (any, error) {
	switch {
	case raw == "true":
		return true, nil
	case raw == "false":
		return false, nil
	case raw == "null":
		return nil, nil
	case jsonNumber.MatchString(raw):
		return json.Number(raw), nil
	case strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "["):
		return decode([]byte(raw))
	case strings.HasPrefix(raw, "@"):
		b, _, err := Read(raw, stdin)
		return string(b), err
	}
	return raw, nil
}

func decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("invalid JSON: trailing data")
	}
	return v, nil
}

// Set assigns value at a dotted path, creating objects and arrays on the way.
func Set(obj map[string]any, path string, value any) error {
	elems, err := parsePath(path)
	if err != nil {
		return err
	}
	_, err = set(obj, elems, value)
	return err
}

const (
	noIndex     = -2
	appendIndex = -1
)

type elem struct {
	key   string
	index int
}

func parsePath(path string) ([]elem, error) {
	var elems []elem
	var seg strings.Builder
	flush := func() error {
		s := seg.String()
		seg.Reset()
		key, rest := s, ""
		if i := strings.IndexByte(s, '['); i >= 0 {
			key, rest = s[:i], s[i:]
		}
		if key == "" {
			return fmt.Errorf("empty key in %q", path)
		}
		elems = append(elems, elem{key: key, index: noIndex})
		for rest != "" {
			if rest[0] != '[' {
				return fmt.Errorf("bad index in %q", path)
			}
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return fmt.Errorf("unterminated [ in %q", path)
			}
			inner := rest[1:end]
			rest = rest[end+1:]
			if inner == "" {
				elems = append(elems, elem{index: appendIndex})
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil || n < 0 {
				return fmt.Errorf("bad index [%s] in %q", inner, path)
			}
			elems = append(elems, elem{index: n})
		}
		return nil
	}
	for i := 0; i < len(path); i++ {
		switch c := path[i]; {
		case c == '\\' && i+1 < len(path) && path[i+1] == '.':
			seg.WriteByte('.')
			i++
		case c == '.':
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			seg.WriteByte(c)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return elems, nil
}

func set(cur any, elems []elem, value any) (any, error) {
	if len(elems) == 0 {
		return value, nil
	}
	e, rest := elems[0], elems[1:]
	if e.index == noIndex {
		m, ok := cur.(map[string]any)
		if cur == nil {
			m = map[string]any{}
		} else if !ok {
			return nil, fmt.Errorf("%q is not an object", e.key)
		}
		v, err := set(m[e.key], rest, value)
		if err != nil {
			return nil, err
		}
		m[e.key] = v
		return m, nil
	}
	arr, ok := cur.([]any)
	if cur != nil && !ok {
		return nil, errors.New("indexing a value that is not an array")
	}
	switch {
	case e.index == appendIndex || e.index == len(arr):
		v, err := set(nil, rest, value)
		if err != nil {
			return nil, err
		}
		return append(arr, v), nil
	case e.index > len(arr):
		return nil, fmt.Errorf("index [%d] is past the end of an array of %d (use [] to append)", e.index, len(arr))
	}
	v, err := set(arr[e.index], rest, value)
	if err != nil {
		return nil, err
	}
	arr[e.index] = v
	return arr, nil
}

// Merge deep-merges src into dst. Objects merge key by key; anything else in
// src, arrays included, replaces what dst had.
func Merge(dst, src map[string]any) {
	for k, sv := range src {
		sm, sok := sv.(map[string]any)
		dm, dok := dst[k].(map[string]any)
		if sok && dok {
			Merge(dm, sm)
			continue
		}
		dst[k] = sv
	}
}
