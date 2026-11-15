package arkprofile

import (
	"encoding"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Decode reads one YAML document from r into v, a pointer to a [Profile] or
// to a struct embedding one with the tag `yaml:",inline"`, so an embedder can
// keep its own keys such as a schedule next to the profile's.
//
// Decoding is strict: an unknown key, a key given twice, a `password:` key, a
// value of the wrong kind, an empty input and a second document fail with an
// error naming the line and the field path, such as
// "line 4: backup.pahts: unknown field"; a fraction for a whole number is the
// wrong kind. Strictness covers the struct fields down to nested structs; a
// map or a slice decodes as YAML has it. Anchors, aliases and the merge key
// `<<` work as in YAML. Every error matches [ErrInvalid] except the one for
// a v that cannot hold a profile: an inline field that is not a struct (a
// *Profile included) or a key two fields share.
// Decode does not call [Profile.Validate].
func Decode(r io.Reader, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("decode a profile: need a pointer to a struct, got %T", v)
	}

	dec := yaml.NewDecoder(r)
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return invalidf("empty profile")
		}
		return invalidf("%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return invalidf("%s", strings.TrimPrefix(err.Error(), "yaml: "))
	default:
		return invalidf("line %d: a second YAML document, a profile is one document", extra.Line)
	}

	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return invalidf("empty profile")
	}
	return decodeStruct(root, rv.Elem(), "")
}

func decodeStruct(n *yaml.Node, v reflect.Value, path string) error {
	n = resolve(n)
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		where := path
		if where == "" {
			where = "profile"
		}
		return fieldErrorf(n.Line, where, "want a mapping of keys")
	}

	fields := map[string]reflect.Value{}
	if err := collectFields(v, fields); err != nil {
		return err
	}
	pairs, err := mappingPairs(n, path)
	if err != nil {
		return err
	}
	for _, kv := range pairs {
		k, val := kv[0], kv[1]
		p := joinPath(path, k.Value)
		f, ok := fields[k.Value]
		switch {
		case !ok && k.Value == "password":
			return fieldErrorf(k.Line, p, "a profile holds no password, name a file with password-file "+
				"or a variable with password-env")
		case !ok:
			return fieldErrorf(k.Line, p, "unknown field")
		case isSection(f.Type()):
			if err := decodeStruct(val, f, p); err != nil {
				return err
			}
		case isInteger(f.Type()) && resolve(val).ShortTag() == "!!float":
			return fieldErrorf(val.Line, p, "want %s", describe(f.Type()))
		default:
			if err := val.Decode(f.Addr().Interface()); err != nil {
				if _, ok := errors.AsType[*yaml.TypeError](err); ok {
					return fieldErrorf(val.Line, p, "want %s", describe(f.Type()))
				}
				return fieldErrorf(val.Line, p, "%s", strings.TrimPrefix(err.Error(), "yaml: "))
			}
		}
	}
	return nil
}

// mappingPairs returns the key-value pairs of the mapping n with the merge
// key `<<` resolved: n's own keys win over merged ones, and an earlier merged
// mapping wins over a later one. A key given twice in n fails.
func mappingPairs(n *yaml.Node, path string) ([][2]*yaml.Node, error) {
	var own, merged [][2]*yaml.Node
	seen := map[string]int{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			where := path
			if where == "" {
				where = "profile"
			}
			return nil, fieldErrorf(k.Line, where, "a key must be a plain name, not a list or a mapping")
		}
		if k.ShortTag() == "!!merge" {
			pairs, err := mergedPairs(val, path)
			if err != nil {
				return nil, err
			}
			merged = append(merged, pairs...)
			continue
		}
		if line, ok := seen[k.Value]; ok {
			return nil, fieldErrorf(k.Line, joinPath(path, k.Value), "already defined at line %d", line)
		}
		seen[k.Value] = k.Line
		own = append(own, [2]*yaml.Node{k, val})
	}
	for _, kv := range merged {
		if _, ok := seen[kv[0].Value]; !ok {
			seen[kv[0].Value] = kv[0].Line
			own = append(own, kv)
		}
	}
	return own, nil
}

// mergedPairs returns the pairs a merge key's value brings in: one mapping or
// a list of them, each usually an alias.
func mergedPairs(val *yaml.Node, path string) ([][2]*yaml.Node, error) {
	val = resolve(val)
	items := []*yaml.Node{val}
	if val.Kind == yaml.SequenceNode {
		items = val.Content
	}
	var out [][2]*yaml.Node
	for _, m := range items {
		m = resolve(m)
		if m.Kind != yaml.MappingNode {
			return nil, fieldErrorf(m.Line, joinPath(path, "<<"), "want a mapping or a list of mappings to merge")
		}
		pairs, err := mappingPairs(m, path)
		if err != nil {
			return nil, err
		}
		out = append(out, pairs...)
	}
	return out, nil
}

func resolve(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.AliasNode {
		return n.Alias
	}
	return n
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// collectFields maps the yaml keys of v's fields to the fields, with the
// fields of inline structs at v's level. A key used by two fields fails, as
// it does in yaml.v3.
func collectFields(v reflect.Value, out map[string]reflect.Value) error {
	t := v.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if hasOption(opts, "inline") {
			if sf.Type.Kind() != reflect.Struct {
				return fmt.Errorf("decode a profile: inline field %s of %s must be a struct, not %s",
					sf.Name, t, sf.Type)
			}
			if err := collectFields(v.Field(i), out); err != nil {
				return err
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(sf.Name)
		}
		if _, dup := out[name]; dup {
			return fmt.Errorf("decode a profile: key %q is used by two fields, the second in %s", name, t)
		}
		out[name] = v.Field(i)
	}
	return nil
}

func hasOption(opts, want string) bool {
	for o := range strings.SplitSeq(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

var (
	unmarshalerType     = reflect.TypeFor[yaml.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	durationType        = reflect.TypeFor[time.Duration]()
)

// isSection reports a struct decoded key by key; a struct that decodes itself
// is a value.
func isSection(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return t.Kind() == reflect.Struct && !pt.Implements(unmarshalerType) && !pt.Implements(textUnmarshalerType)
}

func isInteger(t reflect.Type) bool {
	switch t.Kind() { //nolint:exhaustive // only the integer kinds matter
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return t != durationType
	}
	return false
}

func describe(t reflect.Type) string {
	if t == durationType {
		return "a duration such as 30s or 5m"
	}
	switch t.Kind() { //nolint:exhaustive // other kinds take the default
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return "a list of strings"
		}
		return "a list"
	case reflect.Map:
		return "a mapping"
	default:
		return "a value of type " + t.String()
	}
}

func fieldErrorf(line int, path, format string, args ...any) error {
	return invalidf("line %d: %s: %s", line, path, fmt.Sprintf(format, args...))
}
