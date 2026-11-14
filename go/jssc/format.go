package jssc

import (
	"fmt"
	"math"
	"strings"
)

// SpecVersion is the JSSC specification revision this package implements.
//
// 5.x-draft makes JSS8 the normative container — opportunistic pool groups, a
// collision-surviving key directory, the key-path filter, lz4 structure
// blocks cut inside documents, self-describing pool sections, and (5.3.0)
// keys and values in separate pools with separate index widths. Its appendix
// describes the draft versions JSS1–JSS7 for reference; this package neither
// reads nor writes them.
//
// 4.0.0-draft restricted the pool codec registry to small-string codecs and
// retired ids 2 and 3; 5.4.0-draft retired unishox (id 1) on measurement,
// leaving fsst the sole registered codec. The default pool group is one entry.
// 5.6.0-draft gave each JSS8 pool section a group directory, so the first
// string read walks one 64-group segment rather than the whole pool.
const SpecVersion = "5.6.0-draft"

// MagicV8 is the magic every archive starts with: the format version this
// package reads and writes. The earlier draft versions JSS1–JSS7 are described
// in the specification's Appendix E for reference only.
const MagicV8 = "JSS8" // opportunistic pool groups, complete key directory, path filter

// LatestVersion is the archive version this package reads and writes, JSS8.
const LatestVersion = 8

// headerSizeV8 is the fixed header: magic, algo, struct_codec, struct_layout,
// index_width, content_fmt, pool_group_shift, then key_count, pool_count and
// doc_count as uint32 (specification §4.1).
const headerSizeV8 = 4 + 1 + 1 + 1 + 1 + 1 + 1 + 4 + 4 + 4

// Structure blocking (specification §8A). The structure is compressed in independently
// inflatable blocks so that reaching one document inflates one block.
const (
	// DefaultStructBlockBytes is the target plain size of a structure block.
	//
	// Measured over a million-row archive: 64 KiB costs 3.4% of the archive
	// against a single stream and brings a cold point read from 186 ms and
	// 270 MiB down to 63 microseconds and 64 KiB. Larger blocks recover most of
	// that 3.4% — 256 KiB costs 0.8% — at proportionally more work per read.
	DefaultStructBlockBytes = 64 << 10

	// MinStructBlockBytes floors the target. Below about this, per-block framing
	// and lost compression context cost more than the seek is worth.
	MinStructBlockBytes = 4 << 10
)

// Key hash directory (specification §7A). An archive carries, between the
// pools and the structure, a table mapping the hash of every object key to its
// key-pool index. A field lookup can then resolve "$.field" to an
// integer without decompressing any pool group at all, and can answer "no such
// key anywhere in this document" without touching the pool.
const (
	// KeyHashCRC32 identifies the hash: CRC-32 with the IEEE polynomial, the
	// same function as hash/crc32's ChecksumIEEE.
	//
	// CRC-32 rather than CRC-16 because every collision costs a comparison
	// against the pool. Measured over real-world corpora, CRC-16 collides on 2
	// of 321 keys in citm_catalog and 2 of 685 in update-center; CRC-32
	// collides on none.
	KeyHashCRC32 = "crc32-ieee"

	// MaxKeyDirEntries bounds the directory, which is sized from a uint32 read
	// before any allocation.
	MaxKeyDirEntries = math.MaxUint32
)

// Pool grouping (specification §5.6, §7.3). Entries are compressed in groups of 1<<pool_group_shift.
// A group is the unit a reader must decompress to reach any entry inside it, so
// the shift trades archive size against the cost of one cold string read.
const (
	// MaxPoolGroupShift bounds the group size at 4096 entries.
	MaxPoolGroupShift = 12

	// DefaultPoolGroupShift is 0 — one entry per group, so a string decodes
	// alone and lazily.
	//
	// It was 6 while a group-wide DEFLATE could span 64 sorted neighbours and
	// find the redundancy between them. With the pool codecs now small-string
	// codecs only, nothing spans entries, and a group larger than one buys no
	// compression while still forcing a reader to walk its neighbours' framing
	// to reach the one string it wanted. A larger shift remains settable, but
	// on the current codec set it only costs.
	DefaultPoolGroupShift = 0
)

