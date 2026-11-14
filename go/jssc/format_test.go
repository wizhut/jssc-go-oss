package jssc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// These tests assert the bytes on the wire, not just that a round trip works:
// an archive this implementation writes has to be the archive the
// specification describes.

func TestHeaderJSS8(t *testing.T) {
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	archive, st := encode(t, raw, jssc.Options{Structure: jssc.StructDeflate})

	if got := string(archive[:4]); got != jssc.MagicV8 {
		t.Errorf("magic = %q, want %q", got, jssc.MagicV8)
	}
	if got := jssc.StructCodec(archive[5]); got != st.StructCodec {
		t.Errorf("struct_codec byte = %v, but stats report %v", got, st.StructCodec)
	}
	if got := jssc.Layout(archive[6]); got != jssc.LayoutDefault {
		t.Errorf("struct_layout = 0x%02x, want 0x%02x", uint8(got), uint8(jssc.LayoutDefault))
	}
	if archive[7] != 1 {
		t.Errorf("index_width = %d, want 1 for a small pool", archive[7])
	}
	// JSS5 and JSS6 carry content_fmt and doc_count even for a single document,
	// so one header shape serves both cases.
	if got := jssc.ContentFormat(archive[8]); got != jssc.FormatJSON {
		t.Errorf("content_fmt = %v, want json", got)
	}
	if got := 1 << archive[9]; got != st.PoolGroupSize {
		t.Errorf("pool_group_shift encodes %d, but stats report a group size of %d", got, st.PoolGroupSize)
	}
	keyCount := binary.LittleEndian.Uint32(archive[10:14])
	if int(keyCount) != st.DistinctKeys {
		t.Errorf("key_count = %d, but stats report %d distinct keys", keyCount, st.DistinctKeys)
	}
	poolCount := binary.LittleEndian.Uint32(archive[14:18])
	if int(poolCount) != st.UniqueStrings {
		t.Errorf("pool_count = %d, but stats report %d unique strings", poolCount, st.UniqueStrings)
	}
	if got := binary.LittleEndian.Uint32(archive[18:22]); got != 1 {
		t.Errorf("doc_count = %d, want 1", got)
	}
	if st.HeaderBytes != 22 {
		t.Errorf("HeaderBytes = %d, want 22", st.HeaderBytes)
	}
}

// TestHeaderLegacyVersions checks that the older container and pool encodings
// are still reachable, since they are what an older decoder can read.
func TestHeaderLegacyVersions(t *testing.T) {
	users, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile("testdata/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		raw        []byte
		opt        jssc.Options
		wantMagic  string
		wantHeader int
	}{
		{"jss3", users, jssc.Options{Legacy4: true, Algorithm: "fsst"}, jssc.MagicV3, 12},
		{"jss4", events, jssc.Options{Legacy4: true, Algorithm: "fsst", Format: jssc.FormatNDJSON}, jssc.MagicV4, 17},
		{"jss2", users, jssc.Options{Layout: jssc.LayoutLegacy, Algorithm: "fsst"}, jssc.MagicV2, 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive, st := encode(t, tc.raw, tc.opt)
			if got := string(archive[:4]); got != tc.wantMagic {
				t.Errorf("magic = %q, want %q", got, tc.wantMagic)
			}
			if st.HeaderBytes != tc.wantHeader {
				t.Errorf("HeaderBytes = %d, want %d", st.HeaderBytes, tc.wantHeader)
			}
			if st.PoolGroupSize != 1 {
				t.Errorf("PoolGroupSize = %d; versions before JSS5 frame one entry per group", st.PoolGroupSize)
			}
			text := decode(t, archive)
			if tc.opt.Format.LineOriented() {
				return // compared line by line elsewhere
			}
			jsonEqual(t, tc.raw, text)
		})
	}
}

func TestHeaderJSS2Legacy(t *testing.T) {
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	archive, st := encode(t, raw, jssc.Options{Layout: jssc.LayoutLegacy, Algorithm: "fsst"})

	if got := string(archive[:4]); got != jssc.MagicV2 {
		t.Fatalf("magic = %q, want %q", got, jssc.MagicV2)
	}
	if st.HeaderBytes != 11 {
		t.Errorf("HeaderBytes = %d, want 11", st.HeaderBytes)
	}
	// JSS2 has no struct_layout byte, so the archive carries no walk aids.
	if st.StructLayout != 0 {
		t.Errorf("StructLayout = 0x%02x, want 0 for JSS2", uint8(st.StructLayout))
	}
	jsonEqual(t, raw, decode(t, archive))
}

