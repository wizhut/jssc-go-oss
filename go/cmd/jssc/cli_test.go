package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const usersJSON = `[
  {"id":1,"name":"Alice","email":"alice@example.com","role":"admin"},
  {"id":2,"name":"Bob","email":"bob@example.com","role":"user"},
  {"id":3,"name":"Carol","email":"carol@example.com","role":"user"}
]`

const eventsNDJSON = `{"id":"1","type":"PushEvent","actor":{"login":"alice"}}
{"id":"2","type":"WatchEvent","actor":{"login":"bob"}}
`

// run executes the CLI in-process and returns its stdout, stderr, and error.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompressDecompressRoundTrip(t *testing.T) {
	src := writeTemp(t, "users.json", usersJSON)

	if _, stderr, err := run(t, "compress", src); err != nil {
		t.Fatalf("compress: %v (%s)", err, stderr)
	}
	if _, err := os.Stat(src + ".jssc"); err != nil {
		t.Fatalf("archive was not written: %v", err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := run(t, "decompress", src+".jssc"); err != nil {
		t.Fatalf("decompress: %v (%s)", err, stderr)
	}

	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var want, have any
	if err := json.Unmarshal([]byte(usersJSON), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, have) {
		t.Error("round trip through the CLI changed the value")
	}
}

func TestCompressNDJSON(t *testing.T) {
	src := writeTemp(t, "events.ndjson", eventsNDJSON)
	_, stderr, err := run(t, "compress", "--format", "ndjson", src)
	if err != nil {
		t.Fatalf("compress: %v (%s)", err, stderr)
	}
	if !strings.Contains(stderr, "docs=2") {
		t.Errorf("compress did not report 2 documents: %s", stderr)
	}

	stdout, _, err := run(t, "info", src+".jssc")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"JSS8", "ndjson", "documents       2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("info output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestQueryJSONAndArchive(t *testing.T) {
	src := writeTemp(t, "users.json", usersJSON)
	if _, _, err := run(t, "compress", src); err != nil {
		t.Fatal(err)
	}

	// The same path over the text and over the archive must print the same
	// thing — the archive is recognized by its magic, not its name.
	fromJSON, _, err := run(t, "query", "--values", "$[*].name", src)
	if err != nil {
		t.Fatal(err)
	}
	fromArchive, _, err := run(t, "query", "--values", "$[*].name", src+".jssc")
	if err != nil {
		t.Fatal(err)
	}
	if fromJSON != fromArchive {
		t.Errorf("JSON gave:\n%s\narchive gave:\n%s", fromJSON, fromArchive)
	}
	if want := "\"Alice\"\n\"Bob\"\n\"Carol\"\n"; fromJSON != want {
		t.Errorf("got %q, want %q", fromJSON, want)
	}
}

func TestQueryFlags(t *testing.T) {
	src := writeTemp(t, "users.json", usersJSON)
	if _, _, err := run(t, "compress", src); err != nil {
		t.Fatal(err)
	}
	archive := src + ".jssc"

	t.Run("paths", func(t *testing.T) {
		out, _, err := run(t, "query", "--paths", "$[*].name", archive)
		if err != nil {
			t.Fatal(err)
		}
		if want := "$[0].name\n$[1].name\n$[2].name\n"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
	t.Run("first", func(t *testing.T) {
		out, _, err := run(t, "query", "--first", "$[*].name", archive)
		if err != nil {
			t.Fatal(err)
		}
		if want := "\"Alice\"\n"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
	t.Run("limit", func(t *testing.T) {
		out, _, err := run(t, "query", "--values", "--limit", "2", "$[*].name", archive)
		if err != nil {
			t.Fatal(err)
		}
		if want := "\"Alice\"\n\"Bob\"\n"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
	t.Run("default prints path and value", func(t *testing.T) {
		out, _, err := run(t, "query", "$[0].name", archive)
		if err != nil {
			t.Fatal(err)
		}
		if want := "$[0].name\t\"Alice\"\n"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
	t.Run("paths and values conflict", func(t *testing.T) {
		if _, _, err := run(t, "query", "--paths", "--values", "$[*]", archive); err == nil {
			t.Error("expected an error when both flags are set")
		}
	})
	t.Run("no match with first", func(t *testing.T) {
		if _, _, err := run(t, "query", "--first", "$[*].absent", archive); err == nil {
			t.Error("expected an error when --first matches nothing")
		}
	})
}

func TestAlgorithms(t *testing.T) {
	out, _, err := run(t, "algorithms")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fsst", "deflate", "raw"} {
		if !strings.Contains(out, want) {
			t.Errorf("algorithms output is missing %q:\n%s", want, out)
		}
	}
}

func TestErrorsAreReported(t *testing.T) {
	cases := [][]string{
		{"compress", filepath.Join(t.TempDir(), "absent.json")},
		{"decompress", "not-an-archive.txt"},
		{"compress", "--format", "yaml", writeTemp(t, "x.json", "{}")},
		{"compress", "--structure", "zstd", writeTemp(t, "y.json", "{}")},
		{"info", filepath.Join(t.TempDir(), "absent.jssc")},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			if _, _, err := run(t, args...); err == nil {
				t.Errorf("expected an error for %v", args)
			}
		})
	}
}

func TestDecompressRejectsNonArchive(t *testing.T) {
	src := writeTemp(t, "plain.json.jssc", usersJSON)
	if _, _, err := run(t, "decompress", src); err == nil {
		t.Error("expected an error when the file is not an archive")
	}
}
