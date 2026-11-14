package jssc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ohler55/ojg/jp"
)

// The walked filter is an optimization of the materializing fallback, and this
// file holds it to that: same answers as ojg over the parsed document, with
// measurably less of the pool decoded.

const filterDoc = `[
	{"login": "ada", "role": "admin", "note": "first", "bio": "writes the compilers"},
	{"login": "bob", "role": "user", "note": "second", "bio": "keeps the printers honest"},
	{"login": "cyn", "role": "admin", "note": "third", "bio": "owns the pager"},
	{"login": "dee", "role": "user", "note": "x", "bio": "third floor"},
	{"login": "eve", "profile": {"tier": "gold"}, "role": "user", "note": "y", "bio": "gold tier"},
	{"login": "fay", "profile": {"tier": "iron"}, "role": "user", "note": "z", "bio": "iron tier"},
	{"login": 42, "role": ["admin"], "note": null, "bio": "role is not even a string"}
]`

func filterArchive(t *testing.T, src string) *Archive {
	t.Helper()
	var buf strings.Builder
	if _, err := EncodeWith([]byte(src), &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	a, err := OpenBytes([]byte(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWalkedFilterAgreesWithOjg(t *testing.T) {
	paths := []string{
		// The fast path: dotted @-paths against string literals, pooled
		// ("admin" is longer than index_width) and inline ("x" is not),
		// present, absent, and nested.
		`$[?(@.role == 'admin')].login`,
		`$[?(@.role == 'user')].login`,
		`$[?(@.note == 'x')].login`,
		`$[?(@.profile.tier == 'gold')].login`,
		`$[?(@.role == 'missing-entirely')].login`,
		`$[?(@.nosuchkey == 'admin')].login`,
		`$[?(@.role == 'admin')]`,
		// Compound predicates walk too: conjunction, disjunction, grouping,
		// inequality, and the literal on the left.
		`$[?(@.role == 'admin' || @.note == 'x')].login`,
		`$[?(@.role == 'admin' && @.note == 'third')].login`,
		`$[?(@.role != 'admin')].login`,
		`$[?(@.nosuchkey != 'admin')].login`,
		`$[?((@.role == 'admin' || @.note == 'x') && @.login == 'dee')].login`,
		`$[?('admin' == @.role)].login`,
		`$[?(@.profile)].login`,
		// Fallback shapes: the walk must not claim these. The numeric literal
		// finds nothing on either side — ojg does not match a bare number
		// against a json.Number, the project-wide reference semantics.
		`$[?(@.login == 42)].role`,
	}

	a := filterArchive(t, filterDoc)
	// The reference tree uses json.Number, as every verifier in this project
	// does: number lexemes survive, and ojg's documented refusal to match a
	// bare numeric literal against a json.Number applies to both sides alike.
	var doc any
	dec := json.NewDecoder(bytes.NewReader([]byte(filterDoc)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got, err := a.QueryValues(path)
			if err != nil {
				t.Fatal(err)
			}
			expr, err := jp.ParseString(path)
			if err != nil {
				t.Fatal(err)
			}
			want := expr.Get(doc)
			if len(got) != len(want) {
				t.Fatalf("walk found %d values, ojg found %d\nwalk: %v\nojg:  %v",
					len(got), len(want), got, want)
			}
			for i := range want {
				g, err := json.Marshal(got[i])
				if err != nil {
					t.Fatal(err)
				}
				w, err := json.Marshal(want[i])
				if err != nil {
					t.Fatal(err)
				}
				if string(g) != string(w) {
					t.Errorf("value %d: walk %s, ojg %s", i, g, w)
				}
			}
		})
	}
}

func TestWalkedFilterDecodesOnlyMatches(t *testing.T) {
	a := filterArchive(t, filterDoc)
	if _, err := a.QueryValues(`$[?(@.role == 'admin')].login`); err != nil {
		t.Fatal(err)
	}
	if limit := a.PoolCount() / 2; a.DecodedStrings > limit {
		t.Errorf("walked filter decoded %d of %d pool strings; want at most %d",
			a.DecodedStrings, a.PoolCount(), limit)
	}

	// A compound predicate walks the same way: two leaves per candidate, no
	// materialization of the rejected.
	a = filterArchive(t, filterDoc)
	if _, err := a.QueryValues(`$[?(@.role == 'admin' && @.note == 'third')].login`); err != nil {
		t.Fatal(err)
	}
	// Two admins match; the walk should decode their logins and the strings
	// the predicate machinery resolved — not the bios, notes, and profiles of
	// the five candidates it rejected. The materializing fallback decodes
	// nearly the whole pool here, which is exactly what this ceiling catches.
	if limit := a.PoolCount() / 2; a.DecodedStrings > limit {
		t.Errorf("walked filter decoded %d of %d pool strings; want at most %d",
			a.DecodedStrings, a.PoolCount(), limit)
	}
}

// spell renders a predicate tree compactly for parser assertions.
func (p *pred) spell() string {
	switch p.kind {
	case predAnd:
		return "and(" + p.l.spell() + "," + p.r.spell() + ")"
	case predOr:
		return "or(" + p.l.spell() + "," + p.r.spell() + ")"
	case predExists:
		return "has(" + strings.Join(p.segs, ".") + ")"
	}
	op := "eq"
	if p.kind == predNe {
		op = "ne"
	}
	lit := "'" + p.s + "'"
	switch p.lit {
	case litNull:
		lit = "null"
	case litBool:
		if p.b {
			lit = "true"
		} else {
			lit = "false"
		}
	}
	return op + "(" + strings.Join(p.segs, ".") + "," + lit + ")"
}

func TestParseWalkablePred(t *testing.T) {
	cases := []struct {
		path  string
		spell string // "" means not walkable
	}{
		{`$[?(@.role == 'admin')]`, "eq(role,'admin')"},
		{`$[?(@.a.b == 'x')]`, "eq(a.b,'x')"},
		{`$[?(@.role != 'admin')]`, "ne(role,'admin')"},
		{`$[?(@.a == 'x' && @.b == 'y')]`, "and(eq(a,'x'),eq(b,'y'))"},
		{`$[?(@.a == 'x' || @.b == 'y')]`, "or(eq(a,'x'),eq(b,'y'))"},
		{`$[?(@.a == 'x' && @.b == 'y' || @.c == 'z')]`,
			"or(and(eq(a,'x'),eq(b,'y')),eq(c,'z'))"},
		{`$[?((@.a == 'x' || @.b == 'y') && @.c == 'z')]`,
			"and(or(eq(a,'x'),eq(b,'y')),eq(c,'z'))"},
		{`$[?('x' == @.a)]`, "eq(a,'x')"},
		{`$[?(@.a == true)]`, "eq(a,true)"},
		{`$[?(@.a != false)]`, "ne(a,false)"},
		{`$[?(@.a == null)]`, "eq(a,null)"},
		{`$[?(@.a)]`, "has(a)"},
		{`$[?(@.a.b && @.c == 'x')]`, "and(has(a.b),eq(c,'x'))"},
		// Not walkable: numbers, orderings, regex, bracketed keys,
		// path-to-path comparison.
		{`$[?(@.n == 5)]`, ""},
		{`$[?(@.n > 'a')]`, ""},
		{`$[?(@.n < 5)]`, ""},
		{`$[?(@['a b'] == 'x')]`, ""},
		{`$[?(@.a == @.b)]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			expr, err := jp.ParseString(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			var f *jp.Filter
			for _, frag := range expr {
				if ff, isFilter := frag.(*jp.Filter); isFilter {
					f = ff
				}
			}
			if f == nil {
				t.Fatal("no filter fragment parsed")
			}
			wp, ok := parseWalkablePred(f)
			if ok != (tc.spell != "") {
				t.Fatalf("walkable = %v, want %v (String() = %q)", ok, tc.spell != "", f.String())
			}
			if !ok {
				return
			}
			if got := wp.spell(); got != tc.spell {
				t.Errorf("parsed %s, want %s (String() = %q)", got, tc.spell, f.String())
			}
		})
	}
}

// The edge shapes: comparisons against Nothing, null members, false booleans,
// empty strings, and values of the wrong type. The walk's answers must be
// ojg's answers, member by member — this is the matrix that pins the
// semantics of absent-means-unequal and existence-ignores-the-value.
func TestWalkedFilterEdgeSemanticsAgreeWithOjg(t *testing.T) {
	const doc = `[
	 {"id":"a","s":"admin","n":5,"b":true,"z":null,"e":""},
	 {"id":"b","s":"user","n":5.0,"b":false},
	 {"id":"c","n":"5"},
	 {"id":"d","s":null},
	 {"id":"e","s":123}
	]`
	paths := []string{
		`$[?(@.s == 'admin')].id`,
		`$[?(@.s != 'admin')].id`,
		`$[?(@.b == true)].id`,
		`$[?(@.b != true)].id`,
		`$[?(@.b == false)].id`,
		`$[?(@.z == null)].id`,
		`$[?(@.z != null)].id`,
		`$[?(@.s == null)].id`,
		`$[?(@.s != null)].id`,
		`$[?(@.b)].id`,
		`$[?(@.z)].id`,
		`$[?(@.e)].id`,
		`$[?(@.s)].id`,
		`$[?(@.missing)].id`,
		`$[?(@.missing != 'x')].id`,
		`$[?(@.missing != null)].id`,
		`$[?(@.s != 'admin' && @.b != true)].id`,
		`$[?(@.s == 'admin' || @.z == null)].id`,
	}

	a := filterArchive(t, doc)
	var ref any
	dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.UseNumber()
	if err := dec.Decode(&ref); err != nil {
		t.Fatal(err)
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got, err := a.QueryValues(path)
			if err != nil {
				t.Fatal(err)
			}
			expr, err := jp.ParseString(path)
			if err != nil {
				t.Fatal(err)
			}
			want := expr.Get(ref)
			if len(got) == 0 && len(want) == 0 {
				return
			}
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			if string(g) != string(w) {
				t.Errorf("walk %s, ojg %s", g, w)
			}
		})
	}
}

// The walked filter must engage on the virtual root of a multi-document
// archive exactly as it does on a real array: same answers as ojg, and only
// a fraction of the pool decoded. Before the virtual-root dispatch existed,
// this shape materialized every document — measured on a million-document
// corpus as the whole pool decoded.
func TestWalkedFilterOverMultiDocumentArchive(t *testing.T) {
	docs := []string{
		`{"login":"ada","role":"admin","note":"first","bio":"writes the compilers"}`,
		`{"login":"bob","role":"user","note":"second","bio":"keeps the printers honest"}`,
		`{"login":"cyn","role":"admin","note":"third","bio":"owns the pager"}`,
		`{"login":"dee","profile":{"tier":"gold"},"role":"user","note":"x","bio":"gold tier"}`,
	}
	lines := strings.Join(docs, "\n")

	var buf strings.Builder
	if _, err := EncodeWith([]byte(lines), &buf, Options{Format: FormatNDJSON}); err != nil {
		t.Fatal(err)
	}
	a, err := OpenBytes([]byte(buf.String()))
	if err != nil {
		t.Fatal(err)
	}

	// The reference tree: the documents as an array, with json.Number
	// semantics, which is how every verifier in this project parses.
	var doc any
	dec := json.NewDecoder(bytes.NewReader([]byte("[" + strings.Join(docs, ",") + "]")))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		`$[?(@.role == 'admin')].login`,
		`$[?(@.profile.tier == 'gold')].login`,
		`$[?(@.role == 'missing')].login`,
		`$[?(@.role == 'admin')]`,
		`$[?(@.role == 'admin' || @.profile.tier == 'gold')].login`,
		`$[?(@.role == 'user' && @.note == 'x')].login`,
	} {
		t.Run(path, func(t *testing.T) {
			got, err := a.QueryValues(path)
			if err != nil {
				t.Fatal(err)
			}
			expr, err := jp.ParseString(path)
			if err != nil {
				t.Fatal(err)
			}
			want := expr.Get(doc)
			if len(got) != len(want) {
				t.Fatalf("walk found %d values, ojg found %d\nwalk: %v\nojg:  %v",
					len(got), len(want), got, want)
			}
			for i := range want {
				g, _ := json.Marshal(got[i])
				w, _ := json.Marshal(want[i])
				if string(g) != string(w) {
					t.Errorf("value %d: walk %s, ojg %s", i, g, w)
				}
			}
		})
	}

	// A fresh archive, one filtered extraction: the two admins' logins and
	// the predicate machinery — not the bios and notes of everyone rejected.
	a2, err := OpenBytes([]byte(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.QueryValues(`$[?(@.role == 'admin')].login`); err != nil {
		t.Fatal(err)
	}
	if limit := a2.PoolCount() / 2; a2.DecodedStrings > limit {
		t.Errorf("multi-document walked filter decoded %d of %d pool strings; want at most %d",
			a2.DecodedStrings, a2.PoolCount(), limit)
	}
}
