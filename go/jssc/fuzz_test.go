package jssc_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// FuzzRoundTrip checks the encoder and decoder against each other: whatever
// valid JSON goes in must come back out as the same value.
func FuzzRoundTrip(f *testing.F) {
	seeds := []string{
		`null`, `0`, `""`, `[]`, `{}`,
		`{"a":1,"b":[true,false,null]}`,
		`[{"k":"v"},{"k":"w"}]`,
		`{"n":1e300,"m":-0.0,"o":123456789012345678901234567890}`,
		`{"unicode":"αβγ 🎉"}`,
		`{"escapes":"a\nb\tc\"d"}`,
		`[[[[[[[[[[1]]]]]]]]]]`,
		`{"a":"a","aa":"aa","aaa":"aaa","aaaa":"aaaa","aaaaa":"aaaaa"}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if !json.Valid(raw) {
			return
		}
		var want any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&want); err != nil {
			return
		}
		// json.Valid accepts a stream of values; we only encode single ones.
		if dec.More() {
			return
		}

		archive, _, err := jssc.EncodeToBytes(raw, jssc.Options{Structure: jssc.StructDeflate})
		if errors.Is(err, jssc.ErrLimitExceeded) {
			// Valid JSON the format cannot hold — a number lexeme past 255
			// bytes, nesting past the depth guard. Refusing is correct.
			return
		}
		if err != nil {
			t.Fatalf("encode valid JSON failed: %v", err)
		}
		a, err := jssc.OpenBytes(archive)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		got, err := a.DecodeAll()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("round trip changed the value\nwant %#v\ngot  %#v", want, got)
		}
	})
}

// FuzzOpen checks that arbitrary bytes never panic a decoder. Failing to parse
// is fine; crashing is not.
func FuzzOpen(f *testing.F) {
	for _, opt := range []jssc.Options{
		{Structure: jssc.StructRaw},
		{Structure: jssc.StructDeflate},
		{Structure: jssc.StructRaw, Layout: jssc.LayoutLegacy},
	} {
		archive, _, err := jssc.EncodeToBytes([]byte(`{"users":[{"name":"Alice","role":"admin"}]}`), opt)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(archive)
	}
	f.Add([]byte("JSS3"))
	f.Add([]byte("JSS4\x00\x00\x03\x01\x01\x00\x00\x00\x00\x01\x00\x00\x00"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		a, err := jssc.OpenBytesWith(raw, jssc.Limits{
			MaxDepth:          64,
			MaxPlainStructure: 1 << 20,
			MaxPoolCount:      1 << 16,
		})
		if err != nil {
			return
		}
		_, _ = a.DecodeAll()
		_, _ = a.MarshalText()
		_, _ = a.Query("$..name")
		_, _ = a.Query("$[*]")
		_, _, _ = a.QueryOne("$[0].name")
	})
}
