package rpc_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The wire format of every method of the agent's table is pinned by a golden
// file in testdata/wire: the JSON of its parameters and of its result, once
// as the zero value and once with every field set. The two binaries ship
// together, but a field renamed by accident would still break an API and
// an agent of the same build that disagree on nothing else, and the files
// make every change of the format a visible diff.
//
// CG_WIRE_UPDATE=1 rewrites the files (only for a deliberate change).

// sitePlaceholder stands for a state.Site, whose own format is the store's
// business (internal/state), not this package's.
const sitePlaceholder = `"@state.Site"`

var siteSample = state.Site{Domain: "example.com"}

var fixedTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fill sets every exported field of v, recursively: strings to the field's
// name, numbers to 7, booleans to true, one element per slice and map.
func fill(v reflect.Value, name string, depth int) {
	if depth > 8 {
		return
	}
	switch v.Type() {
	case reflect.TypeFor[state.Site]():
		v.Set(reflect.ValueOf(siteSample))
		return
	case reflect.TypeFor[time.Time]():
		v.Set(reflect.ValueOf(fixedTime))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(name)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), name, depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), name, depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fill(k, "key", depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e, name, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Interface:
		v.Set(reflect.ValueOf(name))
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			fill(v.Field(i), f.Name, depth+1)
		}
	}
}

// wire is the JSON of t, zero and filled, with every site replaced by the
// placeholder.
func wire(t *testing.T, typ reflect.Type) map[string]json.RawMessage {
	site, _ := json.Marshal(siteSample)
	zeroSite, _ := json.Marshal(state.Site{})
	out := map[string]json.RawMessage{}
	for _, kind := range []string{"zero", "full"} {
		v := reflect.New(typ).Elem()
		if kind == "full" {
			fill(v, typ.Name(), 0)
		}
		b, err := json.Marshal(v.Interface())
		if err != nil {
			t.Fatalf("%s: marshal: %v", typ, err)
		}
		b = bytes.ReplaceAll(b, site, []byte(sitePlaceholder))
		b = bytes.ReplaceAll(b, zeroSite, []byte(sitePlaceholder))
		out[kind] = b
	}
	return out
}

func TestWireGolden(t *testing.T) {
	update := os.Getenv("CG_WIRE_UPDATE") != ""
	dir := filepath.Join("testdata", "wire")
	seen := map[string]bool{}
	for _, spec := range agentrpc.Specs() {
		file := spec.Name + ".json"
		seen[file] = true
		doc := map[string]map[string]json.RawMessage{
			"params": wire(t, spec.Params),
			"result": wire(t, spec.Result),
		}
		got, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join(dir, file)
		if update {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden file (%v); a new method gets one with CG_WIRE_UPDATE=1", spec.Name, err)
			continue
		}
		// Compared as JSON values: the order of the fields is not part of
		// the format.
		var g, w any
		_ = json.Unmarshal(got, &g)
		if err := json.Unmarshal(want, &w); err != nil || !reflect.DeepEqual(g, w) {
			t.Errorf("%s: the wire format changed\n--- got\n%s\n--- want\n%s", spec.Name, got, want)
		}
		// Decoding the golden into the type and encoding it again gives the
		// same bytes: the field names are read as they are written.
		for _, part := range []string{"params", "result"} {
			typ := spec.Params
			if part == "result" {
				typ = spec.Result
			}
			var doc map[string]map[string]json.RawMessage
			_ = json.Unmarshal(want, &doc)
			raw := []byte(strings.ReplaceAll(string(doc[part]["full"]), sitePlaceholder, mustJSON(siteSample)))
			v := reflect.New(typ)
			if err := json.Unmarshal(raw, v.Interface()); err != nil {
				t.Errorf("%s %s: decode the golden: %v", spec.Name, part, err)
				continue
			}
			again, _ := json.Marshal(v.Elem().Interface())
			again = bytes.ReplaceAll(again, []byte(mustJSON(siteSample)), []byte(sitePlaceholder))
			var a, b any
			_ = json.Unmarshal(again, &a)
			_ = json.Unmarshal(doc[part]["full"], &b)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("%s %s: decode and encode again differ:\n%s\n%s", spec.Name, part, again, doc[part]["full"])
			}
		}
	}
	if update {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !seen[e.Name()] {
			t.Errorf("testdata/wire/%s names no method of the table", e.Name())
		}
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
