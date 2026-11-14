package jssc_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// jsonEqual compares two JSON texts as values, so object member order and
// insignificant whitespace do not matter — that is exactly the round-trip
// guarantee the specification makes (§13).
func jsonEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var wv, gv any
	dw := json.NewDecoder(bytes.NewReader(want))
	dw.UseNumber()
	if err := dw.Decode(&wv); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	dg := json.NewDecoder(bytes.NewReader(got))
	dg.UseNumber()
	if err := dg.Decode(&gv); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if !reflect.DeepEqual(wv, gv) {
		t.Fatalf("round-trip changed the value\nwant %#v\ngot  %#v", wv, gv)
	}
}

func encode(t *testing.T, raw []byte, opt jssc.Options) ([]byte, *jssc.Stats) {
	t.Helper()
	archive, st, err := jssc.EncodeToBytes(raw, opt)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return archive, st
}

func decode(t *testing.T, archive []byte) []byte {
	t.Helper()
	text, _, err := jssc.DecodeBytes(archive)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return text
}

var roundTripCases = []struct {
	name string
	json string
}{
	{"null", `null`},
	{"true", `true`},
	{"number", `42`},
	{"string", `"hello"`},
	{"empty object", `{}`},
	{"empty array", `[]`},
	{"empty string value", `{"k":""}`},
	{"nested", `{"a":{"b":{"c":[1,2,{"d":null}]}}}`},
	{"unicode", `{"greek":"αβγ","emoji":"🎉","escaped":"line\nbreak\ttab"}`},
	{"number lexemes", `[1e3,1000,0.1,-0.0,1.0,123456789012345678901234567890,1E+10]`},
	{"mixed array", `[null,true,false,0,"",{"k":"v"},[1,[2,[3]]]]`},
	{"long strings", `{"a":"` + strings.Repeat("x", 300) + `"}`},
	{"many keys", `{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9}`},
	{"duplicate values", `["same","same","same","same"]`},
}

// LatestArchiveVersion mirrors jssc.LatestVersion so a version bump is a
// one-line change here rather than a scatter of literals.
const LatestArchiveVersion = jssc.LatestVersion

func TestRoundTripValues(t *testing.T) {
	for _, tc := range roundTripCases {
		for _, sc := range []jssc.StructCodec{jssc.StructRaw, jssc.StructDeflate} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, sc), func(t *testing.T) {
				archive, _ := encode(t, []byte(tc.json), jssc.Options{Structure: sc})
				jsonEqual(t, []byte(tc.json), decode(t, archive))
			})
		}
	}
}

func TestRoundTripLayouts(t *testing.T) {
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	layouts := []struct {
		name   string
		layout jssc.Layout
		want   int // expected archive version
	}{
		{"default", 0, 8},
		{"len prefix only", jssc.LayoutLenPrefix, 8},
		{"offsets only", jssc.LayoutOffsets, 8},
		{"both", jssc.LayoutDefault, 8},
		{"legacy jss2", jssc.LayoutLegacy, 2},
	}
	for _, l := range layouts {
		t.Run(l.name, func(t *testing.T) {
			archive, st := encode(t, raw, jssc.Options{Layout: l.layout, Structure: jssc.StructDeflate})
			if st.Version != l.want {
				t.Errorf("version = %d, want %d", st.Version, l.want)
			}
			jsonEqual(t, raw, decode(t, archive))
		})
	}
}

func TestRoundTripLineFormats(t *testing.T) {
	raw, err := os.ReadFile("testdata/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []jssc.ContentFormat{jssc.FormatJSONL, jssc.FormatNDJSON} {
		t.Run(format.String(), func(t *testing.T) {
			archive, st := encode(t, raw, jssc.Options{Format: format, Structure: jssc.StructDeflate})
			if st.Version != LatestArchiveVersion {
				t.Errorf("version = %d, want %d", st.Version, LatestArchiveVersion)
			}
			if st.DocCount != 5 {
				t.Errorf("DocCount = %d, want 5", st.DocCount)
			}
			text, dst, err := jssc.DecodeBytes(archive)
			if err != nil {
				t.Fatal(err)
			}
			// The label round-trips even though jsonl and ndjson share a wire form.
			if dst.ContentFormat != format {
				t.Errorf("ContentFormat = %v, want %v", dst.ContentFormat, format)
			}
			wantLines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
			gotLines := bytes.Split(bytes.TrimSpace(text), []byte("\n"))
			if len(wantLines) != len(gotLines) {
				t.Fatalf("got %d lines, want %d", len(gotLines), len(wantLines))
			}
			for i := range wantLines {
				jsonEqual(t, wantLines[i], gotLines[i])
			}
		})
	}
}

