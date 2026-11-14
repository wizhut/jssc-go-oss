package jssc

import (
	"bytes"
	"io"
)

// Decode reads an archive from r and returns the reconstructed text: a JSON
// value, or newline-delimited documents, according to the archive's content
// format.
//
// The returned Stats describe the archive as stored, with RawBytes set to the
// size of the restored text.
func Decode(r io.Reader) ([]byte, *Stats, error) {
	a, err := Open(r)
	if err != nil {
		return nil, nil, err
	}
	return decodeArchive(a)
}

// DecodeBytes is Decode for an archive already in memory.
func DecodeBytes(raw []byte) ([]byte, *Stats, error) {
	a, err := OpenBytes(raw)
	if err != nil {
		return nil, nil, err
	}
	return decodeArchive(a)
}

func decodeArchive(a *Archive) ([]byte, *Stats, error) {
	out, err := a.MarshalText()
	if err != nil {
		return nil, nil, err
	}
	st := a.Stats()
	st.RawBytes = len(out)
	st.TotalBytes = st.HeaderBytes + st.StateBytes + 4*st.UniqueStrings + st.PoolBytes + 4 + st.StructBytes
	return out, st, nil
}

// Inspect reads only as much of an archive as the header and framing require,
// and reports what it found. It never decodes a pool entry, and never walks the
// structure.
func Inspect(raw []byte) (*Stats, error) {
	a, err := OpenBytes(raw)
	if err != nil {
		return nil, err
	}
	return a.Stats(), nil
}

// InspectFrom is Inspect for a reader.
func InspectFrom(r io.Reader) (*Stats, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return Inspect(raw)
}

// IsArchive reports whether raw begins with a known JSSC magic.
func IsArchive(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	switch string(raw[:4]) {
	case MagicV1, MagicV2, MagicV3, MagicV4, MagicV5, MagicV6, MagicV7, MagicV8:
		return true
	default:
		return false
	}
}

// EncodeToBytes is a convenience wrapper around EncodeWith that returns the
// archive rather than writing it.
func EncodeToBytes(raw []byte, opt Options) ([]byte, *Stats, error) {
	var buf bytes.Buffer
	st, err := EncodeWith(raw, &buf, opt)
	if err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), st, nil
}
