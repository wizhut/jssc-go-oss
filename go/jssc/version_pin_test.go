package jssc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// pinDoc is one JSON document with enough in it to exercise the legacy layouts:
// a container of at least eight children (a JSS3/JSS4 offset table), pooled
// and inline strings, and a long repeated value so deflate has work to do.
var pinDoc = `{"id":7,"ok":true,"none":null,"tags":["a","bb","ccc","dddd","eeeee",` +
	`"ffffff","ggggggg","hhhhhhhh","iiiiiiiii"],"note":"` +
	strings.Repeat("the quick brown fox ", 40) + `","nested":{"k":[1,2,3],"s":"abc"}}`

// TestPinnedJSS4HoldsOneJSONDocument is the regression for a JSS4 archive of a
// single JSON document. Appendix E (and §5.2 of the 1.0.0 draft, where JSS4
// was first defined) gives every JSS4 archive the root length table, whatever
// content_fmt says; the encoder wrote the bare root instead, and its own
// decoder then read the root's first four bytes as a length and failed with
// "truncated archive: document 0 root".
func TestPinnedJSS4HoldsOneJSONDocument(t *testing.T) {
	for _, src := range []string{`["abc","defg"]`, pinDoc} {
		for _, sc := range []jssc.StructCodec{jssc.StructRaw, jssc.StructDeflate} {
			t.Run(fmt.Sprintf("%s/%.12s", sc, src), func(t *testing.T) {
				opt := jssc.Options{Version: 4, Structure: sc}
				archive, st := encode(t, []byte(src), opt)
				if got := string(archive[:4]); got != "JSS4" {
					t.Fatalf("magic %q, want JSS4", got)
				}
				a, err := jssc.OpenBytes(archive)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				if a.DocCount() != 1 || a.ContentFormat() != jssc.FormatJSON {
					t.Fatalf("doc_count %d, content_fmt %s; want 1, json", a.DocCount(), a.ContentFormat())
				}
				jsonEqual(t, []byte(src), decode(t, archive))

				// JSS4 is JSS3 plus the document table: the same pool, the same
				// walk aids, and a plain structure exactly four bytes longer.
				opt.Version = 3
				_, st3 := encode(t, []byte(src), opt)
				if st.StructPlain != st3.StructPlain+4 {
					t.Fatalf("JSS4 plain structure %d bytes, JSS3 %d; want a four-byte table more",
						st.StructPlain, st3.StructPlain)
				}
				// Stored raw, the structure is the archive's last section and
				// opens with root_len[0], the length of the one root after it.
				if st.StructCodec == jssc.StructRaw {
					s := archive[len(archive)-st.StructBytes:]
					if n := binary.LittleEndian.Uint32(s); int(n) != len(s)-4 {
						t.Fatalf("root_len[0] = %d, want %d", n, len(s)-4)
					}
				}
			})
		}
	}
}

// TestPinnedVersionsRoundTrip checks that every version a caller may pin
// writes an archive this package can read back, with the struct_layout that
// version's header can declare: none before JSS3, which has no byte for it.
func TestPinnedVersionsRoundTrip(t *testing.T) {
	ndjson := pinDoc + "\n" + `{"id":8,"tags":["x"],"note":"short"}` + "\n" + `[1,"two",{"three":3}]` + "\n"
	for _, sc := range []jssc.StructCodec{jssc.StructRaw, jssc.StructDeflate} {
		for v := 1; v <= jssc.LatestVersion; v++ {
			if v == 1 && sc != jssc.StructRaw {
				continue // JSS1 has no struct_codec; TestJSS1StoresTheStructureRaw
			}
			for _, src := range []string{`["abc","defg"]`, pinDoc} {
				t.Run(fmt.Sprintf("json/v%d/%s/%.12s", v, sc, src), func(t *testing.T) {
					archive, st := encode(t, []byte(src), jssc.Options{Version: v, Structure: sc})
					if want := fmt.Sprintf("JSS%d", v); string(archive[:4]) != want {
						t.Fatalf("magic %q, want %s", archive[:4], want)
					}
					if v < 3 && st.StructLayout != 0 {
						t.Fatalf("JSS%d reports struct_layout %v; it has no byte to declare one", v, st.StructLayout)
					}
					jsonEqual(t, []byte(src), decode(t, archive))
				})
			}
		}
		for v := 4; v <= jssc.LatestVersion; v++ {
			t.Run(fmt.Sprintf("ndjson/v%d/%s", v, sc), func(t *testing.T) {
				opt := jssc.Options{Version: v, Structure: sc, Format: jssc.FormatNDJSON}
				archive, _ := encode(t, []byte(ndjson), opt)
				want := strings.Split(strings.TrimSuffix(ndjson, "\n"), "\n")
				got := strings.Split(strings.TrimSuffix(string(decode(t, archive)), "\n"), "\n")
				if len(got) != len(want) {
					t.Fatalf("decoded %d documents, want %d", len(got), len(want))
				}
				for i := range want {
					jsonEqual(t, []byte(want[i]), []byte(got[i]))
				}
			})
		}
	}
}

