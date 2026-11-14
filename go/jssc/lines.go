package jssc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxLineBytes bounds a single JSONL/NDJSON line during streaming
// encodes. Real corpora (GitHub Archive, for one) carry large lines, so the
// default is generous.
const DefaultMaxLineBytes = 16 << 20 // 16 MiB

// parseJSONValue decodes exactly one JSON value, keeping number lexemes intact
// and rejecting trailing content.
func parseJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errInvalidf("parse json: %v", err)
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return v, nil
	case err == nil:
		return nil, errInvalidf("trailing data after json value")
	default:
		return nil, errInvalidf("trailing data after json value: %v", err)
	}
}

// parseLineDocs splits newline-delimited JSON into one value per non-empty
// line. It reports whether maxDocs cut the input short.
func parseLineDocs(raw []byte, maxDocs int) (docs []any, truncated bool, err error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, errInvalidf("empty input")
	}
	err = forEachLineDoc(bytes.NewReader(raw), maxDocs, len(raw)+1, func(_ []byte, doc any) error {
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if len(docs) == 0 {
		return nil, false, errInvalidf("empty input")
	}
	return docs, maxDocs > 0 && len(docs) == maxDocs, nil
}

// marshalLineDocs emits docs as newline-delimited JSON, one value per line with
// a trailing newline.
func marshalLineDocs(docs []any) ([]byte, error) {
	var buf bytes.Buffer
	for _, d := range docs {
		b, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// forEachLineDoc scans r as JSONL/NDJSON and calls fn once per non-empty line
// with the line bytes and the parsed value. Empty lines are skipped; a line
// that is not exactly one JSON value fails the scan.
//
// The line slice handed to fn is owned by the caller and stays valid after fn
// returns.
func forEachLineDoc(r io.Reader, maxDocs, maxLineBytes int, fn func(line []byte, doc any) error) error {
	if maxLineBytes <= 0 || maxLineBytes > DefaultMaxLineBytes {
		maxLineBytes = DefaultMaxLineBytes
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	lineNum, docs := 0, 0
	for sc.Scan() {
		lineNum++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		// Scanner reuses its buffer between calls; fn may retain the bytes.
		owned := bytes.Clone(line)
		v, err := parseJSONValue(owned)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNum, err)
		}
		if err := fn(owned, v); err != nil {
			return err
		}
		docs++
		if maxDocs > 0 && docs >= maxDocs {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errInvalidf("line %d exceeds %d bytes", lineNum+1, maxLineBytes)
		}
		return err
	}
	return nil
}
