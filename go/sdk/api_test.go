package sdk_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/sdk"
)

// The SDK is mostly a thin surface over the format package. These tests check
// that every entry point is wired to the right thing, rather than re-testing
// the format itself.

func TestStreamEntryPoints(t *testing.T) {
	t.Run("CompressTo and DecompressFrom", func(t *testing.T) {
		var archive bytes.Buffer
		st, err := sdk.CompressTo([]byte(usersJSON), &archive, sdk.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		if st.TotalBytes != archive.Len() {
			t.Errorf("TotalBytes = %d, wrote %d bytes", st.TotalBytes, archive.Len())
		}
		text, _, err := sdk.DecompressFrom(bytes.NewReader(archive.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(jsonValue(t, []byte(usersJSON)), jsonValue(t, text)) {
			t.Error("round trip changed the value")
		}
	})

	t.Run("CompressStream", func(t *testing.T) {
		var archive bytes.Buffer
		st, err := sdk.CompressStream(bytes.NewReader([]byte(eventsNDJSON)), &archive, sdk.Options{
			Format:    sdk.FormatJSONL,
			Structure: sdk.StructureDeflate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if st.DocCount != 3 {
			t.Errorf("DocCount = %d, want 3", st.DocCount)
		}

		// A reader and a buffer of the same bytes must give the same archive.
		var direct bytes.Buffer
		if _, err := sdk.CompressTo([]byte(eventsNDJSON), &direct, sdk.Options{
			Format:    sdk.FormatJSONL,
			Structure: sdk.StructureDeflate,
		}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(archive.Bytes(), direct.Bytes()) {
			t.Error("CompressStream and CompressTo produced different archives")
		}
	})
}

func TestQueryVariantsAgree(t *testing.T) {
	archive, _, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	data := jsonValue(t, []byte(usersJSON))
	const path = "$.users[*].name"
	want := []any{"Alice", "Bob", "Carol"}
	wantPaths := []string{"$.users[0].name", "$.users[1].name", "$.users[2].name"}

	t.Run("values", func(t *testing.T) {
		for name, fn := range map[string]func() ([]any, error){
			"QueryValues":        func() ([]any, error) { return sdk.QueryValues([]byte(usersJSON), path) },
			"QueryDataValues":    func() ([]any, error) { return sdk.QueryDataValues(data, path) },
			"QueryArchiveValues": func() ([]any, error) { return sdk.QueryArchiveValues(archive, path) },
		} {
			got, err := fn()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		}
	})

	t.Run("paths", func(t *testing.T) {
		for name, fn := range map[string]func() ([]string, error){
			"QueryPaths":        func() ([]string, error) { return sdk.QueryPaths([]byte(usersJSON), path) },
			"QueryDataPaths":    func() ([]string, error) { return sdk.QueryDataPaths(data, path) },
			"QueryArchivePaths": func() ([]string, error) { return sdk.QueryArchivePaths(archive, path) },
		} {
			got, err := fn()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !reflect.DeepEqual(got, wantPaths) {
				t.Errorf("%s = %v, want %v", name, got, wantPaths)
			}
		}
	})

	t.Run("matches carry both", func(t *testing.T) {
		matches, err := sdk.Query([]byte(usersJSON), path)
		if err != nil {
			t.Fatal(err)
		}
		archiveMatches, err := sdk.QueryArchive(archive, path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(matches, archiveMatches) {
			t.Errorf("Query = %v, QueryArchive = %v", matches, archiveMatches)
		}
	})

	t.Run("one", func(t *testing.T) {
		for name, fn := range map[string]func() (any, bool, error){
			"QueryOne":        func() (any, bool, error) { return sdk.QueryOne([]byte(usersJSON), path) },
			"QueryDataOne":    func() (any, bool, error) { return sdk.QueryDataOne(data, path) },
			"QueryArchiveOne": func() (any, bool, error) { return sdk.QueryArchiveOne(archive, path) },
		} {
			v, ok, err := fn()
			if err != nil || !ok || v != "Alice" {
				t.Errorf("%s = %v, %v, %v; want Alice, true, nil", name, v, ok, err)
			}
		}
	})
}

func TestQueryRejectsBadPaths(t *testing.T) {
	archive, _, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	data := jsonValue(t, []byte(usersJSON))
	const bad = "$[["

	checks := map[string]func() error{
		"Query":             func() error { _, err := sdk.Query([]byte(usersJSON), bad); return err },
		"QueryValues":       func() error { _, err := sdk.QueryValues([]byte(usersJSON), bad); return err },
		"QueryPaths":        func() error { _, err := sdk.QueryPaths([]byte(usersJSON), bad); return err },
		"QueryData":         func() error { _, err := sdk.QueryData(data, bad); return err },
		"QueryDataValues":   func() error { _, err := sdk.QueryDataValues(data, bad); return err },
		"QueryDataPaths":    func() error { _, err := sdk.QueryDataPaths(data, bad); return err },
		"QueryDataOne":      func() error { _, _, err := sdk.QueryDataOne(data, bad); return err },
		"QueryOne":          func() error { _, _, err := sdk.QueryOne([]byte(usersJSON), bad); return err },
		"QueryArchive":      func() error { _, err := sdk.QueryArchive(archive, bad); return err },
		"QueryArchivePaths": func() error { _, err := sdk.QueryArchivePaths(archive, bad); return err },
		"QueryArchiveOne":   func() error { _, _, err := sdk.QueryArchiveOne(archive, bad); return err },
		"QueryArchiveLimit": func() error { _, err := sdk.QueryArchiveLimit(archive, bad, 1); return err },
	}
	for name, fn := range checks {
		if err := fn(); err == nil {
			t.Errorf("%s accepted a malformed path", name)
		}
	}
}

func TestQueryRejectsEmptyJSON(t *testing.T) {
	if _, err := sdk.Query(nil, "$"); err == nil {
		t.Error("Query accepted empty input")
	}
	if _, err := sdk.QueryValues([]byte("   "), "$"); err == nil {
		t.Error("QueryValues accepted whitespace-only input")
	}
	if _, _, err := sdk.QueryOne([]byte("not json"), "$"); err == nil {
		t.Error("QueryOne accepted malformed input")
	}
}

func TestQueryOnCorruptArchive(t *testing.T) {
	notAnArchive := []byte("this is not a jssc archive at all")
	if _, err := sdk.QueryArchive(notAnArchive, "$"); err == nil {
		t.Error("QueryArchive accepted a non-archive")
	}
	if _, err := sdk.OpenArchive(notAnArchive); err == nil {
		t.Error("OpenArchive accepted a non-archive")
	}
	if _, err := sdk.Inspect(notAnArchive); err == nil {
		t.Error("Inspect accepted a non-archive")
	}
	if _, _, err := sdk.Decompress(notAnArchive); err == nil {
		t.Error("Decompress accepted a non-archive")
	}
}

func TestRatio(t *testing.T) {
	_, st, err := sdk.Compress([]byte(usersJSON))
	if err != nil {
		t.Fatal(err)
	}
	if got := sdk.Ratio(st); got != st.Ratio() {
		t.Errorf("Ratio = %v, Stats.Ratio = %v", got, st.Ratio())
	}
	if sdk.Ratio(&sdk.Stats{}) != 0 {
		t.Error("Ratio of an empty Stats should be 0")
	}
}
