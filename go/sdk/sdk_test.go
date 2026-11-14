package sdk_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/sdk"
)

const usersJSON = `{
  "users": [
    {"id": 1, "name": "Alice", "role": "admin",  "email": "alice@example.com"},
    {"id": 2, "name": "Bob",   "role": "user",   "email": "bob@example.com"},
    {"id": 3, "name": "Carol", "role": "user",   "email": "carol@example.com"}
  ],
  "generated": "2026-08-09T00:00:00Z"
}`

const eventsNDJSON = `{"id":"1","type":"PushEvent","actor":{"login":"alice"}}
{"id":"2","type":"WatchEvent","actor":{"login":"bob"}}
{"id":"3","type":"PushEvent","actor":{"login":"carol"}}
`

func jsonValue(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return v
}

func TestCompressRoundTrip(t *testing.T) {
	archive, st, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalBytes != len(archive) {
		t.Errorf("TotalBytes = %d, archive is %d bytes", st.TotalBytes, len(archive))
	}

	text, dst, err := sdk.Decompress(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonValue(t, []byte(usersJSON)), jsonValue(t, text)) {
		t.Error("round trip changed the value")
	}
	if dst.Algorithm != sdk.AlgoFSST {
		t.Errorf("Algorithm = %q, want %q", dst.Algorithm, sdk.AlgoFSST)
	}
}

func TestCompressWithOptions(t *testing.T) {
	for _, structure := range []sdk.Structure{sdk.StructureRaw, sdk.StructureDeflate} {
		t.Run(structure.String(), func(t *testing.T) {
			archive, _, err := sdk.CompressWith([]byte(usersJSON), sdk.Options{Structure: structure})
			if err != nil {
				t.Fatal(err)
			}
			text, _, err := sdk.Decompress(archive)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(jsonValue(t, []byte(usersJSON)), jsonValue(t, text)) {
				t.Error("round trip changed the value")
			}
		})
	}
}

