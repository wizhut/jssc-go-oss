package jssc

import (
	"bytes"
	"testing"
)

// Guards the specification against drift. Every numeric constant the document
// states appears here; a change to either side fails this rather than being
// discovered by a reader.
//
// The specification is normative for JSS8 as of 5.0.0-draft, with the seven
// superseded container versions defined in its Appendix E. The constants
// asserted here are the current ones; the appendix's own figures are guarded by
// TestSpecAppendixE* below, since the golden fixtures no longer cover JSS5–JSS7.
func TestSpecificationConstants(t *testing.T) {
	cases := []struct {
		spec string
		got  any
		want any
	}{
		{"§4.1 header size", headerSizeV8, 22},
		{"§5.5 key width at 0 keys", keyWidthFor(0), 1},
		{"§5.5 key width at 256 keys", keyWidthFor(256), 1},
		{"§5.5 key width at 257 keys", keyWidthFor(257), 2},
		{"§4.1 magic", MagicV8, "JSS8"},
		{"§4.1 latest version", LatestVersion, 8},
		{"§8A.5 default struct block", DefaultStructBlockBytes, 64 << 10},
		{"§8A.5 min struct block", MinStructBlockBytes, 4 << 10},
		{"§7A.3 key hash", KeyHashCRC32, "crc32-ieee"},
		{"§7A.3 key hash of \"id\"", keyHash("id"), uint32(0xBF396750)},
		{"§7A.2 key directory entry stride at width 2", 4 + 2, 6},
		{"§5.3 LayoutLenPrefix", uint8(LayoutLenPrefix), uint8(0x01)},
		{"§5.3 LayoutOffsets", uint8(LayoutOffsets), uint8(0x02)},
		{"§5.3 LayoutPathFilter", uint8(LayoutPathFilter), uint8(0x04)},
		{"§5.3 LayoutLegacy", uint8(LayoutLegacy), uint8(0x80)},
		{"§5.5 index width at 256", indexWidthFor(256), 1},
		{"§5.5 index width at 257", indexWidthFor(257), 2},
		{"§5.5 index width at 65536", indexWidthFor(65536), 2},
		{"§5.5 index width at 65537", indexWidthFor(65537), 4},
		{"§5.6 max pool group shift", MaxPoolGroupShift, 12},
		{"§5.6 default group size", 1 << DefaultPoolGroupShift, 1},
		{"§7.4 group directory segment S", segmentGroups, 64},
		{"§7.4 group directory entries at 64 groups", dirEntries(64), 0},
		{"§7.4 group directory entries at 65 groups", dirEntries(65), 1},
		{"§7.4 group directory entries at 129 groups", dirEntries(129), 2},
		{"§5.2 struct codec raw", int(StructRaw), 0},
		{"§5.2 struct codec deflate", int(StructDeflate), 1},
		{"§5.2 struct codec lz4", int(StructLZ4), 2},
		{"§5.2 the default an encoder writes", defaultStructCodec(), StructLZ4},
		{"§8.2 tag null", tagNull, 0x00},
		{"§8.2 tag false", tagFalse, 0x01},
		{"§8.2 tag true", tagTrue, 0x02},
		{"§8.2 tag number", tagNumber, 0x03},
		{"§8.2 tag string", tagString, 0x04},
		{"§8.2 tag array", tagArray, 0x05},
		{"§8.2 tag object", tagObject, 0x06},
		{"§8.2 inline tag base", tagInline0, 0x07},
		{"§8.2 inline tag max", tagInlineMax, 0x0B},
		{"§8.4 min offset children", MinOffsetChildren, 8},
		{"§8.4 min offset payload", MinOffsetPayload, 2048},
		{"§8.4 offset width at 255", offsetWidthFor(255), 1},
		{"§8.4 offset width at 256", offsetWidthFor(256), 2},
		{"§8.4 offset width at 65536", offsetWidthFor(65536), 4},
		{"§8.6 max index width", MaxIndexWidth, 4},
		{"§8.7 max number lexeme", MaxNumberLexeme, 255},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: implementation has %v, specification says %v", c.spec, c.got, c.want)
		}
	}
}

// Checks the header layouts documented in specification Appendix E.2.
// defaultStructCodec resolves what the empty codec name selects — §5.2's
// "the default an encoder writes".
func defaultStructCodec() StructCodec {
	sc, err := ParseStructCodec("")
	if err != nil {
		return 255
	}
	return sc
}

