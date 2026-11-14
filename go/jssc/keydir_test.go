package jssc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// The key hash directory only earns its bytes if it is exact. These tests check
// that it lists every key and nothing else, that lookups agree with a full pool
// search, and that a corrupt directory is rejected rather than trusted.

func openEncoded(t *testing.T, src string, opt Options) *Archive {
	t.Helper()
	var buf bytes.Buffer
	if _, err := EncodeWith([]byte(src), &buf, opt); err != nil {
		t.Fatal(err)
	}
	a, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestKeyDirectoryListsExactlyTheKeys(t *testing.T) {
	const src = `{"alpha":1,"beta":"alpha","gamma":{"delta":[{"epsilon":"zeta"}]}}`
	a := openEncoded(t, src, Options{Structure: StructDeflate})

	keys := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	if a.keyDir.len() != len(keys) {
		t.Fatalf("directory lists %d keys, want %d", a.keyDir.len(), len(keys))
	}
	for _, k := range keys {
		idx, ok := a.LookupKeyIndex(k)
		if !ok {
			t.Fatalf("key %q is missing from the directory", k)
		}
		got, err := a.keyString(idx)
		if err != nil {
			t.Fatal(err)
		}
		if got != k {
			t.Errorf("directory maps %q to key pool entry %q", k, got)
		}
	}

	// "zeta" is a value, never a key, so the directory must not list it even
	// though it is in the pool.
	if _, ok := a.LookupKeyIndex("zeta"); ok {
		t.Error(`"zeta" is a value, but the directory lists it as a key`)
	}
	if _, found, err := a.LookupIndex("zeta"); err != nil || !found {
		t.Errorf(`LookupIndex("zeta") = %v, %v; the pool holds it`, found, err)
	}
}

func TestKeyDirectoryAgreesWithPoolSearch(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{`)
	for i := range 300 {
		if i > 0 {
			sb.WriteByte(',')
		}
		// Long enough that every string is pooled rather than inlined.
		sb.WriteString(`"key_number_`)
		sb.WriteString(itoa(i))
		sb.WriteString(`":"value_number_`)
		sb.WriteString(itoa(i))
		sb.WriteString(`"`)
	}
	sb.WriteString(`}`)
	src := sb.String()

	withDir := openEncoded(t, src, Options{Structure: StructDeflate})
	if withDir.keyDir.len() != 300 {
		t.Fatalf("directory lists %d keys, want 300", withDir.keyDir.len())
	}

	plain := openEncoded(t, src, Options{Structure: StructDeflate, OmitKeyDirectory: true})
	if plain.keyDir.len() != 0 {
		t.Fatalf("OmitKeyDirectory still wrote %d entries", plain.keyDir.len())
	}

	for i := range 300 {
		k := "key_number_" + itoa(i)
		fast, okFast := withDir.LookupKeyIndex(k)
		slow, okSlow, err := plain.searchKeyPool(k)
		if err != nil {
			t.Fatal(err)
		}
		if !okFast || !okSlow || fast != slow {
			t.Fatalf("key %q: directory says %d/%v, pool search says %d/%v", k, fast, okFast, slow, okSlow)
		}
	}

	// A directory hit must cost nothing, which is the whole point.
	fresh := openEncoded(t, src, Options{Structure: StructDeflate})
	if _, ok := fresh.LookupKeyIndex("key_number_299"); !ok {
		t.Fatal("expected a hit")
	}
	if fresh.DecodedGroups != 0 {
		t.Errorf("a directory hit decompressed %d pool groups, want 0", fresh.DecodedGroups)
	}
	if _, ok := fresh.LookupKeyIndex("no_such_key_anywhere"); ok {
		t.Error("expected a miss")
	}
	if fresh.DecodedGroups != 0 {
		t.Errorf("a directory miss decompressed %d pool groups, want 0", fresh.DecodedGroups)
	}
}

// TestKeyDirectoryQueryCostsNoPoolWork is the property the section exists for:
// a field lookup resolves its key without decompressing anything, and a query
// for a key the document does not have touches the pool not at all.
func TestKeyDirectoryQueryCostsNoPoolWork(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"records":[`)
	for i := range 200 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"identifier":"record_value_`)
		sb.WriteString(itoa(i))
		sb.WriteString(`","measurement":`)
		sb.WriteString(itoa(i * 7))
		sb.WriteString(`}`)
	}
	sb.WriteString(`]}`)
	src := sb.String()

	a := openEncoded(t, src, Options{Structure: StructDeflate})
	vals, err := a.QueryValues("$.records[*].measurement")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 200 {
		t.Fatalf("got %d measurements, want 200", len(vals))
	}
	if a.DecodedGroups != 0 {
		t.Errorf("a numeric field query decompressed %d pool groups, want 0", a.DecodedGroups)
	}

	missing := openEncoded(t, src, Options{Structure: StructDeflate})
	vals, err = missing.QueryValues("$.records[*].no_such_field")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 0 {
		t.Fatalf("got %d values for an absent key", len(vals))
	}
	if missing.DecodedGroups != 0 {
		t.Errorf("querying an absent key decompressed %d pool groups, want 0", missing.DecodedGroups)
	}

	// Without the directory the same queries still answer correctly, just at
	// the cost of searching the pool.
	plain := openEncoded(t, src, Options{Structure: StructDeflate, OmitKeyDirectory: true})
	if got, err := plain.QueryValues("$.records[*].measurement"); err != nil || len(got) != 200 {
		t.Fatalf("without a directory: %d values, %v", len(got), err)
	}
}