func TestIndexWidthGrowsWithPool(t *testing.T) {
	cases := []struct {
		name    string
		strings int
		want    int
	}{
		{"tiny pool", 10, 1},
		{"at the 1-byte boundary", 256, 1},
		{"just past it", 300, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Long distinct values, so none of them is written inline.
			var b bytes.Buffer
			b.WriteByte('[')
			for i := range tc.strings {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(`"value-` + strings.Repeat("x", 20) + "-" + itoa(i) + `"`)
			}
			b.WriteByte(']')

			archive, st := encode(t, b.Bytes(), jssc.Options{})
			if st.IndexWidth != tc.want {
				t.Errorf("IndexWidth = %d, want %d (pool holds %d)", st.IndexWidth, tc.want, st.UniqueStrings)
			}
			if int(archive[7]) != st.IndexWidth {
				t.Errorf("header index_width = %d, but stats report %d", archive[7], st.IndexWidth)
			}
			jsonEqual(t, b.Bytes(), decode(t, archive))
		})
	}
}

func TestShortStringValuesAreInlined(t *testing.T) {
	// With a 1-byte index, a value of 1 byte or less costs no more inline than
	// as a reference, so it must stay out of the pool (specification §8.6).
	const raw = `{"key":"a","other":"bb"}`
	_, st := encode(t, []byte(raw), jssc.Options{})

	if st.IndexWidth != 1 {
		t.Fatalf("IndexWidth = %d, want 1", st.IndexWidth)
	}
	if st.InlineRefs != 1 {
		t.Errorf("InlineRefs = %d, want 1 (only \"a\" fits inline)", st.InlineRefs)
	}
	if st.InlineStrings != 1 {
		t.Errorf("InlineStrings = %d, want 1", st.InlineStrings)
	}
	// The pooled value "bb" in the value pool; the two keys in their own.
	if st.UniqueStrings != 1 {
		t.Errorf("UniqueStrings = %d, want 1 (the pooled value)", st.UniqueStrings)
	}
	if st.DistinctKeys != 2 {
		t.Errorf("DistinctKeys = %d, want 2", st.DistinctKeys)
	}
}

func TestObjectKeysAreAlwaysPooled(t *testing.T) {
	// A one-byte key could be inlined if it were a value, but keys are bare
	// references with no tag, so they are always pooled.
	const raw = `{"a":"a"}`
	archive, st := encode(t, []byte(raw), jssc.Options{})
	if st.DistinctKeys != 1 {
		t.Errorf("DistinctKeys = %d, want 1 (the key)", st.DistinctKeys)
	}
	if st.UniqueStrings != 0 {
		t.Errorf("UniqueStrings = %d, want 0 (the value is inline)", st.UniqueStrings)
	}
	if st.InlineRefs != 1 {
		t.Errorf("InlineRefs = %d, want 1 (the value)", st.InlineRefs)
	}
	jsonEqual(t, []byte(raw), decode(t, archive))
}

// TestOffsetTablesEarnTheirBytes checks the JSS5 rule: a table needs both
// enough children to index and enough payload for the linear skip it replaces
// to be worth avoiding.
func TestOffsetTablesEarnTheirBytes(t *testing.T) {
	// Many children, tiny payload: exactly the case that used to pay 4 bytes per
	// child to index single-digit numbers.
	var small bytes.Buffer
	small.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			small.WriteByte(',')
		}
		small.WriteByte('1')
	}
	small.WriteByte(']')

	_, st := encode(t, small.Bytes(), jssc.Options{Structure: jssc.StructRaw})
	if st.OffsetTables != 0 {
		t.Errorf("a 32-element array of single digits got %d bytes of offset table; "+
			"its whole payload is under the threshold", st.OffsetTables)
	}

	// Many children, large payload: worth indexing. Note what makes a payload
	// large — long strings live in the pool and cost three bytes here, so it
	// takes many *structural* children to move the needle.
	var big bytes.Buffer
	big.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			big.WriteByte(',')
		}
		big.WriteByte('[')
		for k := range 60 {
			if k > 0 {
				big.WriteByte(',')
			}
			big.WriteString(itoa(i*60 + k))
		}
		big.WriteByte(']')
	}
	big.WriteByte(']')

	_, st = encode(t, big.Bytes(), jssc.Options{Structure: jssc.StructRaw})
	if st.OffsetTables == 0 {
		t.Error("a 32-element array spanning kilobytes got no offset table")
	}
	// Its children span well under 65,536 bytes, so one or two bytes per entry
	// is enough — the point of the width descriptor.
	if perEntry := st.OffsetTables / 32; perEntry > 2 {
		t.Errorf("offset entries cost %d bytes each; the span needs at most 2", perEntry)
	}

	// Legacy versions keep the old flat rule.
	_, st = encode(t, small.Bytes(), jssc.Options{Structure: jssc.StructRaw, Legacy4: true})
	if st.OffsetTables != 4*32 {
		t.Errorf("JSS4 offset tables = %d bytes, want %d; the old rule is count alone",
			st.OffsetTables, 4*32)
	}
}