// TestLineFormatsNeedJSS4 pins the other half of the root table rule. JSS1–JSS3
// headers carry neither content_fmt nor doc_count and hold exactly one document
// (Appendix E), so a line-oriented encode pinned to one of them has nowhere to
// say how many documents follow. It must be refused, not written as an archive
// that decodes to something else.
func TestLineFormatsNeedJSS4(t *testing.T) {
	src := "{\"a\":\"xyzzy\"}\n{\"a\":\"plugh\"}\n"
	for _, f := range []jssc.ContentFormat{jssc.FormatJSONL, jssc.FormatNDJSON} {
		for v := 1; v <= 3; v++ {
			opt := jssc.Options{Version: v, Format: f}
			if _, _, err := jssc.EncodeToBytes([]byte(src), opt); !errors.Is(err, jssc.ErrInvalidInput) {
				t.Errorf("EncodeToBytes %s at JSS%d: err = %v, want ErrInvalidInput", f, v, err)
			}
			var buf bytes.Buffer
			if _, err := jssc.EncodeFrom(strings.NewReader(src), &buf, opt); !errors.Is(err, jssc.ErrInvalidInput) {
				t.Errorf("EncodeFrom %s at JSS%d: err = %v, want ErrInvalidInput", f, v, err)
			}
		}
	}

	// Legacy4 takes precedence over Version and still writes JSS4 for lines.
	archive, _ := encode(t, []byte(src), jssc.Options{Version: 3, Legacy4: true, Format: jssc.FormatNDJSON})
	if got := string(archive[:4]); got != "JSS4" {
		t.Fatalf("Legacy4 over Version 3: magic %q, want JSS4", got)
	}
}

// TestPinnedJSS2MatchesGolden pins JSS2 by version rather than by LayoutLegacy
// and expects the other implementation's fixture back. JSS2 has no
// struct_layout byte, so a reader implies no walk aids (Appendix E); a pin that
// kept the default layout wrote length prefixes and offset tables the reader
// then parsed as values.
func TestPinnedJSS2MatchesGolden(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "golden", "users.jss2-deflate.jssc"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := encode(t, source, jssc.Options{Structure: jssc.StructDeflate, Algorithm: "fsst", Version: 2})
	compareReencode(t, got, want)
}

// TestJSS1IsJSS2WithoutStructCodec holds a JSS1 pin to Appendix E's
// definition of the version: JSS2 is JSS1 plus the struct_codec byte at offset
// 5, so with a raw structure the two archives differ in the magic and that
// byte and nothing else.
func TestJSS1IsJSS2WithoutStructCodec(t *testing.T) {
	for _, src := range []string{`["abc","defg"]`, pinDoc} {
		v1, _ := encode(t, []byte(src), jssc.Options{Version: 1})
		v2, _ := encode(t, []byte(src), jssc.Options{Version: 2, Structure: jssc.StructRaw})
		if v2[5] != byte(jssc.StructRaw) {
			t.Fatalf("JSS2 struct_codec %d, want raw", v2[5])
		}
		want := append([]byte("JSS1"), v2[4])
		want = append(want, v2[6:]...)
		if !bytes.Equal(v1, want) {
			t.Fatalf("%.12s: JSS1 archive is not JSS2 less its struct_codec byte\nJSS1 % x\nwant % x", src, v1, want)
		}
	}
}

// TestJSS1StoresTheStructureRaw: JSS1 has no struct_codec byte and a reader
// implies raw, so a compressed structure cannot be declared and is refused —
// as lz4 is below JSS8 — rather than written for a reader to misparse.
func TestJSS1StoresTheStructureRaw(t *testing.T) {
	for _, sc := range []jssc.StructCodec{jssc.StructDeflate, jssc.StructLZ4} {
		_, _, err := jssc.EncodeToBytes([]byte(pinDoc), jssc.Options{Version: 1, Structure: sc})
		if !errors.Is(err, jssc.ErrUnsupported) {
			t.Errorf("JSS1 with %s: err = %v, want ErrUnsupported", sc, err)
		}
	}
	// The shorthands take precedence over Version, and both name versions
	// that do carry struct_codec.
	for _, opt := range []jssc.Options{
		{Version: 1, Structure: jssc.StructDeflate, Layout: jssc.LayoutLegacy},
		{Version: 1, Structure: jssc.StructDeflate, Legacy4: true},
	} {
		archive, _ := encode(t, []byte(pinDoc), opt)
		jsonEqual(t, []byte(pinDoc), decode(t, archive))
	}
}

// TestPinnedStreamingMatchesInMemory extends the streaming invariant to pinned
// versions. JSS4 has no pool_group_shift field and implies one entry per group,
// so a requested group size has to be dropped there; the in-memory encoder did,
// and the streaming one wrote grouped framing the header could not declare.
func TestPinnedStreamingMatchesInMemory(t *testing.T) {
	src := strings.Repeat(`{"a":"xyzzy plugh","b":"lorem ipsum dolor"}`+"\n", 50)
	path := filepath.Join(t.TempDir(), "in.ndjson")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for v := 4; v <= jssc.LatestVersion; v++ {
		opt := jssc.Options{Format: jssc.FormatNDJSON, Version: v, PoolGroupShift: 3,
			Structure: jssc.StructDeflate, MaxDirectBytes: -1}
		mem, _ := encode(t, []byte(src), opt)
		var streamed bytes.Buffer
		if _, err := jssc.EncodeFile(path, &streamed, opt); err != nil {
			t.Fatalf("JSS%d: EncodeFile: %v", v, err)
		}
		if !bytes.Equal(streamed.Bytes(), mem) {
			t.Errorf("JSS%d: streaming archive (%d bytes) differs from in-memory (%d bytes)",
				v, streamed.Len(), len(mem))
		}
	}
}
