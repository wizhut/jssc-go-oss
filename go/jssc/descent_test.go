package jssc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ohler55/ojg/jp"
)

// queryParsed evaluates path over the fully parsed document, which is the
// reference every selective walk is held to.
func queryParsed(t *testing.T, raw []byte, path string) []any {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	expr, err := jp.ParseString(path)
	if err != nil {
		t.Fatal(err)
	}
	return expr.Get(v)
}

// descentDoc holds the same key at two depths and a lot of keys that are only
// passed over, which is the shape that prices a descent: the walk visits every
// member of every object and the answer mentions three of them.
func descentDoc(t *testing.T) []byte {
	t.Helper()
	rows := make([]any, 300)
	for i := range rows {
		rows[i] = map[string]any{
			"id":    json.Number(fmt.Sprint(i)),
			"owner": map[string]any{"login": fmt.Sprintf("user%d", i), "kind": "user"},
			"repo": map[string]any{
				"name":  fmt.Sprintf("org/repo%d", i),
				"owner": map[string]any{"login": fmt.Sprintf("org%d", i%9), "kind": "org"},
			},
			"tags": []any{"alpha", "beta", fmt.Sprintf("t%d", i)},
		}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func openDescent(t *testing.T, opt Options) *Archive {
	t.Helper()
	var buf bytes.Buffer
	if _, err := EncodeWith(descentDoc(t), &buf, opt); err != nil {
		t.Fatal(err)
	}
	a, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A descent for values resolves the key it is looking for once and compares
// pool indices from then on, so it decodes only the strings it returns. The
// document's pool is far larger than that.
func TestDescentForValuesDecodesOnlyWhatItReturns(t *testing.T) {
	a := openDescent(t, Options{Structure: StructRaw, PerEntryPool: true})

	got, err := a.QueryValues("$..login")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 600 { // one per row's owner, one per row's repo.owner
		t.Fatalf("got %d matches, want 600", len(got))
	}
	if a.DecodedStrings > len(got) {
		t.Errorf("decoded %d pool entries to return %d values; a descent should"+
			" decode only what it answers with", a.DecodedStrings, len(got))
	}
	// The pool holds every key and every distinct value; decoding a fraction of
	// it is the whole point.
	if a.DecodedStrings >= a.PoolCount() {
		t.Errorf("decoded %d of %d pool entries", a.DecodedStrings, a.PoolCount())
	}
}

// Asking for paths necessarily names every object the walk descends through, so
// it costs more. The contrast is the measurement: same answer, different work.
func TestDescentForPathsCostsMoreThanForValues(t *testing.T) {
	values := openDescent(t, Options{Structure: StructRaw, PerEntryPool: true})
	paths := openDescent(t, Options{Structure: StructRaw, PerEntryPool: true})

	if _, err := values.QueryValues("$..login"); err != nil {
		t.Fatal(err)
	}
	got, err := paths.QueryPaths("$..login")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 600 {
		t.Fatalf("got %d paths, want 600", len(got))
	}
	if paths.DecodedStrings <= values.DecodedStrings {
		t.Errorf("naming the matches decoded %d entries and not naming them %d;"+
			" the values-only walk was supposed to be the cheaper one",
			paths.DecodedStrings, values.DecodedStrings)
	}

	// And the paths themselves have to be right, or the saving is a bug.
	for _, want := range []string{"$[0].owner.login", "$[0].repo.owner.login"} {
		found := false
		for _, p := range got {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no match named %s", want)
		}
	}
}

// A descent for a key the document does not hold must not decode anything: the
// key directory settles it before the walk compares a single member.
func TestDescentForAnAbsentKeyDecodesNothing(t *testing.T) {
	a := openDescent(t, Options{Structure: StructRaw, PerEntryPool: true})

	got, err := a.QueryValues("$..nosuchkey")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d matches for an absent key", len(got))
	}
	if a.DecodedStrings != 0 {
		t.Errorf("decoded %d pool entries to establish a key is absent, want 0", a.DecodedStrings)
	}
}

// The values a descent returns must not change with the shape of the walk that
// found them. This compares against the same query over the materialized tree.
func TestDescentAgreesWithTheParsedDocument(t *testing.T) {
	raw := descentDoc(t)
	a := openDescent(t, Options{Structure: StructRaw, PerEntryPool: true})

	for _, path := range []string{"$..login", "$..kind", "$..name", "$..id", "$..nosuchkey"} {
		t.Run(path, func(t *testing.T) {
			got, err := a.QueryValues(path)
			if err != nil {
				t.Fatal(err)
			}
			want := queryParsed(t, raw, path)
			if len(got) != len(want) {
				t.Fatalf("archive returned %d values, the parsed document %d", len(got), len(want))
			}
			gotSet := map[string]int{}
			for _, v := range got {
				gotSet[fmt.Sprint(v)]++
			}
			for _, v := range want {
				k := fmt.Sprint(v)
				if gotSet[k] == 0 {
					t.Fatalf("value %v is in the document but not in the answer", v)
				}
				gotSet[k]--
			}
		})
	}
}
