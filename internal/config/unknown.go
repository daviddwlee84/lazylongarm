package config

import (
	"bytes"
	"reflect"
	"sort"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Several lazylongarm builds can share one config file (a release, a global
// install, a dev build). Keys this build does not know, usually written by a
// newer build or mistyped by hand, are therefore not an error: read records
// them, the CLI warns about them, and write puts them back, so an older build
// never strips a newer build's settings.

// unknownKey is one key of the document that matches no field of Config.
type unknownKey struct {
	at    []step // tables leading to the one that holds the key
	key   string
	value any
}

// step is one table on the way to an unknown key: a field of the parent
// table, and for an array-of-tables entry the entry's id (index for display
// only; entries are matched by id when writing).
type step struct {
	key      string
	entry    bool
	id       string
	index    int
	optional bool // a pointer field: if the struct is gone, so are its unknown keys
}

// UnknownKeys lists the keys of the loaded file this build does not know, as
// dotted paths such as "targets[worker-1].pool_name". They are kept when
// the config is written.
func (c *Config) UnknownKeys() []string {
	out := make([]string, 0, len(c.unknown))
	for _, u := range c.unknown {
		out = append(out, u.path())
	}
	return out
}

func (u unknownKey) path() string {
	var b strings.Builder
	for _, s := range u.at {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.key)
		if s.entry {
			b.WriteByte('[')
			if s.id != "" {
				b.WriteString(s.id)
			} else {
				b.WriteString(strconv.Itoa(s.index))
			}
			b.WriteByte(']')
		}
	}
	if b.Len() > 0 {
		b.WriteByte('.')
	}
	b.WriteString(u.key)
	return b.String()
}

// findUnknown walks raw (the document decoded into maps) alongside the struct
// type t and returns the keys t has no field for. Map-typed fields accept any
// key. Field names match case-insensitively, as go-toml's decoder does, so a
// key the struct already absorbed is never kept a second time.
func findUnknown(raw map[string]any, t reflect.Type, at []step) []unknownKey {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []unknownKey
	for _, k := range keys {
		v := raw[k]
		f, ok := tomlField(t, k)
		if !ok {
			out = append(out, unknownKey{at: append([]step(nil), at...), key: k, value: v})
			continue
		}
		ft := f.Type
		optional := ft.Kind() == reflect.Pointer
		if optional {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct:
			if m, ok := v.(map[string]any); ok {
				out = append(out, findUnknown(m, ft, append(at, step{key: k, optional: optional}))...)
			}
		case ft.Kind() == reflect.Slice && deref(ft.Elem()).Kind() == reflect.Struct:
			arr, _ := v.([]any)
			for i, e := range arr {
				if m, ok := e.(map[string]any); ok {
					id, _ := m["id"].(string)
					out = append(out, findUnknown(m, deref(ft.Elem()), append(at, step{key: k, entry: true, id: id, index: i}))...)
				}
			}
		}
	}
	return out
}

func deref(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

// tomlField finds the exported field of struct t that TOML key k decodes into.
func tomlField(t reflect.Type, k string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if strings.EqualFold(name, k) {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// withUnknown merges c's unknown keys into encoded (c rendered as TOML) and
// re-encodes the result. A key whose entry this operation removed (matched by
// id) or whose optional table is gone is dropped with it. Plain tables that
// were omitted because all their known fields are empty are recreated. The
// merged document is written from maps, so its keys come out sorted. It also
// returns the unknown keys that were kept.
func (c *Config) withUnknown(encoded []byte) ([]byte, []unknownKey, error) {
	if len(c.unknown) == 0 {
		return encoded, nil, nil
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(encoded, &doc); err != nil {
		return nil, nil, err
	}
	var kept []unknownKey
	for _, u := range c.unknown {
		if tbl := locate(doc, u.at); tbl != nil {
			if _, taken := tbl[u.key]; !taken {
				tbl[u.key] = u.value
				kept = append(kept, u)
			}
		}
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(doc); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), kept, nil
}

// locate returns the table at path in doc, creating plain tables on the way,
// or nil when an entry or optional table no longer exists.
func locate(doc map[string]any, at []step) map[string]any {
	tbl := doc
	for _, s := range at {
		if s.entry {
			arr, _ := tbl[s.key].([]any)
			var found map[string]any
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok && s.id != "" && m["id"] == s.id {
					found = m
					break
				}
			}
			if found == nil {
				return nil
			}
			tbl = found
			continue
		}
		next, ok := tbl[s.key].(map[string]any)
		if !ok {
			if s.optional {
				return nil
			}
			next = map[string]any{}
			tbl[s.key] = next
		}
		tbl = next
	}
	return tbl
}