// TestSpecWorkedExample pins the byte-annotated archive of appendix B: the
// document, options, and offsets the specification walks through must be the
// archive this encoder writes, or the appendix is fiction. The assertions are
// the appendix's own landmarks — total size, the pool section length
// (§7.4), the stored-raw flag byte, and struct_len — so a layout
// change that forgets the appendix fails here by name.
func TestSpecWorkedExample(t *testing.T) {
	var buf bytes.Buffer
	if _, err := EncodeWith(
		[]byte(`{"id":7,"name":"Ada","tags":["x","yy"],"ok":true}`),
		&buf, Options{Structure: StructRaw}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()

	if len(b) != 161 {
		t.Fatalf("appendix B says 161 bytes, encoder wrote %d", len(b))
	}
	checks := []struct {
		spec string
		at   int
		want []byte
	}{
		{"§B header magic", 0, []byte("JSS8")},
		{"§B key_count = 4 at offset 10", 10, []byte{0x04, 0x00, 0x00, 0x00}},
		{"§B pool_count = 2 at offset 14", 14, []byte{0x02, 0x00, 0x00, 0x00}},
		{"§B key pool_len = 17 at offset 51", 51, []byte{0x11, 0x00, 0x00, 0x00}},
		{"§B key stored-raw flags at offset 55", 55, []byte{0x0f}},
		{"§B key group 0 is length 2, id", 56, []byte{0x02, 'i', 'd'}},
		{"§B value pool_len = 8 at offset 72", 72, []byte{0x08, 0x00, 0x00, 0x00}},
		{"§B value group 0 is length 3, Ada", 77, []byte{0x03, 'A', 'd', 'a'}},
		{"§B dir_count = 4 at offset 84", 84, []byte{0x04, 0x00, 0x00, 0x00}},
		{"§B hash of id maps to key 0, at offset 103", 103, []byte{0x50, 0x67, 0x39, 0xbf, 0x00}},
		{"§B struct_len = 49 at offset 108", 108, []byte{0x31, 0x00, 0x00, 0x00}},
		{"§B block_count = 1 at offset 112", 112, []byte{0x01, 0x00, 0x00, 0x00}},
	}
	for _, c := range checks {
		if got := b[c.at : c.at+len(c.want)]; !bytes.Equal(got, c.want) {
			t.Errorf("%s: got % x, appendix says % x", c.spec, got, c.want)
		}
	}
}

func TestSpecAppendixEHeaderSizes(t *testing.T) {
	want := map[int]int{1: 10, 2: 11, 3: 12, 4: 17, 5: 18, 6: 18, 7: 18, 8: 22}
	for v, size := range want {
		h := header{version: v, indexWidth: 1, docCount: 1}
		if got := h.size(); got != size {
			t.Errorf("JSS%d header size = %d, appendix says %d", v, got, size)
		}
		if got := len(h.marshal()); got != size {
			t.Errorf("JSS%d marshalled to %d bytes, size() says %d", v, got, size)
		}
	}
}

// Checks the per-version feature table in Appendix E.1.
func TestSpecAppendixEFeatureTable(t *testing.T) {
	for v := 1; v <= 8; v++ {
		h := header{version: v}
		check := func(name string, got, want bool) {
			if got != want {
				t.Errorf("JSS%d %s = %v, appendix says %v", v, name, got, want)
			}
		}
		check("varintPool", h.varintPool(), v >= 5)
		check("adaptiveOffsets", h.adaptiveOffsets(), v >= 5)
		check("keyDir", h.keyDir(), v >= 6)
		check("blockedStructure", h.blockedStructure(), v >= 7)
		check("rootOffsets", h.rootOffsets(), v >= 7)
		check("opportunisticPool", h.opportunisticPool(), v >= 8)
		check("completeKeyDirectory", h.completeKeyDirectory(), v >= 8)
	}
}

// Appendix E.6: pre-JSS5 offset tables are uint32 and use child count alone.
func TestSpecAppendixEOffsetTables(t *testing.T) {
	legacy := &structWriter{layout: LayoutOffsets, adaptiveOffsets: false}
	if w := legacy.offsetWidth(MinOffsetChildren, 1); w != 4 {
		t.Errorf("pre-JSS5 table width = %d, appendix says uint32", w)
	}
	if w := legacy.offsetWidth(MinOffsetChildren-1, 1<<20); w != offsetsNone {
		t.Errorf("pre-JSS5 emitted a table below %d children", MinOffsetChildren)
	}
	modern := &structWriter{layout: LayoutOffsets, adaptiveOffsets: true}
	if w := modern.offsetWidth(MinOffsetChildren, MinOffsetPayload-1); w != offsetsNone {
		t.Errorf("JSS5+ emitted a table below the %d-byte payload floor", MinOffsetPayload)
	}
}
