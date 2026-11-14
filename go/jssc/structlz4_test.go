package jssc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The lz4 structure codec (specification §5.2 value 2, §8A) and the
// intra-document cuts that arrived with it. The claims under test: the codec
// round-trips exactly, an incompressible block is stored verbatim and read
// back by its lengths alone, and a large single-document archive is no longer
// one block — a cold read decompresses a bounded number of blocks.

// canonical parses JSON text into a shape whose json.Marshal is stable, so two
// texts can be compared without caring about member order.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// bigRecordArray is a single JSON document — one array — whose structure spans
// several 64 KiB blocks.
func bigRecordArray(n int) []byte {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":%d,"email":"user%d@example.com","role":"%s","note":"note %d"}`,
			i, i, []string{"admin", "user", "guest"}[i%3], i)
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

func TestStructLZ4RoundTrip(t *testing.T) {
	users, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		opt  Options
	}{
		{"users", users, Options{Structure: StructLZ4}},
		{"big", bigRecordArray(4000), Options{Structure: StructLZ4}},
		{"big-single-stream", bigRecordArray(1000), Options{Structure: StructLZ4, StructBlockBytes: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			st, err := EncodeWith(tc.raw, &buf, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if st.StructCodec != StructLZ4 {
				t.Fatalf("stored codec %s, want lz4", st.StructCodec)
			}
			text, _, err := DecodeBytes(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if canonical(t, text) != canonical(t, tc.raw) {
				t.Fatal("lz4 archive did not restore the document")
			}

			// The selective walk over the lz4 structure agrees with the tree.
			a, err := OpenBytes(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "users" {
				vals, err := a.QueryValues("$[500].email")
				if err != nil {
					t.Fatal(err)
				}
				if len(vals) != 1 || vals[0] != "user500@example.com" {
					t.Fatalf("point query over lz4 structure: %v", vals)
				}
			}
		})
	}
}

// TestStructLZ4NDJSON holds the multi-document path to the same bar.
func TestStructLZ4NDJSON(t *testing.T) {
	src := ndjsonDocs(2000)
	a := encodeNDJSON(t, src, Options{Structure: StructLZ4})
	vals, err := a.QueryValues("$[1500].ctx.request_id")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || vals[0] != "req-00001500" {
		t.Fatalf("got %v", vals)
	}
	if got := a.structure.InflatedBlocks; got > 2 {
		t.Errorf("one document decompressed %d blocks, want at most 2", got)
	}
}

// xorshift fills a buffer with bytes lz4 cannot compress: no four-byte match
// recurs, so every block of it must be stored verbatim.
func xorshift(n int) []byte {
	out := make([]byte, n)
	state := uint64(0x9E3779B97F4A7C15)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = byte(state)
	}
	return out
}

func TestLZ4BlockVerbatim(t *testing.T) {
	noise := xorshift(8 << 10)
	stored, err := lz4Block(noise)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(noise) || !bytes.Equal(stored, noise) {
		t.Fatalf("incompressible block stored as %d bytes, want %d verbatim", len(stored), len(noise))
	}

	runs := bytes.Repeat([]byte("structure structure "), 1<<10)
	stored, err = lz4Block(runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) >= len(runs) {
		t.Fatalf("compressible block stored as %d bytes, plain is %d", len(stored), len(runs))
	}

	if stored, err = lz4Block(nil); err != nil || len(stored) != 0 {
		t.Fatalf("empty block: %d bytes, err %v", len(stored), err)
	}
}

// TestStructLZ4VerbatimBlockReads assembles a region by hand — one verbatim
// block, one compressed — and checks the reader tells them apart by lengths
// alone, across the boundary included.
func TestStructLZ4VerbatimBlockReads(t *testing.T) {
	verbatim := xorshift(4 << 10)
	compressible := bytes.Repeat([]byte("abcdabcd"), 512)
	plain := append(append([]byte{}, verbatim...), compressible...)

	storedA, err := lz4Block(verbatim)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedA) != len(verbatim) {
		t.Fatalf("noise block compressed to %d bytes; the test needs it verbatim", len(storedA))
	}
	storedB, err := lz4Block(compressible)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedB) >= len(compressible) {
		t.Fatal("run block did not compress; the test needs one of each")
	}

	var region bytes.Buffer
	var u [4]byte
	binary.LittleEndian.PutUint32(u[:], 2)
	region.Write(u[:])
	for _, b := range [][]byte{storedA, storedB} {
		binary.LittleEndian.PutUint32(u[:], uint32(len(b)))
		region.Write(u[:])
		var plainLen int
		if len(b) == len(storedA) && bytes.Equal(b, storedA) {
			plainLen = len(verbatim)
		} else {
			plainLen = len(compressible)
		}
		binary.LittleEndian.PutUint32(u[:], uint32(plainLen))
		region.Write(u[:])
	}
	region.Write(storedA)
	region.Write(storedB)

	sb, err := newStructBlocks(region.Bytes(), StructLZ4, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sb.at(0, len(plain))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("region did not read back")
	}
	// A read straddling the verbatim/compressed boundary.
	straddle, err := sb.at(len(verbatim)-8, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(straddle, plain[len(verbatim)-8:len(verbatim)+8]) {
		t.Fatal("straddling read did not assemble")
	}
}

