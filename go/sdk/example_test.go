package sdk_test

import (
	"fmt"
	"strings"

	"github.com/wizhut/jssc-go-oss/go/sdk"
)

func ExampleCompress() {
	// Repetition is what JSSC is for: the keys and the repeated role appear
	// once in the pool, and every later use is a one-byte index.
	var b strings.Builder
	b.WriteString(`{"users":[`)
	for i := range 50 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"user%d","email":"user%d@example.com","role":"member"}`, i, i, i)
	}
	b.WriteString(`]}`)
	data := []byte(b.String())

	archive, st, err := sdk.Compress(data)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d pooled strings, %.1fx smaller\n", st.UniqueStrings, st.Ratio())

	restored, _, err := sdk.Decompress(archive)
	if err != nil {
		panic(err)
	}
	// Values survive exactly, including number lexemes. Object members come
	// back in key order rather than input order, which JSON does not consider
	// significant.
	fmt.Println(string(restored[:52]))
	// The ratio was 4.7x when the pool was compressed as groups of 64 entries by
	// a general compressor. Each entry now compresses on its own, so a query
	// decodes one string instead of its 63 neighbours, and the cross-entry
	// redundancy that grouping was finding is left on the table.
	// Output:
	// 106 pooled strings, 2.4x smaller
	// {"users":[{"email":"user0@example.com","id":0,"name"
}

func ExampleQueryArchiveValues() {
	data := []byte(`{"users":[{"name":"Alice","role":"admin"},{"name":"Bob","role":"user"}]}`)
	archive, _, err := sdk.Compress(data)
	if err != nil {
		panic(err)
	}

	// The walk decodes only the pool strings this path needs; "role" and its
	// values stay compressed.
	names, err := sdk.QueryArchiveValues(archive, "$.users[*].name")
	if err != nil {
		panic(err)
	}
	fmt.Println(names)
	// Output: [Alice Bob]
}

func ExampleCompressWith_ndjson() {
	lines := []byte(`{"id":1,"type":"push"}` + "\n" + `{"id":2,"type":"watch"}` + "\n")

	// Line-oriented input becomes one multi-document archive over a shared
	// string pool. The format must be declared; it is never guessed.
	archive, st, err := sdk.CompressWith(lines, sdk.Options{
		Format:    sdk.FormatNDJSON,
		Structure: sdk.StructureDeflate,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("documents:", st.DocCount)

	// Documents are queried as a virtual root array.
	types, err := sdk.QueryArchiveValues(archive, "$[*].type")
	if err != nil {
		panic(err)
	}
	fmt.Println(types)
	// Output:
	// documents: 2
	// [push watch]
}

func ExampleOpenArchive() {
	data := []byte(`{"a":{"deep":{"value":42}},"b":[1,2,3]}`)
	archive, _, err := sdk.Compress(data)
	if err != nil {
		panic(err)
	}

	// Opening once and querying repeatedly keeps the decoded-string and
	// container-offset caches warm.
	a, err := sdk.OpenArchive(archive)
	if err != nil {
		panic(err)
	}
	v, found, err := a.QueryOne("$.a.deep.value")
	if err != nil {
		panic(err)
	}
	fmt.Println(v, found)
	// Output: 42 true
}
