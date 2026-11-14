package jssc

import (
	"bytes"
	"testing"
)

// Opening an archive must not prepare the pool. A column reference in a database
// hands every operator a fresh datum, so anything open does is paid once per row
// per operator, and a lookup that returns a number or an inline string needs no
// string from the pool at all.
func TestOpenDoesNotPrepareThePool(t *testing.T) {
	var buf bytes.Buffer
	src := []byte(`{"count":42,"ok":true,"name":"a long enough value to be pooled","ratio":1.5}`)
	if _, err := EncodeWith(src, &buf, Options{Structure: StructRaw}); err != nil {
		t.Fatal(err)
	}

	a, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if a.vals.ready {
		t.Error("opening built the pool index and cache")
	}
	if a.vals.segs != nil {
		t.Error("opening allocated the segment table")
	}
	// The stats it reports are the ones a caller expects at open, so the scan
	// that finds the section's extent still has to account for it.
	if a.Stats().PoolBytes == 0 {
		t.Error("open reported no pool bytes for an archive with a pool")
	}
}

func TestALookupThatReturnsANumberNeverTouchesThePool(t *testing.T) {
	var buf bytes.Buffer
	src := []byte(`{"count":42,"nested":{"depth":7},"name":"a long enough value to be pooled"}`)
	if _, err := EncodeWith(src, &buf, Options{Structure: StructRaw}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"$.count", "$.nested.depth", "$.absent"} {
		t.Run(path, func(t *testing.T) {
			a, err := OpenBytes(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.QueryValues(path); err != nil {
				t.Fatal(err)
			}
			if a.vals.ready {
				t.Errorf("%s built the pool index; the key directory should have"+
					" answered without it", path)
			}
			if a.DecodedGroups != 0 || a.DecodedStrings != 0 {
				t.Errorf("%s decoded %d groups and %d strings, want none",
					path, a.DecodedGroups, a.DecodedStrings)
			}
		})
	}
}

// And a lookup that does return a pooled string builds the index once, on
// demand, rather than not at all — the laziness must not become a leak.
func TestALookupThatReturnsAStringPreparesThePoolOnce(t *testing.T) {
	var buf bytes.Buffer
	src := []byte(`{"count":42,"name":"a long enough value to be pooled"}`)
	if _, err := EncodeWith(src, &buf, Options{Structure: StructRaw}); err != nil {
		t.Fatal(err)
	}
	a, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.QueryValues("$.name")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "a long enough value to be pooled" {
		t.Fatalf("got %v", got)
	}
	if !a.vals.ready {
		t.Error("returning a pooled string did not prepare the pool")
	}
	if a.DecodedGroups != 1 {
		t.Errorf("decoded %d groups to return one string", a.DecodedGroups)
	}

	// Asking again must not rebuild anything.
	before := a.DecodedGroups
	if _, err := a.QueryValues("$.name"); err != nil {
		t.Fatal(err)
	}
	if a.DecodedGroups != 0 {
		t.Errorf("the second lookup decoded %d groups; the first one cached them (was %d)",
			a.DecodedGroups, before)
	}
}
