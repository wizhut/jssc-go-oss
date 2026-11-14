// Command basic compresses a JSON document, reads single fields out of the
// archive without decompressing it, and restores the whole document.
//
//	go run ./examples/basic
package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/wizhut/jssc-go-oss/go/sdk"
)

func main() {
	// A document with the repetition JSSC is built for: every key, and the
	// repeated roles and cities, are stored once in the pools.
	var b strings.Builder
	b.WriteString(`{"users":[`)
	roles := []string{"admin", "editor", "viewer"}
	cities := []string{"Athens", "Lisbon", "Tallinn", "Utrecht"}
	for i := range 200 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"user %d","email":"user%d@example.com","role":%q,"city":%q,"score":%d.5}`,
			i, i, i, roles[i%len(roles)], cities[i%len(cities)], i%10)
	}
	b.WriteString(`]}`)
	doc := []byte(b.String())

	archive, st, err := sdk.Compress(doc)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d bytes of JSON -> %d bytes of archive (%.1fx), %d pooled strings\n",
		len(doc), len(archive), sdk.Ratio(st), st.UniqueStrings)

	// One value: the walk skips the other 199 users and decodes one string.
	email, found, err := sdk.QueryArchiveOne(archive, "$.users[150].email")
	if err != nil || !found {
		log.Fatal(err)
	}
	fmt.Println("users[150].email =", email)

	// A filter, evaluated over the archive's structure.
	admins, err := sdk.QueryArchiveValues(archive, `$.users[?(@.city == "Tallinn" && @.role == "admin")].name`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d admins in Tallinn, first %v\n", len(admins), admins[0])

	// Reuse an open archive across queries to keep its caches warm.
	a, err := sdk.OpenArchive(archive)
	if err != nil {
		log.Fatal(err)
	}
	scores, err := a.QueryValues("$.users[0:3].score")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("first three scores, lexemes kept:", scores)

	// The whole document comes back; object members are in key order.
	text, _, err := sdk.Decompress(archive)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("restored:", string(text[:64])+"…")
}
