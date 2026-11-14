package fsst_test

import (
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/fsst"
)

// TestDumpFSSTVectors writes cross-implementation vectors: a trained table and
// encodings an FSST port in another language can be held to. A port that
// reproduces this table and these encodings byte for byte writes the same pool
// as this package for the same input.
//
//	FSST_DUMP=fsst-vectors.txt go test -run TestDumpFSSTVectors ./fsst
func TestDumpFSSTVectors(t *testing.T) {
	if os.Getenv("FSST_DUMP") == "" {
		t.Skip("set FSST_DUMP=<path>")
	}
	var corpus [][]byte
	for i := range 400 {
		corpus = append(corpus,
			[]byte(fmt.Sprintf("user%d@example.com", i)),
			[]byte(fmt.Sprintf("/api/v1/resource/%d/detail", i)),
			[]byte(fmt.Sprintf("2026-08-%02dT12:00:00Z", (i%28)+1)))
	}
	tab := fsst.Train(corpus)
	state, err := tab.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(os.Getenv("FSST_DUMP"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "symbols %d\n", tab.NumSymbols())
	fmt.Fprintf(f, "state %s\n", hex.EncodeToString(state))
	for _, s := range corpus {
		fmt.Fprintf(f, "enc %s %s\n", hex.EncodeToString(s), hex.EncodeToString(tab.Encode(s)))
	}
}
