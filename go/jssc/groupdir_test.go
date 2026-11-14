package jssc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The JSS8 group directory (specification §7.4, 5.6.0-draft): one
// uint32 per 64 groups after the first, between the stored-raw flags and the
// groups, so a reader reaches any group by walking one segment.

// stringArray is a JSON array of n distinct strings that sort in index order,
// so value-pool entry i is the string at position i.
func stringArray(n int) []byte {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"value-%05d"`, i)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func encodeForTest(t *testing.T, src []byte, opt Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := EncodeWith(src, &buf, opt); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The layout is checked against the specification's formula by walking the
// archive's own bytes, not through the reader: open is used only to find where
// the value pool section starts.
func TestGroupDirectoryLayout(t *testing.T) {
	for _, tc := range []struct{ groups, entries int }{
		{1, 0}, {63, 0}, {64, 0}, {65, 1}, {128, 1}, {129, 2}, {3200, 49},
	} {
		t.Run(fmt.Sprint(tc.groups), func(t *testing.T) {
			archive := encodeForTest(t, stringArray(tc.groups), Options{})
			a, err := OpenBytes(archive)
			if err != nil {
				t.Fatal(err)
			}
			if a.poolCount != tc.groups {
				t.Fatalf("pool holds %d entries, want %d", a.poolCount, tc.groups)
			}
			start := a.vals.flags
			plen := int(binary.LittleEndian.Uint32(archive[start-4:]))
			dir := start + rawFlagBytes(tc.groups)
			framed := dir + 4*tc.entries

			pos := framed
			for g := range tc.groups {
				if g > 0 && g%64 == 0 {
					entry := int(binary.LittleEndian.Uint32(archive[dir+4*(g/64-1):]))
					if entry != pos-framed {
						t.Fatalf("directory entry %d = %d, group %d starts at %d",
							g/64-1, entry, g, pos-framed)
					}
				}
				n, size := binary.Uvarint(archive[pos:])
				if size <= 0 {
					t.Fatalf("group %d length unreadable at %d", g, pos)
				}
				pos += size + int(n)
			}
			if pos != start+plen {
				t.Fatalf("groups end at %d, pool_len says %d: the directory is not %d entries",
					pos-start, plen, tc.entries)
			}
		})
	}
}

// Reading one string walks one segment's lengths and decodes one group,
// wherever the string is in the pool.
func TestGroupDirectoryReadsOneSegment(t *testing.T) {
	archive := encodeForTest(t, stringArray(3200), Options{})
	a, err := OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.QueryValues("$[2000]")
	if err != nil || len(got) != 1 || got[0] != "value-02000" {
		t.Fatalf("got %v, %v", got, err)
	}
	walked := 0
	for k, s := range a.vals.segs {
		if s != nil {
			walked++
			if k != 2000/64 {
				t.Errorf("walked segment %d to read entry 2000", k)
			}
		}
	}
	if walked != 1 || a.DecodedGroups != 1 {
		t.Errorf("walked %d segments and decoded %d groups to read one string", walked, a.DecodedGroups)
	}
}

// A directory entry that disagrees with the framing fails the read of the
// segment it closes, and a full decode, instead of returning the wrong string.
func TestGroupDirectoryRejectsABadEntry(t *testing.T) {
	archive := encodeForTest(t, stringArray(3200), Options{})
	a, err := OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}
	const k = 31 // the entry for b_31, which closes segment 30
	at := a.vals.dir + 4*(k-1)
	honest := binary.LittleEndian.Uint32(archive[at:])

	for name, v := range map[string]uint32{
		"moved a byte":   honest + 1,
		"moved back":     honest - 1,
		"past the end":   0xFFFFFFFF,
		"before b_(k-1)": 0,
	} {
		t.Run(name, func(t *testing.T) {
			bad := bytes.Clone(archive)
			binary.LittleEndian.PutUint32(bad[at:], v)
			b, err := OpenBytes(bad)
			if err != nil {
				return // rejecting at open is also a refusal
			}
			if got, err := b.QueryValues("$[1950]"); err == nil {
				t.Fatalf("read in segment %d returned %v", 1950/64, got)
			}
			c, err := OpenBytes(bad)
			if err != nil {
				return
			}
			if _, err := c.Doc(0); err == nil {
				t.Fatal("full decode accepted the archive")
			}
		})
	}
}

// Every path that reads every string — warming, materializing — agrees with
// the source across many segments, as a single document and as NDJSON read
// back as the virtual root array.
func TestGroupDirectoryFullDecode(t *testing.T) {
	src := stringArray(3200)
	var lines bytes.Buffer
	for i := range 3200 {
		fmt.Fprintf(&lines, "\"value-%05d\"\n", i)
	}
	var want any
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{"json", encodeForTest(t, src, Options{})},
		{"ndjson", encodeForTest(t, lines.Bytes(), Options{Format: FormatNDJSON, Structure: StructDeflate})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := tc.archive
			a, err := OpenBytes(archive)
			if err != nil {
				t.Fatal(err)
			}
			// A point read first, so it finds its segment through the
			// directory before anything else has walked one.
			if got, err := a.QueryValues("$[3199]"); err != nil || len(got) != 1 || got[0] != "value-03199" {
				t.Fatalf("got %v, %v", got, err)
			}
			if err := a.WarmPool(); err != nil {
				t.Fatal(err)
			}
			for i := range 3200 {
				s, err := a.String(i)
				if err != nil || s != fmt.Sprintf("value-%05d", i) {
					t.Fatalf("entry %d = %q, %v", i, s, err)
				}
			}
			b, err := OpenBytes(archive)
			if err != nil {
				t.Fatal(err)
			}
			var got any
			if b.MultiDoc() {
				docs := make([]any, b.DocCount())
				for i := range docs {
					if docs[i], err = b.Doc(i); err != nil {
						t.Fatal(err)
					}
				}
				got = docs
			} else if got, err = b.Doc(0); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("materialized document differs from the source")
			}
		})
	}
}

// The key pool has a directory of its own, read the same way.
func TestGroupDirectoryInTheKeyPool(t *testing.T) {
	var b strings.Builder
	b.WriteByte('{')
	for i := range 100 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"key-%03d":%d`, i, i)
	}
	b.WriteByte('}')
	src := []byte(b.String())

	a, err := OpenBytes(encodeForTest(t, src, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if a.keys == a.vals || a.keys.dir < 0 || a.keys.groups != 100 {
		t.Fatalf("key pool: separate %v, directory at %d, %d groups",
			a.keys != a.vals, a.keys.dir, a.keys.groups)
	}
	k, err := a.keyString(99)
	if err != nil || k != "key-099" {
		t.Fatalf("key 99 = %q, %v", k, err)
	}
	if a.keys.segs[0] != nil || a.keys.segs[1] == nil {
		t.Error("reading key 99 did not walk exactly its own segment")
	}
	// Doc keeps number lexemes, so compare canonical JSON rather than values.
	var want any
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	got, err := a.Doc(0)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if !bytes.Equal(g, w) {
		t.Fatalf("materialized document differs from the source:\n%s\n%s", g, w)
	}
}
