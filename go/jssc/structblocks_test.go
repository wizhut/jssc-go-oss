package jssc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The blocked structure exists for one property: reaching a document inflates
// one block, not the archive. These tests check that it holds, that it holds no
// matter where the block boundaries fall, and that a corrupt block index is
// rejected rather than trusted.

// ndjsonDocs builds n log-shaped documents, large enough to span many blocks.
func ndjsonDocs(n int) []byte {
	var sb strings.Builder
	for i := range n {
		fmt.Fprintf(&sb, `{"ts":"2026-08-09T00:00:%02d.%03dZ","seq":%d,"service":"svc-%d",`+
			`"message":"request %d completed","ctx":{"request_id":"req-%08d","retries":%d}}`+"\n",
			i%60, i%1000, i, i%7, i, i, i%3)
	}
	return []byte(sb.String())
}

func encodeNDJSON(t *testing.T, src []byte, opt Options) *Archive {
	t.Helper()
	opt.Format = FormatNDJSON
	var buf bytes.Buffer
	if _, err := EncodeWith(src, &buf, opt); err != nil {
		t.Fatal(err)
	}
	a, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStructBlocksReadOneBlockPerDocument(t *testing.T) {
	src := ndjsonDocs(4000)
	a := encodeNDJSON(t, src, Options{Structure: StructDeflate})
	if a.structure.len() == 0 {
		t.Fatal("empty structure")
	}
	blocks := len(a.structure.blocks)
	if blocks < 3 {
		t.Fatalf("structure came out as %d blocks; the corpus is meant to span several", blocks)
	}

	// Opening must not inflate anything at all.
	if a.structure.InflatedBlocks != 0 {
		t.Errorf("opening inflated %d blocks, want 0", a.structure.InflatedBlocks)
	}

	// One document in the middle: at most the block holding the root offset
	// table entry and the block holding the document itself.
	vals, err := a.QueryValues("$[2000].ctx.request_id")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || vals[0] != "req-00002000" {
		t.Fatalf("got %v", vals)
	}
	if got := a.structure.InflatedBlocks; got > 2 {
		t.Errorf("one document inflated %d of %d blocks, want at most 2", got, blocks)
	}
}

func TestStructBlocksRoundTripAtEveryBlockSize(t *testing.T) {
	src := ndjsonDocs(1500)
	want, err := DecodeBytesTo(t, src)
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []int{-1, 1, 4 << 10, 16 << 10, 64 << 10, 1 << 20} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			a := encodeNDJSON(t, src, Options{Structure: StructDeflate, StructBlockBytes: target})
			got, err := a.DecodeAll()
			if err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("block target %d did not round trip", target)
			}

			// Every document must be reachable directly, whatever the cuts.
			for _, i := range []int{0, 1, 749, 1498, 1499} {
				v, err := a.Doc(i)
				if err != nil {
					t.Fatalf("doc %d: %v", i, err)
				}
				m, ok := v.(map[string]any)
				if !ok {
					t.Fatalf("doc %d is %T", i, v)
				}
				if got := m["seq"]; fmt.Sprint(got) != fmt.Sprint(i) {
					t.Errorf("doc %d has seq %v", i, got)
				}
			}
		})
	}
}

// DecodeBytesTo materializes the reference document set the same way the
// archive should.
func DecodeBytesTo(t *testing.T, src []byte) (any, error) {
	t.Helper()
	var out []any
	for _, line := range bytes.Split(bytes.TrimSpace(src), []byte("\n")) {
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// TestStructBlocksScaleWithBlockNotArchive is the claim the format change was
// made for: the work of one read tracks the block, not the document count.
func TestStructBlocksScaleWithBlockNotArchive(t *testing.T) {
	small := encodeNDJSON(t, ndjsonDocs(1000), Options{Structure: StructDeflate})
	large := encodeNDJSON(t, ndjsonDocs(16000), Options{Structure: StructDeflate})

	if len(large.structure.blocks) <= len(small.structure.blocks) {
		t.Fatalf("16x the documents produced %d blocks against %d",
			len(large.structure.blocks), len(small.structure.blocks))
	}
	for _, tc := range []struct {
		name string
		a    *Archive
		path string
	}{
		{"1k", small, "$[500].seq"},
		{"16k", large, "$[8000].seq"},
	} {
		if _, err := tc.a.QueryValues(tc.path); err != nil {
			t.Fatal(err)
		}
		if got := tc.a.structure.InflatedBlocks; got > 2 {
			t.Errorf("%s: one read inflated %d blocks", tc.name, got)
		}
	}
}

func TestStructBlocksRejectCorruptIndex(t *testing.T) {
	var buf bytes.Buffer
	st, err := EncodeWith(ndjsonDocs(600), &buf, Options{
		Structure: StructDeflate, Format: FormatNDJSON, StructBlockBytes: 4 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()
	if st.StructBlocks < 3 {
		t.Fatalf("wanted several blocks, got %d", st.StructBlocks)
	}
	// The structure region is the tail of the archive.
	regionStart := len(good) - st.StructBytes

	cases := []struct {
		name string
		mut  func(b []byte) []byte
	}{
		{"block count larger than the region", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[regionStart:], 0x0FFFFFFF)
			return b
		}},
		{"stored length past the region", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[regionStart+4:], uint32(len(b)))
			return b
		}},
		{"plain length disagrees with the block", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[regionStart+8:], 7)
			return b
		}},
		{"region truncated mid-index", func(b []byte) []byte {
			return b[:regionStart+6]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := tc.mut(bytes.Clone(good))
			a, err := OpenBytes(bad)
			if err != nil {
				return // rejected at open, which is the best outcome
			}
			// A bad plain length is only detectable when the block is inflated,
			// which is the point of inflating lazily — it must still fail
			// cleanly rather than return wrong bytes.
			if _, err := a.DecodeAll(); err == nil {
				t.Error("a corrupt block index was accepted")
			}
		})
	}
}

// TestStructBlockBytesIsAKnobNotAFormatChange checks that block size changes
// only the framing, never the document set.
func TestStructBlockBytesIsAKnobNotAFormatChange(t *testing.T) {
	src := ndjsonDocs(900)
	one := encodeNDJSON(t, src, Options{Structure: StructDeflate, StructBlockBytes: -1})
	many := encodeNDJSON(t, src, Options{Structure: StructDeflate, StructBlockBytes: 4 << 10})
	if len(one.structure.blocks) != 1 {
		t.Errorf("StructBlockBytes -1 produced %d blocks", len(one.structure.blocks))
	}
	if len(many.structure.blocks) < 3 {
		t.Errorf("a 4 KiB target produced %d blocks", len(many.structure.blocks))
	}
	a, _ := one.DecodeAll()
	b, _ := many.DecodeAll()
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if !bytes.Equal(aj, bj) {
		t.Error("block size changed the decoded document set")
	}
}