func TestMultiDocSharesOnePool(t *testing.T) {
	raw, err := os.ReadFile("testdata/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	_, shared := encode(t, raw, jssc.Options{Format: jssc.FormatNDJSON})

	// Encoding each line on its own must cost more pool entries in total than
	// the shared pool does, since the documents repeat keys and repo names.
	total := 0
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		_, st := encode(t, line, jssc.Options{})
		total += st.UniqueStrings
	}
	if shared.UniqueStrings >= total {
		t.Errorf("shared pool holds %d strings, per-document pools hold %d in total; "+
			"the shared pool should be smaller", shared.UniqueStrings, total)
	}
}

func TestMaxDocsTruncates(t *testing.T) {
	raw, err := os.ReadFile("testdata/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	archive, st := encode(t, raw, jssc.Options{Format: jssc.FormatNDJSON, MaxDocs: 2})
	if st.DocCount != 2 {
		t.Fatalf("DocCount = %d, want 2", st.DocCount)
	}
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}
	if a.DocCount() != 2 {
		t.Errorf("archive DocCount = %d, want 2", a.DocCount())
	}
	ids, err := a.QueryValues("$[*].id")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []any{"1", "2"}) {
		t.Errorf("ids = %v, want [1 2]", ids)
	}
}

func TestNumberLexemesArePreserved(t *testing.T) {
	// 1e3 and 1000 are the same number but different lexemes; the format stores
	// the text, so both survive intact (specification §8.7).
	const raw = `{"a":1e3,"b":1000,"c":1.0,"d":0.30000000000000004}`
	archive, _ := encode(t, []byte(raw), jssc.Options{})
	got := decode(t, archive)
	if string(got) != raw {
		t.Errorf("lexemes changed\nwant %s\ngot  %s", raw, got)
	}
}