func TestKeyDirectoryRejectsCorruptSections(t *testing.T) {
	var buf bytes.Buffer
	if _, err := EncodeWith([]byte(`{"alpha":1,"beta":2,"gamma":3}`), &buf, Options{Structure: StructDeflate}); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()

	// Locate the directory: it is the four-byte count whose value is the key
	// count, found by opening the archive and re-deriving the offset.
	a, err := OpenBytes(good)
	if err != nil {
		t.Fatal(err)
	}
	if a.keyDir.len() != 3 {
		t.Fatalf("expected 3 keys, got %d", a.keyDir.len())
	}
	dirStart := bytes.Index(good, keyDirBytesOf(a, good))
	if dirStart < 0 {
		t.Skip("could not locate the directory bytes; the layout changed")
	}

	cases := []struct {
		name string
		mut  func(b []byte) []byte
	}{
		{"count larger than the pool", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[dirStart:], 0xFFFF)
			return b
		}},
		{"count past the end of the archive", func(b []byte) []byte {
			// Every pool entry is a key here, so the largest count the pool
			// check allows still has to fit in the bytes that remain.
			binary.LittleEndian.PutUint32(b[dirStart:], uint32(a.poolCount))
			return b[:dirStart+4+(4+a.indexWidth)]
		}},
		{"entries out of hash order", func(b []byte) []byte {
			// Swap the first two hashes so the table is no longer sorted.
			stride := 4 + a.indexWidth
			e0 := dirStart + 4
			e1 := e0 + stride
			h0 := binary.LittleEndian.Uint32(b[e0:])
			h1 := binary.LittleEndian.Uint32(b[e1:])
			binary.LittleEndian.PutUint32(b[e0:], h1)
			binary.LittleEndian.PutUint32(b[e1:], h0)
			return b
		}},
		{"index past the pool", func(b []byte) []byte {
			e0 := dirStart + 4
			b[e0+4] = 0xFF
			return b
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := tc.mut(bytes.Clone(good))
			if _, err := OpenBytes(bad); err == nil {
				t.Error("Open accepted a corrupt key directory")
			}
		})
	}
}

// keyDirBytesOf rebuilds the directory section as it should appear on the wire,
// so a test can find it inside the archive without hard-coding an offset.
func keyDirBytesOf(a *Archive, _ []byte) []byte {
	out := make([]byte, 0, 4+a.keyDir.len()*(4+a.indexWidth))
	out = binary.LittleEndian.AppendUint32(out, uint32(a.keyDir.len()))
	for i := range a.keyDir.len() {
		out = binary.LittleEndian.AppendUint32(out, a.keyDir.hashAt(i))
		out = appendIndex(out, a.keyDir.indexAt(i), a.indexWidth)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
