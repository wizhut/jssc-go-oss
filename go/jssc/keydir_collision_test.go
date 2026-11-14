package jssc

import (
	"bytes"
	"testing"
)

// "l98cu" and "pvdba" both hash to 0x5304d385 under CRC-32/IEEE. They are the
// cheapest way to exercise the branch that decides what a directory does when
// two keys land on the same hash, which is the whole difference between the
// JSS7 directory and the JSS8 one.
const (
	collidingKeyA = "l98cu"
	collidingKeyB = "pvdba"
)

func TestCollidingKeysHashTheSame(t *testing.T) {
	if keyHash(collidingKeyA) != keyHash(collidingKeyB) {
		t.Fatalf("the fixture is not a collision any more: %#08x vs %#08x",
			keyHash(collidingKeyA), keyHash(collidingKeyB))
	}
	if collidingKeyA == collidingKeyB {
		t.Fatal("a string does not collide with itself")
	}
}

// collidingDoc is an object holding both colliding keys and a third that does
// not collide, so one test covers the run and its neighbours.
var collidingDoc = []byte(`{"` + collidingKeyA + `":"first","` + collidingKeyB + `":"second","plain":"third"}`)

func TestJSS7DropsTheDirectoryOnACollision(t *testing.T) {
	var buf bytes.Buffer
	st, err := EncodeWith(collidingDoc, &buf, Options{Structure: StructDeflate, Version: 7})
	if err != nil {
		t.Fatal(err)
	}
	if st.KeyDirEntries != 0 {
		t.Errorf("JSS7 wrote %d directory entries; a collision should have emptied it", st.KeyDirEntries)
	}

	// The archive still answers correctly — it falls back to searching the pool
	// — which is exactly what makes the loss silent.
	assertLookups(t, buf.Bytes())
}

func TestJSS8KeepsTheDirectoryThroughACollision(t *testing.T) {
	var buf bytes.Buffer
	st, err := EncodeWith(collidingDoc, &buf, Options{Structure: StructDeflate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 8 {
		t.Fatalf("version = %d, want 8", st.Version)
	}
	if st.KeyDirEntries != 3 {
		t.Errorf("JSS8 wrote %d directory entries, want all 3 keys", st.KeyDirEntries)
	}
	assertLookups(t, buf.Bytes())
}

// assertLookups checks every key resolves to the member it names, that the two
// colliding keys resolve to *different* pool indices, and that a key the
// document does not hold is reported absent.
func assertLookups(t *testing.T, archive []byte) {
	t.Helper()
	a, err := OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		collidingKeyA: "first",
		collidingKeyB: "second",
		"plain":       "third",
	} {
		got, err := a.QueryValues("$." + key)
		if err != nil {
			t.Fatalf("query $.%s: %v", key, err)
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("$.%s = %v, want [%q]", key, got, want)
		}
	}

	ia, oka, err := a.lookupFieldIndex(collidingKeyA)
	if err != nil {
		t.Fatal(err)
	}
	ib, okb, err := a.lookupFieldIndex(collidingKeyB)
	if err != nil {
		t.Fatal(err)
	}
	if !oka || !okb {
		t.Fatalf("colliding keys not both found: %v %v", oka, okb)
	}
	if ia == ib {
		t.Errorf("both colliding keys resolved to pool index %d; the run was not settled against the pool", ia)
	}

	// A miss must stay definitive: the directory lists every key, so a hash that
	// is not in it belongs to no member anywhere in the document.
	if _, ok, err := a.lookupFieldIndex("absent"); err != nil || ok {
		t.Errorf("lookup of an absent key: ok=%v err=%v, want false and no error", ok, err)
	}
}