func TestObjectMembersSortedByKeyIndex(t *testing.T) {
	// Members are written in ascending pool-index order, and the pool is sorted
	// lexicographically, so decoded object keys come back in sorted order.
	const raw = `{"zebra":1,"alpha":2,"mango":3}`
	archive, _ := encode(t, []byte(raw), jssc.Options{})
	got := decode(t, archive)
	if want := `{"alpha":2,"mango":3,"zebra":1}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := encode(t, raw, jssc.Options{Structure: jssc.StructDeflate})
	for i := range 5 {
		again, _ := encode(t, raw, jssc.Options{Structure: jssc.StructDeflate})
		if !bytes.Equal(first, again) {
			t.Fatalf("encode %d differed from the first; archives must not depend on map order", i)
		}
	}
}

func TestEncodeRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		opt  jssc.Options
	}{
		{"empty", "", jssc.Options{}},
		{"whitespace only", "   \n\t ", jssc.Options{}},
		{"malformed json", `{"a":`, jssc.Options{}},
		{"trailing data", `{"a":1} {"b":2}`, jssc.Options{}},
		{"bad line", "{\"a\":1}\nnot json\n", jssc.Options{Format: jssc.FormatJSONL}},
		{"empty lines only", "\n\n\n", jssc.Options{Format: jssc.FormatJSONL}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := jssc.EncodeToBytes([]byte(tc.raw), tc.opt); err == nil {
				t.Fatal("expected an error")
			} else if !errors.Is(err, jssc.ErrInvalidInput) {
				t.Errorf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestEncodeRejectsLegacyLayoutForLines(t *testing.T) {
	_, _, err := jssc.EncodeToBytes([]byte(`{"a":1}`), jssc.Options{
		Format: jssc.FormatJSONL,
		Layout: jssc.LayoutLegacy,
	})
	if !errors.Is(err, jssc.ErrInvalidInput) {
		t.Errorf("error = %v, want ErrInvalidInput", err)
	}
}

func TestEncodeRejectsOverlongNumber(t *testing.T) {
	raw := []byte(`[1` + strings.Repeat("0", 300) + `]`)
	_, _, err := jssc.EncodeToBytes(raw, jssc.Options{})
	if !errors.Is(err, jssc.ErrLimitExceeded) {
		t.Errorf("error = %v, want ErrLimitExceeded", err)
	}
}

func TestEncodeRejectsDeepNesting(t *testing.T) {
	raw := []byte(strings.Repeat("[", 60) + strings.Repeat("]", 60))
	if _, _, err := jssc.EncodeToBytes(raw, jssc.Options{MaxDepth: 10}); !errors.Is(err, jssc.ErrLimitExceeded) {
		t.Errorf("error = %v, want ErrLimitExceeded", err)
	}
	if _, _, err := jssc.EncodeToBytes(raw, jssc.Options{}); err != nil {
		t.Errorf("default depth should accept 60 levels: %v", err)
	}
}

// TestConcurrencyDoesNotChangeTheArchive is the constraint that governs the
// parallel pool encoder. Determinism is normative: the same input and options
// must produce the same bytes however many workers did the compressing.
func TestConcurrencyDoesNotChangeTheArchive(t *testing.T) {
	inputs := []struct {
		name string
		raw  []byte
		opt  jssc.Options
	}{}

	users, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile("testdata/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}

	// A corpus with enough distinct strings to span many pool groups, so the
	// workers genuinely interleave.
	var big bytes.Buffer
	big.WriteByte('[')
	for i := range 4000 {
		if i > 0 {
			big.WriteByte(',')
		}
		fmt.Fprintf(&big, `{"id":%d,"email":"user%d@example.com","note":"a distinct note number %d"}`, i, i, i)
	}
	big.WriteByte(']')

	for _, base := range []struct {
		name string
		raw  []byte
		opt  jssc.Options
	}{
		{"users", users, jssc.Options{}},
		{"events-ndjson", events, jssc.Options{Format: jssc.FormatNDJSON}},
		{"big", big.Bytes(), jssc.Options{}},
		{"big-raw-struct", big.Bytes(), jssc.Options{Structure: jssc.StructRaw}},
		{"big-deflate", big.Bytes(), jssc.Options{Structure: jssc.StructDeflate}},
		{"big-lz4", big.Bytes(), jssc.Options{Structure: jssc.StructLZ4}},
		{"big-lz4-ndjson", events, jssc.Options{Structure: jssc.StructLZ4, Format: jssc.FormatNDJSON}},
		{"big-fsst", big.Bytes(), jssc.Options{Algorithm: "fsst", PerEntryPool: true}},
		{"big-group8", big.Bytes(), jssc.Options{PoolGroupShift: 3}},
		{"big-jss4", big.Bytes(), jssc.Options{Algorithm: "fsst", Legacy4: true}},
	} {
		inputs = append(inputs, base)
	}

	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			seq := in.opt
			seq.Concurrency = 1
			want, _ := encode(t, in.raw, seq)

			for _, workers := range []int{2, 3, 8, 64} {
				opt := in.opt
				opt.Concurrency = workers
				got, _ := encode(t, in.raw, opt)
				if !bytes.Equal(want, got) {
					t.Fatalf("concurrency %d produced a different archive (%d vs %d bytes)",
						workers, len(got), len(want))
				}
			}

			// The default (GOMAXPROCS) must agree too, and repeat runs must not
			// drift as goroutines finish in different orders.
			for range 8 {
				got, _ := encode(t, in.raw, in.opt)
				if !bytes.Equal(want, got) {
					t.Fatal("the default concurrency produced a different archive")
				}
			}
			// And the archive still round-trips. A line-oriented archive
			// restores newline-delimited text, so compare it that way.
			text := decode(t, want)
			if in.opt.Format.LineOriented() {
				wantLines := bytes.Split(bytes.TrimSpace(in.raw), []byte("\n"))
				gotLines := bytes.Split(bytes.TrimSpace(text), []byte("\n"))
				if len(wantLines) != len(gotLines) {
					t.Fatalf("restored %d lines, want %d", len(gotLines), len(wantLines))
				}
				for i := range wantLines {
					jsonEqual(t, wantLines[i], gotLines[i])
				}
				return
			}
			jsonEqual(t, in.raw, text)
		})
	}
}
