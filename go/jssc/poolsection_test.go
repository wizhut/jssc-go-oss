package jssc

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The JSS8 pool section length (specification §7.4): open trusts it to
// find the key directory, so a lying length must fail — loudly at open where
// it can, and no later than the first pool access where it cannot.
func TestPoolSectionLength(t *testing.T) {
	var buf bytes.Buffer
	if _, err := EncodeWith([]byte(`[{"name":"ada","role":"admin"},{"name":"bob","role":"user"}]`),
		&buf, Options{}); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()

	a, err := OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}
	poolLenAt := a.vals.flags - 4

	// Declared past the buffer: rejected at open.
	tooBig := bytes.Clone(archive)
	binary.LittleEndian.PutUint32(tooBig[poolLenAt:], uint32(len(archive)))
	if _, err := OpenBytes(tooBig); err == nil {
		t.Fatal("pool_len past the buffer was accepted")
	}

	// Declared short: the sections after it are mislocated, so open or the
	// first access must fail — silence is the one wrong answer.
	tooSmall := bytes.Clone(archive)
	binary.LittleEndian.PutUint32(tooSmall[poolLenAt:],
		binary.LittleEndian.Uint32(archive[poolLenAt:])-1)
	if b, err := OpenBytes(tooSmall); err == nil {
		if _, qerr := b.QueryValues("$[0].name"); qerr == nil {
			t.Fatal("shrunken pool_len answered a query")
		}
	}

	// And the honest length round-trips with a lazy walk: the first read walks
	// its segment and finds it ending where pool_len says.
	if got, err := a.QueryValues("$[0].name"); err != nil || len(got) != 1 || got[0] != "ada" {
		t.Fatalf("got %v, %v", got, err)
	}
	if !a.vals.ready || a.vals.segs[0] == nil {
		t.Fatal("reading a pooled string did not walk its segment")
	}
}
