package jssc_test

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// appendixB is the complete archive of Appendix B of spec/draft-jssc-00.md:
// the document {"id":7,"name":"Ada","tags":["x","yy"],"ok":true} with the FSST
// pool codec, a raw structure, the default layout and group size. The draft
// annotates every byte, so this test holds the encoder to the specification's
// worked example, not to an earlier run of itself.
const appendixB = `
4a 53 53 38 00 00 03 01 00 00 04 00 00 00 02 00
00 00 01 00 00 00 01 0d 00 00 0a 14 34 01 0d 00
00 00 00 00 00 00 61 64 79 41 65 67 69 6b 6d 6e
6f 73 74 11 00 00 00 0f 02 69 64 04 6e 61 6d 65
02 6f 6b 04 74 61 67 73 08 00 00 00 03 03 41 64
61 02 79 79 04 00 00 00 06 7e 23 5e 01 26 94 bc
6f 03 47 dd dc 79 02 50 67 39 bf 00 31 00 00 00
01 00 00 00 25 00 00 00 25 00 00 00 04 00 00 00
20 06 04 00 00 00 00 00 03 03 01 37 01 02 04 00
02 01 02 03 0c 05 02 00 00 00 00 02 08 78 02 04
01`

func TestSpecificationWorkedExample(t *testing.T) {
	want, err := hex.DecodeString(strings.Join(strings.Fields(appendixB), ""))
	if err != nil {
		t.Fatal(err)
	}
	doc := []byte(`{"id":7,"name":"Ada","tags":["x","yy"],"ok":true}`)
	got, _, err := jssc.EncodeToBytes(doc, jssc.Options{Structure: jssc.StructRaw})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("archive differs from Appendix B:\n got %x\nwant %x", got, want)
	}

	a, err := jssc.OpenBytes(want)
	if err != nil {
		t.Fatal(err)
	}
	text, err := a.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	// Members come back in key order (specification §13).
	if string(text) != `{"id":7,"name":"Ada","ok":true,"tags":["x","yy"]}` {
		t.Errorf("decoded %s", text)
	}
	if v, ok, err := a.QueryOne("$.tags[1]"); err != nil || !ok || v != "yy" {
		t.Errorf("$.tags[1] = %v, %v, %v", v, ok, err)
	}
}
