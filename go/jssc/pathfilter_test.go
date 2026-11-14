package jssc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// pathFilterDoc is large enough that the filter earns its place under the cost
// rule: the section is emitted only when it is small beside the bytes it
// indexes, so a three-key document would carry none.
func pathFilterDoc(t *testing.T) []byte {
	t.Helper()
	rows := make([]any, 400)
	for i := range rows {
		rows[i] = map[string]any{
			"id":    json.Number(fmt.Sprint(i)),
			"actor": map[string]any{"login": fmt.Sprintf("user%d", i), "id": json.Number(fmt.Sprint(i))},
			"repo":  map[string]any{"name": fmt.Sprintf("org/repo%d", i%17)},
			"note":  strings.Repeat("a distinct sentence of prose ", 3) + fmt.Sprint(i),
		}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func openFiltered(t *testing.T, raw []byte, opt Options) (*Archive, *Stats) {
	t.Helper()
	var buf bytes.Buffer
	st, err := EncodeWith(raw, &buf, opt)
	if err != nil {
		t.Fatal(err)
	}
	a, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return a, st
}

func TestPathFilterIsCarriedWhenItPaysForItself(t *testing.T) {
	raw := pathFilterDoc(t)
	a, st := openFiltered(t, raw, Options{Structure: StructDeflate})
	if st.PathFilterBytes == 0 {
		t.Fatalf("no filter emitted for a %d-byte document", len(raw))
	}
	if st.PathFilterPaths != 7 {
		// id, actor, actor.login, actor.id, repo, repo.name, note
		t.Errorf("filter indexes %d paths, want 7", st.PathFilterPaths)
	}
	if !a.paths.present() {
		t.Error("the archive declares a filter but the reader did not find one")
	}
	if share := float64(st.PathFilterBytes) / float64(st.TotalBytes); share > 1.0/pathFilterCostRatio {
		t.Errorf("filter is %.2f%% of the archive, above the cost rule", 100*share)
	}
}

func TestPathFilterRejectsAbsentPathsWithoutWalking(t *testing.T) {
	a, _ := openFiltered(t, pathFilterDoc(t), Options{Structure: StructDeflate})

	for _, path := range []string{
		"$[*].actor.missing",    // a key that exists, under a parent it never has
		"$[*].repo.login",       // both keys exist, this pairing does not
		"$[*].nowhere",          // a key the document does not hold at all
		"$[0].actor.login.deep", // one level past a leaf
	} {
		t.Run(path, func(t *testing.T) {
			got, err := a.QueryValues(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("got %d matches, want none", len(got))
			}
			if !a.FilteredOut {
				t.Error("the filter did not reject the path; the walk ran instead")
			}
			if a.DecodedGroups != 0 {
				t.Errorf("a rejected path decoded %d pool groups, want 0", a.DecodedGroups)
			}
		})
	}
}

func TestPathFilterNeverRejectsAPathThatExists(t *testing.T) {
	raw := pathFilterDoc(t)
	a, _ := openFiltered(t, raw, Options{Structure: StructDeflate})

	// Every path the document holds, including one reached through an array
	// subscript, which the filter elides.
	for _, path := range []string{
		"$[*].id", "$[*].actor.login", "$[*].actor.id", "$[*].repo.name", "$[*].note",
		"$[0].actor.login", "$[399].repo.name",
	} {
		t.Run(path, func(t *testing.T) {
			got, err := a.QueryValues(path)
			if err != nil {
				t.Fatal(err)
			}
			if a.FilteredOut {
				t.Fatal("the filter rejected a path the document holds")
			}
			if len(got) == 0 {
				t.Fatal("no matches for a path the document holds")
			}
		})
	}
}

// The filter must never change an answer, only the work behind it. This runs
// every query both ways and compares.
func TestPathFilterDoesNotChangeAnswers(t *testing.T) {
	raw := pathFilterDoc(t)
	with, st := openFiltered(t, raw, Options{Structure: StructDeflate})
	without, stWithout := openFiltered(t, raw, Options{Structure: StructDeflate, OmitPathFilter: true})

	if st.PathFilterBytes == 0 || stWithout.PathFilterBytes != 0 {
		t.Fatalf("the two archives are not a with/without pair: %d and %d filter bytes",
			st.PathFilterBytes, stWithout.PathFilterBytes)
	}
	if stWithout.TotalBytes >= st.TotalBytes {
		t.Errorf("suppressing the filter did not make the archive smaller: %d vs %d",
			stWithout.TotalBytes, st.TotalBytes)
	}

	for _, path := range []string{
		"$[*].id", "$[*].actor.login", "$[*].missing", "$..login", "$[*].repo.name",
		`$[?(@.id=="7")].note`, "$[10:12].id", "$[*].*",
	} {
		t.Run(path, func(t *testing.T) {
			a, errA := with.QueryValues(path)
			b, errB := without.QueryValues(path)
			if (errA == nil) != (errB == nil) {
				t.Fatalf("errors differ: %v vs %v", errA, errB)
			}
			if len(a) != len(b) {
				t.Fatalf("%d matches with the filter, %d without", len(a), len(b))
			}
			for i := range a {
				if fmt.Sprint(a[i]) != fmt.Sprint(b[i]) {
					t.Fatalf("match %d differs: %v vs %v", i, a[i], b[i])
				}
			}
		})
	}
}

func TestPathFilterSurvivesARoundTripThroughItsOwnEncoding(t *testing.T) {
	paths := []uint32{}
	for i := range 200 {
		paths = append(paths, pathHash(uint32(i), fmt.Sprintf("key%d", i)))
	}
	section := buildPathFilter(paths)
	f, next, err := readPathFilter(section, 0)
	if err != nil {
		t.Fatal(err)
	}
	if next != len(section) {
		t.Errorf("parsed %d of %d bytes", next, len(section))
	}
	for _, h := range paths {
		if !f.mayContain(h) {
			t.Fatalf("a path that was inserted is reported absent: %#08x", h)
		}
	}

	// A false positive is allowed; a rate far above the design point is not.
	false_ := 0
	const probes = 20000
	for i := range probes {
		if f.mayContain(pathHash(0xdeadbeef, fmt.Sprint(i))) {
			false_++
		}
	}
	if rate := float64(false_) / probes; rate > 0.05 {
		t.Errorf("false-positive rate %.3f, design point is about 0.008", rate)
	}
}
