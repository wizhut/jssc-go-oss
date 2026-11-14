package jssc_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ohler55/ojg/jp"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

func parseJSON(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return v
}

func openTestArchive(t *testing.T, path string, opt jssc.Options) (*jssc.Archive, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := encode(t, raw, opt)
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return a, raw
}

// wantSameAsOJG checks the selective walk against ojg evaluating the same path
// over the fully parsed document — the walk is an optimization, never a
// different answer.
func wantSameAsOJG(t *testing.T, a *jssc.Archive, raw []byte, path string, multiDoc bool) {
	t.Helper()

	var data any
	if multiDoc {
		var docs []any
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			docs = append(docs, parseJSON(t, line))
		}
		data = docs
	} else {
		data = parseJSON(t, raw)
	}

	expr, err := jp.ParseString(path)
	if err != nil {
		t.Fatalf("parse %q: %v", path, err)
	}
	want := expr.Get(data)

	got, err := a.QueryValues(path)
	if err != nil {
		t.Fatalf("query %q: %v", path, err)
	}
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("query %q\nwant %#v\ngot  %#v", path, want, got)
	}
}

func TestQueryMatchesOJG(t *testing.T) {
	paths := []string{
		"$[0].name",
		"$[*].name",
		"$[*].role",
		"$..role",
		"$..name",
		"$[2:5].name",
		"$[:3].name",
		"$[5:].name",
		"$[::2].name",
		"$[1:7:3].name",
		"$[-3:].name",
		"$[:-2].name",
		"$[7:2:-2].name",
		"$[::-1].name",
		"$[20:30].name",
		"$[5:2].name",
		"$[-1].name",
		"$[0,2].name",
		"$[*]['name','role']",
		`$[?(@.role=="admin")].name`,
		"$[*]",
		"$",
		"$[99].name",
		"$[*].missing",
	}
	for _, layout := range []jssc.Layout{jssc.LayoutDefault, jssc.LayoutLenPrefix, jssc.LayoutOffsets, jssc.LayoutLegacy} {
		for _, sc := range []jssc.StructCodec{jssc.StructRaw, jssc.StructDeflate} {
			opt := jssc.Options{Layout: layout, Structure: sc}
			a, raw := openTestArchive(t, "testdata/users.json", opt)
			for _, p := range paths {
				t.Run(layout.String()+"/"+sc.String()+"/"+p, func(t *testing.T) {
					wantSameAsOJG(t, a, raw, p, false)
				})
			}
		}
	}
}

func TestQueryMultiDoc(t *testing.T) {
	paths := []string{
		"$[0].type",
		"$[*].type",
		"$[*].actor.login",
		"$..login",
		"$[1:3].id",
		"$[*].repo.name",
		`$[?(@.type=="PushEvent")].id`,
		"$[4].actor.id",
		"$[-1].id",
	}
	for _, sc := range []jssc.StructCodec{jssc.StructRaw, jssc.StructDeflate} {
		a, raw := openTestArchive(t, "testdata/events.ndjson",
			jssc.Options{Format: jssc.FormatNDJSON, Structure: sc})
		for _, p := range paths {
			t.Run(sc.String()+"/"+p, func(t *testing.T) {
				wantSameAsOJG(t, a, raw, p, true)
			})
		}
	}
}

