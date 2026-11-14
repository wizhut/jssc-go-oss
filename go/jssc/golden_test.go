package jssc_test

import (
	"bytes"
	"compress/flate"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// Golden archives pin the wire format. A change in this package that alters
// what a JSSC archive means will break these, which is the point: the format is
// the contract, and other implementations depend on it.
//
// The JSS3 and JSS4 fixtures were produced by a separate, earlier
// implementation, so they check interoperability, not just self-consistency —
// and they must keep decoding after every format revision. The JSS2 fixture
// comes from this package's own write paths and guards them against
// regression.
//
// Earlier JSS5–JSS7 fixtures carried a pool codec that has since been retired
// (specification §5.1) and no longer decode, so they are not included. The
// JSS5–JSS7 layouts are held by the round-trip and stability tests.

type goldenCase struct {
	file    string
	source  string // the text the archive was built from
	version int
	format  jssc.ContentFormat
}

var goldenCases = []goldenCase{
	{file: "users.jss5-perentry.jssc", source: "users.json", version: 5, format: jssc.FormatJSON},
	{file: "users.jss3-deflate.jssc", source: "users.json", version: 3, format: jssc.FormatJSON},
	{file: "users.jss3-raw.jssc", source: "users.json", version: 3, format: jssc.FormatJSON},
	{file: "users.jss2-deflate.jssc", source: "users.json", version: 2, format: jssc.FormatJSON},
	{file: "events.jss4-deflate.jssc", source: "events.ndjson", version: 4, format: jssc.FormatNDJSON},
}

func TestGoldenArchivesDecode(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.file, func(t *testing.T) {
			archive, err := os.ReadFile(filepath.Join("testdata", "golden", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			source, err := os.ReadFile(filepath.Join("testdata", tc.source))
			if err != nil {
				t.Fatal(err)
			}

			text, st, err := jssc.DecodeBytes(archive)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if st.Version != tc.version {
				t.Errorf("Version = %d, want %d", st.Version, tc.version)
			}
			if st.ContentFormat != tc.format {
				t.Errorf("ContentFormat = %v, want %v", st.ContentFormat, tc.format)
			}

			if tc.format.LineOriented() {
				wantLines := splitLines(source)
				gotLines := splitLines(text)
				if len(wantLines) != len(gotLines) {
					t.Fatalf("restored %d lines, want %d", len(gotLines), len(wantLines))
				}
				for i := range wantLines {
					jsonEqual(t, wantLines[i], gotLines[i])
				}
				return
			}
			jsonEqual(t, source, text)
		})
	}
}

func TestGoldenArchivesQuery(t *testing.T) {
	for _, tc := range goldenCases {
		if tc.source != "users.json" {
			continue
		}
		t.Run(tc.file, func(t *testing.T) {
			archive, err := os.ReadFile(filepath.Join("testdata", "golden", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			a, err := jssc.OpenBytes(archive)
			if err != nil {
				t.Fatal(err)
			}
			names, err := a.QueryValues("$[*].name")
			if err != nil {
				t.Fatal(err)
			}
			want := []any{"Alice", "Bob", "Carol", "Dave", "Eve", "Frank", "Grace", "Heidi"}
			if len(names) != len(want) {
				t.Fatalf("got %d names, want %d", len(names), len(want))
			}
			for i := range want {
				if names[i] != want[i] {
					t.Errorf("name %d = %v, want %v", i, names[i], want[i])
				}
			}
		})
	}
}

// TestReencodeGoldenIsStable re-encodes each fixture's source and compares the
// result to the fixture. What it pins is that this encoder still makes the same
// choices as the implementation that produced the fixture: pool order, index
// width, key width, member order, the offset tables, the structure codec and
// the framing around it.
//
// Every case names its version explicitly. JSS5 changed the pool framing, the
// default codec, and the offset-table rule, so these only reproduce when asked
// for the older encodings — which is the point: the old encodings still have to
// be exactly what they were.
//
// Every surviving case also names its Algorithm explicitly. That is now load-
// bearing: the default pool codec changed from deflate to fsst, so a case that
// let the default decide would re-encode to different bytes than the fixture
// holds. Naming it keeps the fixture pinned to the codec it was written with.
//
// The JSS1–JSS4 fixtures came from a different implementation, which is what
// makes them interoperability evidence.
//
// # Why the structure region is compared inflated rather than byte for byte
//
// A DEFLATE stream's bytes are not part of the wire format's contract. The
// specification fixes the framing around the structure — struct_codec,
// struct_len, the block layout — and then asks of the payload only that
// "Decompression MUST yield exactly the plain block" (§ Structure
// compression); "at maximum compression" there names a setting, not an output.
// Conformance says the same from the other side: its checks are each "a
// property, not a byte comparison, so an independent implementation can check
// it without matching the reference byte for byte" (§ Conformance), and the
// determinism it does require is of one encoder against itself — same input
// and options, same bytes — not of one compressor against another. An
// independent implementation may well compress structure blocks differently —
// a fast lz4 mode where this package searches at level 9, say — with the same
// block format, different stored bytes, and nothing different on any read.
//
// So a byte-exact assertion over the stored structure region was asserting
// something the format never promised, and it came due. Go 1.27's
// compress/flate emits three bytes fewer than Go 1.26's for these inputs,
// which broke the four deflate cases on an otherwise clean tree — fixtures
// untouched, this package untouched, the drift entirely in the standard
// library. Pinning the toolchain would hold a stdlib compressor's output
// steady only until the next time it moved, and would freeze the project's Go
// version to do it. Regenerating the fixtures is not on the table: they are
// another implementation's bytes and the record of what the old layouts were.
//
// This keeps every byte the format fixes and gives up only the bytes it does
// not. Everything up to struct_len must match the fixture exactly, and the
// stored structure must inflate to exactly the fixture's plain structure. A
// change in pool order, index width, member order, an offset table or the
// codec choice still moves bytes in the prefix or in the plain structure, so
// all of that stays pinned; only the compressor's freedom to spend fewer bits
// on the same block is allowed.
func TestReencodeGoldenIsStable(t *testing.T) {
	cases := []struct {
		file   string
		source string
		opt    jssc.Options
	}{
		{"users.jss5-perentry.jssc", "users.json",
			jssc.Options{Structure: jssc.StructDeflate, PerEntryPool: true, Algorithm: "fsst", Version: 5}},
		{"users.jss3-deflate.jssc", "users.json",
			jssc.Options{Structure: jssc.StructDeflate, Algorithm: "fsst", Legacy4: true}},
		{"users.jss3-raw.jssc", "users.json",
			jssc.Options{Structure: jssc.StructRaw, Algorithm: "fsst", Legacy4: true}},
		{"users.jss2-deflate.jssc", "users.json",
			jssc.Options{Structure: jssc.StructDeflate, Algorithm: "fsst", Layout: jssc.LayoutLegacy}},
		{"events.jss4-deflate.jssc", "events.ndjson",
			jssc.Options{Structure: jssc.StructDeflate, Algorithm: "fsst", Format: jssc.FormatNDJSON, Legacy4: true}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("testdata", tc.source))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "golden", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := encode(t, source, tc.opt)
			compareReencode(t, got, want)
		})
	}
}

// compareReencode holds a re-encode to its fixture as tightly as the format
// allows: byte for byte everywhere the specification fixes bytes, and by the
// plain structure where it fixes only that decompression reproduces it. See
// TestReencodeGoldenIsStable for why that line falls where it does.
func compareReencode(t *testing.T, got, want []byte) {
	t.Helper()

	wantStats, err := jssc.Inspect(want)
	if err != nil {
		t.Fatalf("inspect fixture: %v", err)
	}
	gotStats, err := jssc.Inspect(got)
	if err != nil {
		t.Fatalf("inspect re-encode: %v", err)
	}
	if gotStats.StructCodec != wantStats.StructCodec {
		t.Fatalf("re-encoded with struct_codec %v, fixture has %v",
			gotStats.StructCodec, wantStats.StructCodec)
	}
	if gotStats.Version != wantStats.Version {
		t.Fatalf("re-encoded as JSS%d, fixture is JSS%d", gotStats.Version, wantStats.Version)
	}

	// Nothing stands between a raw structure and the file, so every byte of
	// such an archive is fixed and gets compared as one.
	if wantStats.StructCodec == jssc.StructRaw {
		compareBytes(t, "archive", got, want)
		return
	}
	// Only the single-stream deflate layout of JSS1–JSS6 (Appendix E) is
	// inflated in one piece here. The blocked structure of JSS7 and later and
	// the lz4 codec both need their own reading of the region; a fixture that
	// arrives in either shape should get that reading rather than a weaker
	// check than the one above.
	if wantStats.StructCodec != jssc.StructDeflate || wantStats.Version >= 7 {
		t.Fatalf("no structure comparison for struct_codec %v at JSS%d: teach this test "+
			"that layout rather than loosening it", wantStats.StructCodec, wantStats.Version)
	}

	// The structure region is the archive's last section and its four-byte
	// struct_len sits immediately before it (§ Container), so the stored
	// bytes run from len-struct_len to the end and everything the format
	// fixes byte for byte is the prefix before that length.
	gotStart, wantStart := len(got)-gotStats.StructBytes, len(want)-wantStats.StructBytes
	if gotStart < 4 || wantStart < 4 {
		t.Fatalf("struct_len %d leaves no room before the structure region in a %d-byte archive",
			wantStats.StructBytes, len(want))
	}
	compareBytes(t, "the bytes before struct_len", got[:gotStart-4], want[:wantStart-4])
	compareBytes(t, "the plain structure",
		inflateStructure(t, got[gotStart:]), inflateStructure(t, want[wantStart:]))
}

func compareBytes(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: re-encoded to %d bytes, fixture has %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: differs from the fixture at byte %d: 0x%02x vs 0x%02x",
				what, i, got[i], want[i])
		}
	}
}

func inflateStructure(t *testing.T, stored []byte) []byte {
	t.Helper()
	plain, err := io.ReadAll(flate.NewReader(bytes.NewReader(stored)))
	if err != nil {
		t.Fatalf("inflate the structure region: %v", err)
	}
	return plain
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, []byte(line))
		}
	}
	return out
}
