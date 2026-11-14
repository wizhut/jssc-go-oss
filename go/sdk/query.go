package sdk

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ohler55/ojg/jp"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// Match is one JSONPath hit: the normalized path and the value found there.
type Match = jssc.Match

// Query evaluates a JSONPath expression against JSON bytes and returns every
// match with its normalized path and value.
//
// The syntax is the Goessner / IETF JSONPath family as implemented by
// github.com/ohler55/ojg — for example "$.users[*].name" or "$..role".
func Query(jsonBytes []byte, path string) ([]Match, error) {
	data, err := decodeJSON(jsonBytes)
	if err != nil {
		return nil, err
	}
	return QueryData(data, path)
}

// QueryValues is Query returning only the matched values.
func QueryValues(jsonBytes []byte, path string) ([]any, error) {
	data, err := decodeJSON(jsonBytes)
	if err != nil {
		return nil, err
	}
	return QueryDataValues(data, path)
}

// QueryPaths is Query returning only the normalized paths.
func QueryPaths(jsonBytes []byte, path string) ([]string, error) {
	matches, err := Query(jsonBytes, path)
	if err != nil {
		return nil, err
	}
	return pathsOf(matches), nil
}

// QueryOne returns the first value matched by path, or (nil, false, nil).
func QueryOne(jsonBytes []byte, path string) (any, bool, error) {
	data, err := decodeJSON(jsonBytes)
	if err != nil {
		return nil, false, err
	}
	return QueryDataOne(data, path)
}

// QueryData evaluates a JSONPath against an already-decoded value, typically a
// map[string]any or []any from encoding/json.
func QueryData(data any, path string) ([]Match, error) {
	expr, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	locs := expr.Locate(data, 0)
	out := make([]Match, 0, len(locs))
	for _, loc := range locs {
		out = append(out, Match{Path: loc.String(), Value: loc.First(data)})
	}
	return out, nil
}

// QueryDataValues returns only the values matched against data.
func QueryDataValues(data any, path string) ([]any, error) {
	expr, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	return expr.Get(data), nil
}

// QueryDataPaths returns only the normalized paths matched against data.
func QueryDataPaths(data any, path string) ([]string, error) {
	matches, err := QueryData(data, path)
	if err != nil {
		return nil, err
	}
	return pathsOf(matches), nil
}

// QueryDataOne returns the first value matched against data.
func QueryDataOne(data any, path string) (any, bool, error) {
	expr, err := parsePath(path)
	if err != nil {
		return nil, false, err
	}
	v, ok := expr.FirstFound(data)
	return v, ok, nil
}

// OpenArchive indexes an archive for selective access. Pool strings are decoded
// on demand; the structure block is inflated once if it was deflated.
//
// Reuse the returned archive across queries when querying the same bytes more
// than once — it caches decoded strings and container offsets.
func OpenArchive(archive []byte) (*jssc.Archive, error) {
	return jssc.OpenBytes(archive)
}

// QueryArchive evaluates a JSONPath against an archive by walking the binary
// structure, decoding only the pool strings the path requires.
func QueryArchive(archive []byte, path string) ([]Match, error) {
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		return nil, err
	}
	return a.Query(path)
}

// QueryArchiveValues is QueryArchive returning only matched values.
func QueryArchiveValues(archive []byte, path string) ([]any, error) {
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		return nil, err
	}
	return a.QueryValues(path)
}

// QueryArchivePaths is QueryArchive returning only normalized paths.
func QueryArchivePaths(archive []byte, path string) ([]string, error) {
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		return nil, err
	}
	return a.QueryPaths(path)
}

// QueryArchiveOne returns the first value matched in an archive, stopping the
// structure walk as soon as it is found.
func QueryArchiveOne(archive []byte, path string) (any, bool, error) {
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		return nil, false, err
	}
	return a.QueryOne(path)
}

// QueryArchiveLimit is QueryArchive, stopping after limit matches. A limit of
// zero or less means unlimited.
func QueryArchiveLimit(archive []byte, path string, limit int) ([]Match, error) {
	a, err := jssc.OpenBytes(archive)
	if err != nil {
		return nil, err
	}
	return a.QueryLimit(path, limit)
}

func parsePath(path string) (jp.Expr, error) {
	expr, err := jp.ParseString(path)
	if err != nil {
		return nil, fmt.Errorf("jsonpath %q: %w", path, err)
	}
	return expr, nil
}

func pathsOf(matches []Match) []string {
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.Path
	}
	return out
}

func decodeJSON(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty json")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data any
	if err := dec.Decode(&data); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	return data, nil
}
