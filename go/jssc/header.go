package jssc

import (
	"encoding/binary"
	"io"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

// header is the fixed-size prefix of an archive. Which fields are actually
// present on the wire depends on version; the rest carry their implied values
// (specification §5).
type header struct {
	version     int // 1 through 8
	algo        codec.ID
	structCodec StructCodec
	layout      Layout
	indexWidth  int
	contentFmt  ContentFormat
	// keyCount is the key pool's cardinality (JSS8, 5.3.0-draft); the key
	// width derives from it. Earlier versions have one pool and no field.
	keyCount  uint32
	poolCount uint32
	docCount  uint32
	// groupShift is the JSS5 pool group size exponent. Earlier versions imply
	// zero: one entry per group, which is what they encode.
	groupShift uint8
}

// groupSize is how many pool entries share one compressed group.
func (h header) groupSize() int { return 1 << h.groupShift }

// varintPool reports whether pool groups are framed with LEB128 lengths rather
// than the uint32 of JSS1–JSS4.
func (h header) varintPool() bool { return h.version >= 5 }

// adaptiveOffsets reports whether containers carry an offset-width descriptor.
func (h header) adaptiveOffsets() bool { return h.version >= 5 }

// keyDir reports whether a key hash directory sits between the pool and the
// structure. JSS6 always writes the section; an encoder that declines to build
// a directory writes a count of zero rather than omitting the field.
func (h header) keyDir() bool { return h.version >= 6 }

// blockedStructure reports whether the structure region is a block index
// followed by independently inflatable blocks, rather than one stored stream.
func (h header) blockedStructure() bool { return h.version >= 7 }

// rootOffsets reports whether the per-document root table holds absolute plain
// offsets rather than lengths. JSS7 changed it so that reaching document i is a
// single read instead of a prefix sum over every document before it.
func (h header) rootOffsets() bool { return h.version >= 7 }

// poolSectionLength reports whether the pool section opens with its own
// uint32 stored length, so a reader finds the key directory without walking
// the group framing (specification §7.4).
func (h header) poolSectionLength() bool { return h.version >= 8 }

// groupDirectory reports whether each pool section carries a group directory
// between its stored-raw flags and its groups, so a reader reaches any group by
// walking one 64-group segment (specification §7.4, 5.6.0-draft).
func (h header) groupDirectory() bool { return h.version >= 8 }

// splitPools reports whether keys and values live in separate pools with
// separate index spaces (specification §7, 5.3.0-draft).
func (h header) splitPools() bool { return h.version >= 8 }

// keyWidth is the width of a key-pool reference: derived from key_count where
// the pools are split, the shared index width before that (specification
// §5.5).
func (h header) keyWidth() int {
	if h.splitPools() {
		return keyWidthFor(int(h.keyCount))
	}
	return h.indexWidth
}

// opportunisticPool reports whether a pool group's framed length carries the
// stored-raw flag in its low bit. Through JSS7 every group is compressed whether
// or not that helped, which on short strings costs bytes and a decompression to
// get them back.
func (h header) opportunisticPool() bool { return h.version >= 8 }

// completeKeyDirectory reports whether the directory lists every key even when
// two of them share a hash. Through JSS7 a collision made the encoder write an
// empty directory, so one unlucky pair of keys silently cost the whole document
// its integer key lookups; JSS8 keeps the directory and verifies only the
// colliding run.
func (h header) completeKeyDirectory() bool { return h.version >= 8 }

// pathFilter reports whether a key-path filter section sits between the key
// directory and the structure.
func (h header) pathFilter() bool { return h.version >= 8 && h.layout&LayoutPathFilter != 0 }

// layoutMask is the set of struct_layout bits this version defines. Bits outside
// it are reserved: a writer zeroes them and a reader ignores them.
func (h header) layoutMask() Layout {
	if h.version >= 8 {
		return layoutStoredV8
	}
	return layoutStored
}

func (h header) magic() string {
	switch h.version {
	case 1:
		return MagicV1
	case 2:
		return MagicV2
	case 3:
		return MagicV3
	case 4:
		return MagicV4
	case 5:
		return MagicV5
	case 6:
		return MagicV6
	case 7:
		return MagicV7
	default:
		return MagicV8
	}
}

func (h header) size() int {
	switch h.version {
	case 1:
		return headerSizeV1
	case 2:
		return headerSizeV2
	case 3:
		return headerSizeV3
	case 4:
		return headerSizeV4
	case 5:
		return headerSizeV5
	case 6:
		return headerSizeV6
	case 7:
		return headerSizeV7
	default:
		return headerSizeV8
	}
}

// marshal renders the header exactly as it appears at the head of an archive.
func (h header) marshal() []byte {
	b := make([]byte, 0, h.size())
	b = append(b, h.magic()...)
	b = append(b, uint8(h.algo))
	if h.version >= 2 {
		b = append(b, uint8(h.structCodec))
	}
	if h.version >= 3 {
		b = append(b, uint8(h.layout&h.layoutMask()))
	}
	b = append(b, uint8(h.indexWidth))
	if h.version >= 4 {
		b = append(b, uint8(h.contentFmt))
	}
	if h.version >= 5 {
		b = append(b, h.groupShift)
	}
	if h.version >= 8 {
		b = binary.LittleEndian.AppendUint32(b, h.keyCount)
	}
	b = binary.LittleEndian.AppendUint32(b, h.poolCount)
	if h.version >= 4 {
		b = binary.LittleEndian.AppendUint32(b, h.docCount)
	}
	return b
}

func (h header) writeTo(w io.Writer) error {
	_, err := w.Write(h.marshal())
	return err
}

// parseHeader reads the fixed header from the front of raw and returns it along
// with the offset of the first byte after it (where codec_state starts).
func parseHeader(raw []byte) (header, int, error) {
	if len(raw) < 4 {
		return header{}, 0, errTruncatedf("shorter than a magic")
	}
	h := header{docCount: 1, contentFmt: FormatJSON}
	switch string(raw[:4]) {
	case MagicV1:
		h.version = 1
	case MagicV2:
		h.version = 2
	case MagicV3:
		h.version = 3
	case MagicV4:
		h.version = 4
	case MagicV5:
		h.version = 5
	case MagicV6:
		h.version = 6
	case MagicV7:
		h.version = 7
	case MagicV8:
		h.version = 8
	default:
		return header{}, 0, ErrBadMagic
	}
	if len(raw) < h.size() {
		return header{}, 0, errTruncatedf("header needs %d bytes, have %d", h.size(), len(raw))
	}

	pos := 4
	h.algo = codec.ID(raw[pos])
	pos++

	// JSS1 predates struct_codec; its structure is always stored raw.
	h.structCodec = StructRaw
	if h.version >= 2 {
		h.structCodec = StructCodec(raw[pos])
		pos++
		if !h.structCodec.valid() {
			return header{}, 0, errUnsupportedf("structure codec %d", uint8(h.structCodec))
		}
	}

	// Reserved struct_layout bits are ignored on read (specification §5.3).
	if h.version >= 3 {
		h.layout = Layout(raw[pos]) & h.layoutMask()
		pos++
	}

	h.indexWidth = int(raw[pos])
	pos++
	if !validIndexWidth(h.indexWidth) {
		return header{}, 0, errUnsupportedf("index width %d (want 1, 2, or 4)", h.indexWidth)
	}

	if h.version >= 4 {
		h.contentFmt = ContentFormat(raw[pos])
		pos++
		if h.contentFmt > FormatNDJSON {
			return header{}, 0, errUnsupportedf("content format %d", uint8(h.contentFmt))
		}
	}

	if h.version >= 5 {
		h.groupShift = raw[pos]
		pos++
		if h.groupShift > MaxPoolGroupShift {
			return header{}, 0, errUnsupportedf("pool group shift %d (max %d)", h.groupShift, MaxPoolGroupShift)
		}
	}

	if h.version >= 8 {
		h.keyCount = binary.LittleEndian.Uint32(raw[pos:])
		pos += 4
	}

	h.poolCount = binary.LittleEndian.Uint32(raw[pos:])
	pos += 4

	if h.version >= 4 {
		h.docCount = binary.LittleEndian.Uint32(raw[pos:])
		pos += 4
		if h.docCount == 0 {
			return header{}, 0, errCorruptf("doc_count is zero")
		}
	}
	return h, pos, nil
}
