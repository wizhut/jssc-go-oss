package jssc_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// buildCorpus generates records with enough distinct strings to push past the
// one-byte index boundary, enough repetition for the pool to pay off, and
// enough members per object to trigger offset tables.
func buildCorpus(n int) []map[string]any {
	recs := make([]map[string]any, n)
	for i := range n {
		recs[i] = map[string]any{
			"id":       json.Number(fmt.Sprint(i)),
			"uuid":     fmt.Sprintf("%08x-%04x-4000-8000-%012x", i*2654435761&0xffffffff, i&0xffff, i),
			"name":     fmt.Sprintf("user_%d_name", i),
			"email":    fmt.Sprintf("user%d@example.com", i),
			"role":     []string{"admin", "user", "guest"}[i%3],
			"active":   i%2 == 0,
			"score":    json.Number(fmt.Sprintf("%d.%02d", i%100, i%97)),
			"initial":  string(rune('a' + i%26)), // one byte: written inline
			"tags":     []any{fmt.Sprintf("tag%d", i%97), "common", fmt.Sprintf("tag%d", i%13)},
			"nested":   map[string]any{"a": map[string]any{"b": map[string]any{"c": json.Number(fmt.Sprint(i % 13))}}},
			"nothing":  nil,
			"comment":  strings.Repeat("padding ", i%4),
			"revision": json.Number(fmt.Sprint(i)),
		}
	}
	return recs
}

func TestWideCorpusJSON(t *testing.T) {
	recs := buildCorpus(2000)
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}

	archive, st := encode(t, raw, jssc.Options{Structure: jssc.StructDeflate})
	if st.IndexWidth != 2 {
		t.Fatalf("IndexWidth = %d, want 2 for a pool of %d", st.IndexWidth, st.UniqueStrings)
	}
	if st.InlineRefs == 0 {
		t.Error("no strings were written inline, but every record has a one-byte value")
	}
	if st.Ratio() <= 1 {
		t.Errorf("archive is %d bytes for %d of input; a repetitive corpus should shrink",
			st.TotalBytes, st.RawBytes)
	}
	t.Logf("%d -> %d bytes (%.2fx), pool %d, index %dB, inline %d",
		st.RawBytes, st.TotalBytes, st.Ratio(), st.UniqueStrings, st.IndexWidth, st.InlineRefs)

	jsonEqual(t, raw, decode(t, archive))

	// Selective access has to hold up at this size: reach deep into a late
	// record without materializing the rest.
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := a.QueryOne("$[1999].nested.a.b.c")
	if err != nil || !ok {
		t.Fatalf("QueryOne = %v, %v, %v", v, ok, err)
	}
	if want := json.Number(fmt.Sprint(1999 % 13)); v != want {
		t.Errorf("value = %v, want %v", v, want)
	}
	// With a grouped pool, one string costs one group. Selectivity is how many
	// groups were inflated, not how many entries fell out of them. The keys on
	// the path are scattered through a lexicographically sorted pool, so a few
	// groups is the floor — the claim is that it stays a small fraction.
	totalGroups := (a.PoolCount() + st.PoolGroupSize - 1) / st.PoolGroupSize
	if a.DecodedGroups > totalGroups/10 {
		t.Errorf("a single deep lookup inflated %d of %d pool groups; that is not selective",
			a.DecodedGroups, totalGroups)
	}
	t.Logf("deep lookup inflated %d of %d pool groups", a.DecodedGroups, totalGroups)

	names, err := a.QueryValues("$[*].email")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(recs) {
		t.Fatalf("got %d emails, want %d", len(names), len(recs))
	}
	if names[1500] != recs[1500]["email"] {
		t.Errorf("email 1500 = %v, want %v", names[1500], recs[1500]["email"])
	}
}

func TestWideCorpusNDJSON(t *testing.T) {
	recs := buildCorpus(2000)
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	raw := []byte(b.String())

	archive, st := encode(t, raw, jssc.Options{Format: jssc.FormatNDJSON, Structure: jssc.StructDeflate})
	if st.DocCount != len(recs) {
		t.Fatalf("DocCount = %d, want %d", st.DocCount, len(recs))
	}

	a, err := jssc.OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}

	// The root length table means the last document opens without touching the
	// first 1999.
	last, err := a.Doc(len(recs) - 1)
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := last.(map[string]any)
	if !ok {
		t.Fatalf("last document is %T, want an object", last)
	}
	if obj["name"] != recs[len(recs)-1]["name"] {
		t.Errorf("last name = %v, want %v", obj["name"], recs[len(recs)-1]["name"])
	}

	roles, err := a.QueryValues("$[*].role")
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != len(recs) {
		t.Fatalf("got %d roles, want %d", len(roles), len(recs))
	}
	want := make([]any, len(recs))
	for i, r := range recs {
		want[i] = r["role"]
	}
	if !reflect.DeepEqual(roles, want) {
		t.Error("roles did not round trip")
	}
}

func BenchmarkEncodeJSON(b *testing.B) {
	raw, err := json.Marshal(buildCorpus(1000))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := jssc.EncodeToBytes(raw, jssc.Options{Structure: jssc.StructDeflate}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeJSON(b *testing.B) {
	raw, err := json.Marshal(buildCorpus(1000))
	if err != nil {
		b.Fatal(err)
	}
	archive, _, err := jssc.EncodeToBytes(raw, jssc.Options{Structure: jssc.StructDeflate})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := jssc.DecodeBytes(archive); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkQueryArchive measures the case the format exists for: pulling one
// field out of every record without materializing the documents.
func BenchmarkQueryArchive(b *testing.B) {
	raw, err := json.Marshal(buildCorpus(1000))
	if err != nil {
		b.Fatal(err)
	}
	archive, _, err := jssc.EncodeToBytes(raw, jssc.Options{Structure: jssc.StructRaw})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for b.Loop() {
		a, err := jssc.OpenBytes(archive)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := a.QueryValues("$[*].email"); err != nil {
			b.Fatal(err)
		}
	}
}