func TestCompressNDJSON(t *testing.T) {
	archive, st, err := sdk.CompressWith([]byte(eventsNDJSON), sdk.Options{
		Format:    sdk.FormatNDJSON,
		Structure: sdk.StructureDeflate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.DocCount != 3 {
		t.Fatalf("DocCount = %d, want 3", st.DocCount)
	}
	text, dst, err := sdk.Decompress(archive)
	if err != nil {
		t.Fatal(err)
	}
	if dst.ContentFormat != sdk.FormatNDJSON {
		t.Errorf("ContentFormat = %v, want ndjson", dst.ContentFormat)
	}
	if got := len(bytes.Split(bytes.TrimSpace(text), []byte("\n"))); got != 3 {
		t.Errorf("restored %d lines, want 3", got)
	}
}

func TestQueryJSONAndArchiveAgree(t *testing.T) {
	archive, _, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"$.users[*].name",
		"$.users[0].email",
		"$..role",
		`$.users[?(@.role=="user")].name`,
		"$.generated",
	} {
		t.Run(path, func(t *testing.T) {
			fromJSON, err := sdk.QueryValues([]byte(usersJSON), path)
			if err != nil {
				t.Fatal(err)
			}
			fromArchive, err := sdk.QueryArchiveValues(archive, path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fromJSON, fromArchive) {
				t.Errorf("JSON gave %#v, archive gave %#v", fromJSON, fromArchive)
			}
		})
	}
}

func TestQueryArchiveOneAndLimit(t *testing.T) {
	archive, _, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := sdk.QueryArchiveOne(archive, "$.users[*].name")
	if err != nil || !ok {
		t.Fatalf("QueryArchiveOne = %v, %v, %v", v, ok, err)
	}
	if v != "Alice" {
		t.Errorf("first name = %v, want Alice", v)
	}

	matches, err := sdk.QueryArchiveLimit(archive, "$.users[*].name", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Errorf("got %d matches, want 2", len(matches))
	}
}

func TestOpenArchiveReuse(t *testing.T) {
	archive, _, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	a, err := sdk.OpenArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	// The pool cache carries across queries, so the second one decodes fewer
	// strings than the first.
	if _, err := a.QueryValues("$.users[*].name"); err != nil {
		t.Fatal(err)
	}
	first := a.DecodedStrings
	if _, err := a.QueryValues("$.users[*].name"); err != nil {
		t.Fatal(err)
	}
	if a.DecodedStrings != 0 {
		t.Errorf("second identical query decoded %d strings; the cache should cover it", a.DecodedStrings)
	}
	if first == 0 {
		t.Error("first query decoded nothing, which cannot be right")
	}
}

func TestFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "users.json")
	if err := os.WriteFile(src, []byte(usersJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	archivePath, st, err := sdk.CompressFile(src, sdk.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if archivePath != src+".jssc" {
		t.Errorf("archive path = %q, want %q", archivePath, src+".jssc")
	}
	if st.RawBytes != len(usersJSON) {
		t.Errorf("RawBytes = %d, want %d", st.RawBytes, len(usersJSON))
	}

	// Move the original aside so the restore has to recreate it.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	outPath, _, err := sdk.DecompressFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if outPath != src {
		t.Errorf("restored to %q, want %q", outPath, src)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonValue(t, []byte(usersJSON)), jsonValue(t, got)) {
		t.Error("restored file differs from the original")
	}
}

func TestDecompressFileRestoresLineExtension(t *testing.T) {
	dir := t.TempDir()
	// A bare name, so the content format has to supply the extension.
	src := filepath.Join(dir, "events")
	if err := os.WriteFile(src, []byte(eventsNDJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath, _, err := sdk.CompressFile(src, sdk.Options{Format: sdk.FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	outPath, _, err := sdk.DecompressFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := src + ".ndjson"; outPath != want {
		t.Errorf("restored to %q, want %q", outPath, want)
	}
}

func TestDecompressFileRejectsWrongSuffix(t *testing.T) {
	if _, _, err := sdk.DecompressFile("data.json"); err == nil {
		t.Fatal("expected an error for a non-.jssc path")
	}
}

func TestCompressFileCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(src, []byte(`{"broken":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sdk.CompressFile(src, sdk.DefaultOptions()); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if _, err := os.Stat(src + ".jssc"); !os.IsNotExist(err) {
		t.Error("a failed compress left its output file behind")
	}
}

func TestInspect(t *testing.T) {
	archive, _, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sdk.Inspect(archive)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != sdk.LatestVersion {
		t.Errorf("Version = %d, want %d", st.Version, sdk.LatestVersion)
	}
	if st.UniqueStrings == 0 {
		t.Error("UniqueStrings = 0, want a populated pool")
	}
}

func TestRegistries(t *testing.T) {
	algos := sdk.Algorithms()
	if len(algos) < 1 {
		t.Fatalf("got %d algorithms, want at least fsst", len(algos))
	}
	var sawDefault bool
	for _, a := range algos {
		if a.Default {
			sawDefault = true
			if a.Name != sdk.AlgoFSST {
				t.Errorf("default algorithm is %q, want %q", a.Name, sdk.AlgoFSST)
			}
			if !a.Available {
				t.Error("the default algorithm must be available in every build")
			}
		}
	}
	if !sawDefault {
		t.Error("no algorithm is marked default")
	}

	structures := sdk.Structures()
	if len(structures) != 2 {
		t.Errorf("got %d structure codecs, want 2", len(structures))
	}
}

func TestParseHelpers(t *testing.T) {
	if f, err := sdk.ParseFormat("ndjson"); err != nil || f != sdk.FormatNDJSON {
		t.Errorf("ParseFormat(ndjson) = %v, %v", f, err)
	}
	if _, err := sdk.ParseFormat("csv"); err == nil {
		t.Error("ParseFormat(csv) should fail")
	}
	if s, err := sdk.ParseStructure("raw"); err != nil || s != sdk.StructureRaw {
		t.Errorf("ParseStructure(raw) = %v, %v", s, err)
	}
}

func TestUnknownAlgorithmIsRejected(t *testing.T) {
	if _, _, err := sdk.CompressWith([]byte(usersJSON), sdk.Options{Algorithm: "nope"}); err == nil {
		t.Fatal("expected an error for an unregistered algorithm")
	}
}