func TestDeflateFallsBackToRaw(t *testing.T) {
	// A tiny structure does not compress, so the encoder must store it plain
	// and say so in the header (specification §5.2).
	archive, st := encode(t, []byte(`1`), jssc.Options{Structure: jssc.StructDeflate})
	if st.StructCodec != jssc.StructRaw {
		t.Errorf("StructCodec = %v, want raw after the deflate fallback", st.StructCodec)
	}
	if jssc.StructCodec(archive[5]) != jssc.StructRaw {
		t.Errorf("header struct_codec = %d, want 0; the header must describe the stored bytes", archive[5])
	}
	// From JSS7 the structure region carries a block index, so a raw structure
	// costs its plain size plus that framing rather than exactly its plain size.
	framing := 4 + 8*st.StructBlocks
	if st.StructBytes != st.StructPlain+framing {
		t.Errorf("StructBytes = %d, want StructPlain %d + %d bytes of block index",
			st.StructBytes, st.StructPlain, framing)
	}
}

func TestEmptyPool(t *testing.T) {
	// Numbers, booleans, and null are never pooled, so this archive has no pool
	// at all — still a valid archive (specification §8.4).
	const raw = `[1,2,true,false,null]`
	archive, st := encode(t, []byte(raw), jssc.Options{})
	if st.UniqueStrings != 0 {
		t.Errorf("UniqueStrings = %d, want 0", st.UniqueStrings)
	}
	jsonEqual(t, []byte(raw), decode(t, archive))
}

func TestStatsAccountForEveryByte(t *testing.T) {
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	archive, st := encode(t, raw, jssc.Options{Structure: jssc.StructDeflate})
	if st.TotalBytes != len(archive) {
		t.Errorf("TotalBytes = %d, archive is %d bytes", st.TotalBytes, len(archive))
	}
	sum := st.HeaderBytes + st.StateBytes + st.PoolFraming + st.PoolBytes +
		st.KeyDirBytes + 4 + st.StructBytes
	if sum != len(archive) {
		t.Errorf("header+state+pool+keydir+structure = %d, archive is %d bytes", sum, len(archive))
	}
}

func TestIsArchive(t *testing.T) {
	archive, _ := encode(t, []byte(`{"a":1}`), jssc.Options{})
	if !jssc.IsArchive(archive) {
		t.Error("IsArchive said no to an archive we just wrote")
	}
	for _, notArchive := range []string{"", "JSS", `{"a":1}`, "JSS9xxxx"} {
		if jssc.IsArchive([]byte(notArchive)) {
			t.Errorf("IsArchive(%q) = true", notArchive)
		}
	}
}

func TestParseHelpers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want jssc.ContentFormat
	}{{"", jssc.FormatJSON}, {"json", jssc.FormatJSON}, {"JSONL", jssc.FormatJSONL}, {" ndjson ", jssc.FormatNDJSON}} {
		got, err := jssc.ParseContentFormat(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseContentFormat(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := jssc.ParseContentFormat("yaml"); !errors.Is(err, jssc.ErrUnsupported) {
		t.Errorf("ParseContentFormat(yaml) error = %v, want ErrUnsupported", err)
	}

	for _, tc := range []struct {
		in   string
		want jssc.StructCodec
	}{{"", jssc.StructLZ4}, {"lz4", jssc.StructLZ4}, {"deflate", jssc.StructDeflate}, {"raw", jssc.StructRaw}, {"none", jssc.StructRaw}} {
		got, err := jssc.ParseStructCodec(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseStructCodec(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := jssc.ParseStructCodec("zstd"); !errors.Is(err, jssc.ErrUnsupported) {
		t.Errorf("ParseStructCodec(zstd) error = %v, want ErrUnsupported", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