// Structure tags (specification §8.2).
const (
	tagNull   = 0x00
	tagFalse  = 0x01
	tagTrue   = 0x02
	tagNumber = 0x03
	tagString = 0x04
	tagArray  = 0x05
	tagObject = 0x06

	// tagInline0+n holds an n-byte string value written inline, n in [0,4].
	tagInline0   = 0x07
	tagInlineMax = tagInline0 + MaxIndexWidth
)

// Format limits (specification §14).
const (
	// MaxIndexWidth is the widest pool index, and therefore the longest string
	// value that can be written inline.
	MaxIndexWidth = 4

	// MaxNumberLexeme is the longest JSON number text a number tag can carry,
	// bounded by its single-byte length prefix.
	MaxNumberLexeme = math.MaxUint8

	// MaxPoolCount is the largest pool cardinality the header can express.
	MaxPoolCount = math.MaxUint32

	// MaxStoredStructure is the largest structure block struct_len can express.
	MaxStoredStructure = math.MaxUint32

	// MinOffsetChildren is the container size at which LayoutOffsets starts
	// emitting a child offset table, together with MinOffsetPayload: a table is
	// only worth its bytes when the linear skip it replaces would traverse a
	// meaningful amount of the document, and the median container in real JSON
	// holds four children (specification §8.4).
	MinOffsetChildren = 8

	// MinOffsetPayload is the second condition: a container carries an
	// offset table only if its children span at least this many bytes.
	// Measured on a GeoJSON corpus, adding it cut offset-table bytes from
	// 23,112 to 856 by declining to index hundreds of small coordinate arrays.
	MinOffsetPayload = 2048

	maxPoolWidth1 = 256
	maxPoolWidth2 = 65536
)

// Offset table entry widths (specification §8.3). A container stores one descriptor byte
// after its count saying how wide its table entries are, so a container whose
// children span less than 256 bytes pays one byte per child instead of four.
const (
	offsetsNone = 0 // no table for this container
)

// offsetWidthFor returns the narrowest table entry width that can address a
// child span of the given size.
func offsetWidthFor(span int) int {
	switch {
	case span <= math.MaxUint8:
		return 1
	case span <= math.MaxUint16:
		return 2
	default:
		return 4
	}
}

func validOffsetWidth(w int) bool {
	return w == offsetsNone || w == 1 || w == 2 || w == 4
}

// StructCodec selects how the binary structure block is stored. It is
// independent of the pool codec: only the structure is affected.
type StructCodec uint8

const (
	// StructRaw stores the structure verbatim, so a reader can traverse it in
	// place without inflating anything.
	StructRaw StructCodec = 0
	// StructDeflate stores the structure as a raw DEFLATE stream (RFC 1951,
	// no zlib wrapper). The smallest of the three, and the slowest to enter:
	// the right choice for cold storage.
	StructDeflate StructCodec = 1
	// StructLZ4 stores each structure block as an LZ4 block, or verbatim when
	// compression would not shrink it — a stored length equal to the plain
	// length is the signal (specification §8A). The default: it keeps about
	// two thirds of deflate's structure ratio while a cold read decompresses
	// its block an order of magnitude faster. JSS8 only.
	StructLZ4 StructCodec = 2
)

func (s StructCodec) String() string {
	switch s {
	case StructRaw:
		return "raw"
	case StructDeflate:
		return "deflate"
	case StructLZ4:
		return "lz4"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(s))
	}
}

func (s StructCodec) valid() bool {
	return s == StructRaw || s == StructDeflate || s == StructLZ4
}

// ParseStructCodec resolves a structure codec name. The empty string selects
// the default, StructLZ4.
func ParseStructCodec(name string) (StructCodec, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "lz4":
		return StructLZ4, nil
	case "deflate":
		return StructDeflate, nil
	case "raw", "none":
		return StructRaw, nil
	default:
		return 0, fmt.Errorf("%w: structure codec %q (want raw, deflate or lz4)", ErrUnsupported, name)
	}
}

// Layout is the struct_layout bitmask of walk aids stored in the header
// (specification §5.3).
type Layout uint8

