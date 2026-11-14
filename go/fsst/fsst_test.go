package fsst_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/fsst"
)

func sampleStrings() []string {
	var out []string
	for i := range 200 {
		out = append(out,
			fmt.Sprintf(`{"id":%d,"name":"user%d","email":"user%d@example.com"}`, i, i, i),
			fmt.Sprintf("https://example.com/api/v1/resource/%d", i),
			fmt.Sprintf("2026-08-%02dT12:%02d:00Z", 1+i%28, i%60),
		)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	inputs := sampleStrings()
	tbl := fsst.TrainStrings(inputs)

	for _, s := range inputs {
		enc := tbl.Encode([]byte(s))
		got := tbl.DecodeAll(enc)
		if string(got) != s {
			t.Fatalf("round trip failed\n want %q\n got  %q", s, got)
		}
	}
}

func TestRoundTripEdgeCases(t *testing.T) {
	tbl := fsst.TrainStrings(sampleStrings())
	cases := []string{
		"",
		"a",
		"ab",
		strings.Repeat("x", 1),
		strings.Repeat("x", 7),
		strings.Repeat("x", 8),
		strings.Repeat("x", 9),
		strings.Repeat("long repetitive content ", 500),
		"unicode: αβγδε 漢字 🎉🎊",
		"\x00\x01\x02\xff\xfe binary bytes",
		"a string the table has never seen before, full of unusual content",
	}
	for _, s := range cases {
		enc := tbl.Encode([]byte(s))
		got := tbl.DecodeAll(enc)
		if string(got) != s {
			t.Errorf("round trip failed for %d bytes\n want %q\n got  %q", len(s), s, got)
		}
	}
}

func TestEntriesDecodeIndependently(t *testing.T) {
	// The whole point for JSSC: entry i must decode without touching entry j.
	inputs := sampleStrings()
	tbl := fsst.TrainStrings(inputs)

	encoded := make([][]byte, len(inputs))
	for i, s := range inputs {
		encoded[i] = tbl.Encode([]byte(s))
	}
	for i := len(encoded) - 1; i >= 0; i-- {
		if got := string(tbl.DecodeAll(encoded[i])); got != inputs[i] {
			t.Fatalf("entry %d decoded out of order gave %q, want %q", i, got, inputs[i])
		}
	}
}

func TestTableSerialization(t *testing.T) {
	inputs := sampleStrings()
	tbl := fsst.TrainStrings(inputs)

	var buf bytes.Buffer
	n, err := tbl.WriteTo(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != buf.Len() {
		t.Errorf("WriteTo reported %d bytes, wrote %d", n, buf.Len())
	}

	var restored fsst.Table
	read, err := restored.ReadFrom(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if read != n {
		t.Errorf("ReadFrom consumed %d bytes, WriteTo wrote %d", read, n)
	}
	if restored.NumSymbols() != tbl.NumSymbols() {
		t.Errorf("symbol count changed: %d -> %d", tbl.NumSymbols(), restored.NumSymbols())
	}

	// A table read back has to decode what the original encoded.
	for _, s := range inputs {
		enc := tbl.Encode([]byte(s))
		if got := string(restored.DecodeAll(enc)); got != s {
			t.Fatalf("restored table decoded %q as %q", s, got)
		}
	}
}

func TestReadFromRejectsGarbage(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("short"),
		bytes.Repeat([]byte{0xff}, 64),
		make([]byte, 16), // valid length, version word of zero
	}
	for i, raw := range cases {
		var tbl fsst.Table
		if _, err := tbl.ReadFrom(bytes.NewReader(raw)); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
}

func TestTrainOnEmptySet(t *testing.T) {
	// An archive with an empty pool still carries a table.
	tbl := fsst.Train([][]byte{{}})
	var buf bytes.Buffer
	if _, err := tbl.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	var restored fsst.Table
	if _, err := restored.ReadFrom(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("an escape-only table must round trip: %v", err)
	}
	const s = "anything at all"
	if got := string(restored.DecodeAll(tbl.Encode([]byte(s)))); got != s {
		t.Errorf("got %q, want %q", got, s)
	}
}

func TestCompressesRepetitiveInput(t *testing.T) {
	inputs := sampleStrings()
	tbl := fsst.TrainStrings(inputs)

	var raw, enc int
	for _, s := range inputs {
		raw += len(s)
		enc += len(tbl.Encode([]byte(s)))
	}
	if enc >= raw {
		t.Errorf("encoded %d bytes from %d; repetitive input should shrink", enc, raw)
	}
	t.Logf("%d -> %d bytes (%.2fx) with %d symbols", raw, enc, float64(raw)/float64(enc), tbl.NumSymbols())
}

func BenchmarkEncode(b *testing.B) {
	inputs := sampleStrings()
	tbl := fsst.TrainStrings(inputs)
	data := [][]byte{}
	for _, s := range inputs {
		data = append(data, []byte(s))
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		tbl.Encode(data[i%len(data)])
	}
}

func BenchmarkDecode(b *testing.B) {
	inputs := sampleStrings()
	tbl := fsst.TrainStrings(inputs)
	var encoded [][]byte
	for _, s := range inputs {
		encoded = append(encoded, tbl.Encode([]byte(s)))
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		tbl.DecodeAll(encoded[i%len(encoded)])
	}
}
