package jssc_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// writeNDJSON builds a corpus with enough repetition that a shared pool is
// clearly worth having.
func writeNDJSON(t *testing.T, dir string, docs int) (path string, raw []byte) {
	t.Helper()
	var b strings.Builder
	for i := range docs {
		fmt.Fprintf(&b,
			`{"id":%d,"type":"PushEvent","actor":{"login":"user%d","id":%d},"repo":{"name":"org/repo%d"},"payload":{"size":%d,"ref":"refs/heads/main"}}`+"\n",
			i, i%50, 1000+i%50, i%20, i%7)
	}
	raw = []byte(b.String())
	path = filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func TestStreamingMatchesInMemory(t *testing.T) {
	dir := t.TempDir()
	path, raw := writeNDJSON(t, dir, 500)

	for _, sc := range []jssc.StructCodec{jssc.StructRaw, jssc.StructDeflate} {
		t.Run(sc.String(), func(t *testing.T) {
			// MaxDirectBytes < 0 forces the two-pass path regardless of size.
			var streamed bytes.Buffer
			streamStats, err := jssc.EncodeFile(path, &streamed, jssc.Options{
				Format:         jssc.FormatNDJSON,
				Structure:      sc,
				MaxDirectBytes: -1,
			})
			if err != nil {
				t.Fatalf("streaming encode: %v", err)
			}

			var direct bytes.Buffer
			directStats, err := jssc.EncodeWith(raw, &direct, jssc.Options{
				Format:    jssc.FormatNDJSON,
				Structure: sc,
			})
			if err != nil {
				t.Fatalf("in-memory encode: %v", err)
			}

			// The two paths must agree byte for byte: same pool, same order,
			// same structure. Anything else means one of them is not the format.
			if !bytes.Equal(streamed.Bytes(), direct.Bytes()) {
				t.Fatalf("streaming and in-memory archives differ (%d vs %d bytes)",
					streamed.Len(), direct.Len())
			}
			if streamStats.DocCount != directStats.DocCount {
				t.Errorf("DocCount: streaming %d, in-memory %d", streamStats.DocCount, directStats.DocCount)
			}
			if streamStats.UniqueStrings != directStats.UniqueStrings {
				t.Errorf("UniqueStrings: streaming %d, in-memory %d",
					streamStats.UniqueStrings, directStats.UniqueStrings)
			}

			text := decode(t, streamed.Bytes())
			wantLines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
			gotLines := bytes.Split(bytes.TrimSpace(text), []byte("\n"))
			if len(wantLines) != len(gotLines) {
				t.Fatalf("restored %d lines, want %d", len(gotLines), len(wantLines))
			}
			for i := range wantLines {
				jsonEqual(t, wantLines[i], gotLines[i])
			}
		})
	}
}

func TestStreamingHonorsMaxDocs(t *testing.T) {
	dir := t.TempDir()
	path, _ := writeNDJSON(t, dir, 200)

	var buf bytes.Buffer
	st, err := jssc.EncodeFile(path, &buf, jssc.Options{
		Format:         jssc.FormatJSONL,
		MaxDirectBytes: -1,
		MaxDocs:        25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.DocCount != 25 {
		t.Fatalf("DocCount = %d, want 25", st.DocCount)
	}
	a, err := jssc.OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	ids, err := a.QueryValues("$[*].id")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 25 {
		t.Errorf("got %d ids, want 25", len(ids))
	}
}

func TestEncodeFromReader(t *testing.T) {
	dir := t.TempDir()
	_, raw := writeNDJSON(t, dir, 100)

	// A plain reader is not a file, so this exercises the spill-to-temp path.
	var buf bytes.Buffer
	st, err := jssc.EncodeFrom(bytes.NewReader(raw), &buf, jssc.Options{
		Format:         jssc.FormatNDJSON,
		Structure:      jssc.StructDeflate,
		MaxDirectBytes: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.DocCount != 100 {
		t.Errorf("DocCount = %d, want 100", st.DocCount)
	}

	var direct bytes.Buffer
	if _, err := jssc.EncodeWith(raw, &direct, jssc.Options{
		Format:    jssc.FormatNDJSON,
		Structure: jssc.StructDeflate,
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), direct.Bytes()) {
		t.Error("EncodeFrom and EncodeWith produced different archives")
	}
}

func TestEncodeFileSingleJSON(t *testing.T) {
	var buf bytes.Buffer
	st, err := jssc.EncodeFile("testdata/users.json", &buf, jssc.Options{Structure: jssc.StructDeflate})
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != LatestArchiveVersion {
		t.Errorf("version = %d, want %d", st.Version, LatestArchiveVersion)
	}
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, raw, decode(t, buf.Bytes()))
}

func TestEncodeFileMissing(t *testing.T) {
	var buf bytes.Buffer
	if _, err := jssc.EncodeFile(filepath.Join(t.TempDir(), "absent.json"), &buf, jssc.Options{}); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
