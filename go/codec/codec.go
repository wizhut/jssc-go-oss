// Package codec is the registry of JSSC string-pool codecs.
//
// A pool codec compresses each pool entry independently, so decoding entry i
// never requires decoding entry j. FSST learns a symbol table from the pool and
// persists it as codec state in the archive.
//
// Small-string compression is the whole point: a pool entry is a JSON key or
// string value, usually tens of bytes, and it must decompress on its own when a
// query reaches it. A general compressor cannot do that — it needs a window
// spanning many entries to beat the per-stream overhead, and paying for that
// window means inflating neighbours nobody asked for. A learned symbol table
// shared through codec state is the shape that works at this size.
//
// The registry is keyed by the algo byte in the archive header. IDs 1, 2 and 3
// are retired — unishox (stateless, retired on measurement), and pool-wide
// DEFLATE and LZ4, which compressed a whole group at a time — and must not be
// reissued. IDs 4–255 are reserved for future registration (specification §5.1).
package codec

import (
	"fmt"
	"io"
	"strings"
)

// ID identifies a pool codec in the archive header's algo field.
type ID uint8

const (
	// IDFSST is a learned Fast Static Symbol Table, compressing each entry
	// against a shared table.
	IDFSST ID = 0

	// ID 1 was Unishox2 with the JSON preset, stateless. It played by the
	// per-entry rule and was retired on measurement: with no training stage it
	// cannot learn a corpus's own symbols, and it produced pools 20–48% larger
	// than fsst on every corpus with a non-trivial pool while decoding slower.
	//
	// IDs 2 and 3 were raw DEFLATE and the LZ4 block format, each applied to a
	// whole pool group rather than an entry. Both are removed: a group-wide
	// codec makes reaching one string cost its 63 neighbours, which is the
	// opposite of what the pool is for.
	//
	// All three IDs stay burned so an archive written by an older encoder is
	// rejected rather than misread.
)

// DefaultName is the codec used when a caller names none.
const DefaultName = "fsst"

// PerEntryName is the codec to use when a pool entry must decompress without
// touching any other. Every registered codec now works that way, so this is
// DefaultName; it is kept because callers name it to say why they chose it.
const PerEntryName = DefaultName

// Appender is a codec that can decode into a caller-supplied buffer, so a
// reader that decodes many entries pays one allocation instead of one per
// entry. Implementing it is optional: a reader type-asserts for it and falls
// back to Decode.
type Appender interface {
	DecodeAppend(dst, src []byte) ([]byte, error)
}

// Codec compresses and decompresses individual pool entries.
//
// The encode side is used as Train, then Encode per entry, then WriteState.
// The decode side is used as ReadState, then Decode per entry, in any order and
// possibly never for a given entry.
type Codec interface {
	ID() ID
	Name() string
	Description() string

	// Train prepares the codec from the strings it will be asked to encode.
	// Stateless codecs ignore the samples.
	Train(samples [][]byte) error

	Encode(src []byte) ([]byte, error)
	Decode(src []byte) ([]byte, error)

	// WriteState serializes whatever the decoder needs to reproduce Decode.
	// Stateless codecs write nothing.
	WriteState(w io.Writer) error

	// ReadState consumes exactly the bytes WriteState produced. Its own framing
	// defines that length: the archive does not store it separately.
	ReadState(r io.Reader) error
}

type entry struct {
	id          ID
	name        string
	description string
	newFn       func() Codec
	// unavailable, when set, explains why this build cannot use the codec.
	unavailable string
}

// registry is append-only and ordered by ID. Build-tagged files may replace an
// entry's constructor with a stub that reports why it is unavailable.
var registry = []entry{
	{
		id:          IDFSST,
		name:        "fsst",
		description: "Fast Static Symbol Table (learned symbols, good for repetitive JSON)",
		newFn:       newFSST,
	},
}

// Info describes a registered codec.
type Info struct {
	ID          ID
	Name        string
	Description string
	// Available reports whether this build can actually use the codec.
	Available bool
}

// List returns metadata for every registered codec, ordered by ID.
func List() []Info {
	out := make([]Info, len(registry))
	for i, e := range registry {
		out[i] = Info{
			ID:          e.id,
			Name:        e.name,
			Description: e.description,
			Available:   e.unavailable == "",
		}
	}
	return out
}

// Names returns the registered codec names, ordered by ID.
func Names() []string {
	out := make([]string, len(registry))
	for i, e := range registry {
		out[i] = e.name
	}
	return out
}

func (e entry) instantiate() (Codec, error) {
	if e.unavailable != "" {
		return nil, fmt.Errorf("codec %q (id %d) is not available: %s", e.name, e.id, e.unavailable)
	}
	return e.newFn(), nil
}

// NewByName returns a fresh codec for the given name. The empty string selects
// DefaultName.
func NewByName(name string) (Codec, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = DefaultName
	}
	for _, e := range registry {
		if e.name == key {
			return e.instantiate()
		}
	}
	return nil, fmt.Errorf("unknown pool codec %q (registered: %s)", name, strings.Join(Names(), ", "))
}

// retired names the codecs that were registered once and removed. An archive
// using one is well formed and was conformant when written, so saying which
// codec it wants is more useful than calling the id unknown (specification
// §5.1).
var retired = map[ID]string{
	1: "unishox, a stateless short-string codec retired on measurement — " +
		"fsst compressed every evaluated corpus better (specification §5.1)",
	2: "deflate, a raw DEFLATE stream over a whole pool group, " +
		"which the registry forbids",
	3: "lz4, an LZ4 block over a whole pool group, which the registry forbids",
}

// NewByID returns a fresh codec for the algo byte read from an archive header.
func NewByID(id ID) (Codec, error) {
	for _, e := range registry {
		if e.id == id {
			return e.instantiate()
		}
	}
	if what, ok := retired[id]; ok {
		return nil, fmt.Errorf("pool codec id %d is retired: %s. "+
			"This archive cannot be read by this version; re-encode it "+
			"with a build that still had the codec", id, what)
	}
	return nil, fmt.Errorf("unknown pool codec id %d", id)
}