// TestIntraDocumentCutsBoundColdReads is the claim the 5.1.0-draft cut rule
// exists for: a single-document archive spans blocks, and a point read
// decompresses a bounded few of them, not the structure.
func TestIntraDocumentCutsBoundColdReads(t *testing.T) {
	raw := bigRecordArray(6000)
	for _, sc := range []StructCodec{StructLZ4, StructDeflate} {
		t.Run(sc.String(), func(t *testing.T) {
			var buf bytes.Buffer
			st, err := EncodeWith(raw, &buf, Options{Structure: sc})
			if err != nil {
				t.Fatal(err)
			}
			if st.StructBlocks < 3 {
				t.Fatalf("single document came out as %d blocks; the corpus should span several", st.StructBlocks)
			}
			a, err := OpenBytes(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if a.structure.InflatedBlocks != 0 {
				t.Errorf("opening decompressed %d blocks, want 0", a.structure.InflatedBlocks)
			}
			vals, err := a.QueryValues("$[3000].email")
			if err != nil {
				t.Fatal(err)
			}
			if len(vals) != 1 || vals[0] != "user3000@example.com" {
				t.Fatalf("got %v", vals)
			}
			if got := a.structure.InflatedBlocks; got > 3 {
				t.Errorf("one point read decompressed %d of %d blocks", got, st.StructBlocks)
			}

			// And the whole document still restores across every cut.
			text, _, err := DecodeBytes(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if canonical(t, text) != canonical(t, raw) {
				t.Fatal("cut structure did not restore the document")
			}
		})
	}
}

func TestPlanStructBlocksSubdivision(t *testing.T) {
	k := 1 << 10
	cases := []struct {
		name             string
		plainLen, target int
		tableEnd         int
		docStarts        []int
		subdivide        bool
		want             []int
	}{
		{"fits", 60 * k, 64 * k, 0, nil, true, []int{0, 60 * k}},
		{"one-stream", 300 * k, 0, 0, nil, true, []int{0, 300 * k}},
		{"single-doc", 300 * k, 64 * k, 0, nil, true,
			[]int{0, 64 * k, 128 * k, 192 * k, 256 * k, 300 * k}},
		{"barely-over", 64*k + 1, 64 * k, 0, nil, true, []int{0, 64 * k, 64*k + 1}},
		{"oversized-doc-between-cuts", 400 * k, 64 * k, 16,
			[]int{16, 100 * k, 350 * k, 380 * k}, true,
			[]int{0, 64 * k, 100 * k, 164 * k, 228 * k, 292 * k, 350 * k, 400 * k}},
		// Raw structures are not subdivided: nothing is decompressed to read
		// them, so extra blocks would only deepen every read's block lookup.
		{"raw-single-doc-stays-whole", 300 * k, 64 * k, 0, nil, false,
			[]int{0, 300 * k}},
		{"raw-keeps-doc-boundary-cuts", 400 * k, 64 * k, 16,
			[]int{16, 100 * k, 350 * k, 380 * k}, false,
			[]int{0, 100 * k, 350 * k, 400 * k}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planStructBlocks(tc.plainLen, tc.target, tc.tableEnd, tc.docStarts, tc.subdivide)
			if !reflect.DeepEqual(got.cuts, tc.want) {
				t.Fatalf("cuts %v, want %v", got.cuts, tc.want)
			}
		})
	}
}