func TestQueryPathsAreNormalized(t *testing.T) {
	a, _ := openTestArchive(t, "testdata/users.json", jssc.Options{})
	paths, err := a.QueryPaths("$[*].name")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"$[0].name", "$[1].name", "$[2].name", "$[3].name",
		"$[4].name", "$[5].name", "$[6].name", "$[7].name",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestQueryDecodesFewStrings(t *testing.T) {
	// The point of the format: answering "$[*].name" should not decompress the
	// whole pool. With a group size of 1 the accounting is exact — only the
	// strings the path touches come out.
	a, _ := openTestArchive(t, "testdata/users.json", jssc.Options{PerEntryPool: true, Algorithm: "fsst"})
	values, err := a.QueryValues("$[*].name")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 8 {
		t.Fatalf("got %d names, want 8", len(values))
	}
	if a.DecodedStrings >= a.PoolCount() {
		t.Errorf("decoded %d of %d pool strings; a selective query should decode fewer",
			a.DecodedStrings, a.PoolCount())
	}
	if a.SkippedValues == 0 {
		t.Error("no values were skipped; the walk should be jumping over subtrees")
	}
	t.Logf("decoded %d of %d pool strings in %d group(s), skipped %d values",
		a.DecodedStrings, a.PoolCount(), a.DecodedGroups, a.SkippedValues)
}

// TestQueryDecodesFewGroups is the same claim under a grouped pool, where the
// unit of work is a group rather than an entry. Reaching one string pulls its
// neighbours out with it, so the honest measure of selectivity is how many
// groups had to be inflated.
//
// Grouping is no longer the default — the pool codecs compress one entry at a
// time, so a group of one is what an encoder writes — but PoolGroupShift still
// produces valid archives and readers must stay selective over them.
func TestQueryDecodesFewGroups(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := range 400 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"user_%d","email":"user%d@example.com","note":"note number %d"}`,
			i, i, i, i)
	}
	b.WriteByte(']')

	archive, st := encode(t, []byte(b.String()), jssc.Options{PoolGroupShift: 6})
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}
	if st.PoolGroupSize <= 1 {
		t.Fatalf("PoolGroupSize = %d; this test is about grouped pools", st.PoolGroupSize)
	}

	v, ok, err := a.QueryOne("$[200].name")
	if err != nil || !ok {
		t.Fatalf("QueryOne = %v, %v, %v", v, ok, err)
	}
	if v != "user_200" {
		t.Errorf("value = %v, want user_200", v)
	}

	totalGroups := (a.PoolCount() + st.PoolGroupSize - 1) / st.PoolGroupSize
	if a.DecodedGroups > totalGroups/4 {
		t.Errorf("a single-value lookup inflated %d of %d pool groups; that is not selective",
			a.DecodedGroups, totalGroups)
	}
	t.Logf("inflated %d of %d groups (%d entries materialized of %d), skipped %d values",
		a.DecodedGroups, totalGroups, a.DecodedStrings, a.PoolCount(), a.SkippedValues)
}

func TestQueryOneStopsEarly(t *testing.T) {
	a, _ := openTestArchive(t, "testdata/users.json", jssc.Options{})

	all, err := a.QueryValues("$[*].name")
	if err != nil {
		t.Fatal(err)
	}
	allWork := a.SkippedValues

	v, ok, err := a.QueryOne("$[*].name")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || v != all[0] {
		t.Fatalf("QueryOne = %v, %v; want %v", v, ok, all[0])
	}
	if a.SkippedValues >= allWork {
		t.Errorf("QueryOne skipped %d values, the full query skipped %d; "+
			"an early exit should do strictly less work", a.SkippedValues, allWork)
	}
}

func TestQueryLimit(t *testing.T) {
	a, _ := openTestArchive(t, "testdata/users.json", jssc.Options{})
	for _, limit := range []int{1, 3, 8, 20} {
		matches, err := a.QueryLimit("$[*].name", limit)
		if err != nil {
			t.Fatal(err)
		}
		want := min(limit, 8)
		if len(matches) != want {
			t.Errorf("limit %d returned %d matches, want %d", limit, len(matches), want)
		}
	}
	matches, err := a.QueryLimit("$[*].name", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 8 {
		t.Errorf("limit 0 returned %d matches, want all 8", len(matches))
	}
}

func TestQueryOneNoMatch(t *testing.T) {
	a, _ := openTestArchive(t, "testdata/users.json", jssc.Options{})
	v, ok, err := a.QueryOne("$[*].nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if ok || v != nil {
		t.Errorf("QueryOne = %v, %v; want nil, false", v, ok)
	}
}

func TestQueryRejectsBadPath(t *testing.T) {
	a, _ := openTestArchive(t, "testdata/users.json", jssc.Options{})
	if _, err := a.Query("$[["); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestArchiveDocAccess(t *testing.T) {
	a, _ := openTestArchive(t, "testdata/events.ndjson", jssc.Options{Format: jssc.FormatJSONL})
	if a.DocCount() != 5 {
		t.Fatalf("DocCount = %d, want 5", a.DocCount())
	}
	// Document 4 must be readable without touching 0..3.
	doc, err := a.Doc(4)
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("Doc(4) returned %T, want an object", doc)
	}
	if obj["id"] != "5" {
		t.Errorf(`Doc(4)["id"] = %v, want "5"`, obj["id"])
	}
	if _, err := a.Doc(5); err == nil {
		t.Error("Doc(5) should be out of range")
	}
	if _, err := a.Doc(-1); err == nil {
		t.Error("Doc(-1) should be out of range")
	}
}