const (
	// LayoutLenPrefix precedes every value with a LEB128 payload length, so a
	// reader can skip a subtree without parsing it.
	LayoutLenPrefix Layout = 1 << 0
	// LayoutOffsets gives containers of MinOffsetChildren or more children a
	// table of child offsets, so element and field lookup is O(1)/O(log n)
	// instead of a linear walk.
	LayoutOffsets Layout = 1 << 1

	// LayoutPathFilter announces the JSS8 key-path filter section between the key
	// directory and the structure. It is a layout bit rather than a header field
	// because the section is genuinely optional: an encoder emits it only when it
	// is small relative to what it indexes, and a reader that finds the bit clear
	// simply walks as it always did.
	LayoutPathFilter Layout = 1 << 2

	// LayoutDefault is what a conformant encoder writes unless told otherwise.
	// The path filter is not in it: whether the section earns its bytes is a
	// per-document question the encoder answers, not a mode the caller selects.
	LayoutDefault = LayoutLenPrefix | LayoutOffsets

	// layoutStored is the walk-aid bits a caller may ask for.
	layoutStored = LayoutLenPrefix | LayoutOffsets
	// layoutStoredV8 is every bit JSS8 defines: the walk aids and the path
	// filter bit the encoder sets. Bits 3–7 are reserved: writers zero them,
	// readers ignore them (specification §5.3).
	layoutStoredV8 = layoutStored | LayoutPathFilter
)

func (l Layout) String() string {
	if l == 0 {
		return "none"
	}
	var parts []string
	if l&LayoutLenPrefix != 0 {
		parts = append(parts, "lenprefix")
	}
	if l&LayoutOffsets != 0 {
		parts = append(parts, "offsets")
	}
	if l&LayoutPathFilter != 0 {
		parts = append(parts, "pathfilter")
	}
	if rest := l &^ layoutStoredV8; rest != 0 {
		parts = append(parts, fmt.Sprintf("reserved(0x%02x)", uint8(rest)))
	}
	return strings.Join(parts, "+")
}

// ContentFormat names the textual document form stored in, and restored from,
// an archive. Callers must set it explicitly: this package never sniffs
// filenames or content (specification §10.5).
type ContentFormat uint8

const (
	// FormatJSON is a single JSON value.
	FormatJSON ContentFormat = 0
	// FormatJSONL is newline-delimited JSON: one archive, one document per line.
	FormatJSONL ContentFormat = 1
	// FormatNDJSON has the same wire encoding as FormatJSONL; the distinct
	// label is preserved so decompression restores the caller's chosen name.
	FormatNDJSON ContentFormat = 2
)

func (f ContentFormat) String() string {
	switch f {
	case FormatJSON:
		return "json"
	case FormatJSONL:
		return "jsonl"
	case FormatNDJSON:
		return "ndjson"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(f))
	}
}

// LineOriented reports whether f is a multi-document newline format.
func (f ContentFormat) LineOriented() bool {
	return f == FormatJSONL || f == FormatNDJSON
}

// Extension is the preferred file suffix for f.
func (f ContentFormat) Extension() string {
	switch f {
	case FormatJSONL:
		return ".jsonl"
	case FormatNDJSON:
		return ".ndjson"
	default:
		return ".json"
	}
}

// ParseContentFormat resolves "json", "jsonl", or "ndjson". The empty string
// selects FormatJSON.
func ParseContentFormat(name string) (ContentFormat, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "json":
		return FormatJSON, nil
	case "jsonl":
		return FormatJSONL, nil
	case "ndjson":
		return FormatNDJSON, nil
	default:
		return 0, fmt.Errorf("%w: content format %q (want json, jsonl, or ndjson)", ErrUnsupported, name)
	}
}

// keyWidthFor returns the key width: the narrowest index width addressing the
// key pool, with an empty pool taking width 1 (specification §5.5).
func keyWidthFor(k int) int {
	if k <= 0 {
		return 1
	}
	return indexWidthFor(k)
}

// indexWidthFor returns the narrowest pool index width that addresses n entries.
func indexWidthFor(n int) int {
	switch {
	case n <= maxPoolWidth1:
		return 1
	case n <= maxPoolWidth2:
		return 2
	default:
		return MaxIndexWidth
	}
}

func validIndexWidth(w int) bool {
	return w == 1 || w == 2 || w == 4
}
