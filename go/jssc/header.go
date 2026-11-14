package jssc

import (
	"encoding/binary"
	"io"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

// header is the fixed-size prefix of an archive (specification §4.1). This
// package reads and writes JSS8 alone; the earlier versions are described in
// the specification's Appendix E for reference and are refused on open.
type header struct {
	algo        codec.ID
	structCodec StructCodec
	layout      Layout
	indexWidth  int
	contentFmt  ContentFormat
	groupShift  uint8
	// keyCount is the key pool's cardinality; the key width derives from it
	// (specification §5.5).
	keyCount  uint32
	poolCount uint32
	docCount  uint32
}

// groupSize is how many pool entries share one compressed group.
func (h header) groupSize() int { return 1 << h.groupShift }

// keyWidth is the width of a key-pool reference, derived from key_count
// (specification §5.5).
func (h header) keyWidth() int { return keyWidthFor(int(h.keyCount)) }

// pathFilter reports whether a key-path filter section sits between the key
// directory and the structure (specification §5.3, §7B).
func (h header) pathFilter() bool { return h.layout&LayoutPathFilter != 0 }

func (h header) size() int { return headerSizeV8 }

// marshal renders the header exactly as it appears at the head of an archive.
func (h header) marshal() []byte {
	b := make([]byte, 0, headerSizeV8)
	b = append(b, MagicV8...)
	b = append(b, uint8(h.algo), uint8(h.structCodec), uint8(h.layout&layoutStoredV8),
		uint8(h.indexWidth), uint8(h.contentFmt), h.groupShift)
	b = binary.LittleEndian.AppendUint32(b, h.keyCount)
	b = binary.LittleEndian.AppendUint32(b, h.poolCount)
	b = binary.LittleEndian.AppendUint32(b, h.docCount)
	return b
}

func (h header) writeTo(w io.Writer) error {
	_, err := w.Write(h.marshal())
	return err
}

// earlierMagic reports whether m is the magic of one of the draft versions
// JSS1–JSS7 (specification Appendix E). They are recognised only so that
// opening one says what it is rather than calling it garbage.
func earlierMagic(m string) bool {
	return len(m) == 4 && m[:3] == "JSS" && m[3] >= '1' && m[3] <= '7'
}

// parseHeader reads the fixed header from the front of raw and returns it along
// with the offset of the first byte after it (where codec_state starts),
// applying the checks of specification §4.1.
func parseHeader(raw []byte) (header, int, error) {
	if len(raw) < 4 {
		return header{}, 0, errTruncatedf("shorter than a magic")
	}
	switch m := string(raw[:4]); {
	case m == MagicV8:
	case earlierMagic(m):
		return header{}, 0, errUnsupportedf("archive version %s: this implementation reads %s only", m, MagicV8)
	default:
		return header{}, 0, ErrBadMagic
	}
	if len(raw) < headerSizeV8 {
		return header{}, 0, errTruncatedf("header needs %d bytes, have %d", headerSizeV8, len(raw))
	}

	h := header{
		algo:        codec.ID(raw[4]),
		structCodec: StructCodec(raw[5]),
		// Reserved struct_layout bits are ignored on read (specification §5.3).
		layout:     Layout(raw[6]) & layoutStoredV8,
		indexWidth: int(raw[7]),
		contentFmt: ContentFormat(raw[8]),
		groupShift: raw[9],
		keyCount:   binary.LittleEndian.Uint32(raw[10:]),
		poolCount:  binary.LittleEndian.Uint32(raw[14:]),
		docCount:   binary.LittleEndian.Uint32(raw[18:]),
	}
	switch {
	case !h.structCodec.valid():
		return header{}, 0, errUnsupportedf("structure codec %d", uint8(h.structCodec))
	case !validIndexWidth(h.indexWidth):
		return header{}, 0, errUnsupportedf("index width %d (want 1, 2, or 4)", h.indexWidth)
	case h.contentFmt > FormatNDJSON:
		return header{}, 0, errUnsupportedf("content format %d", uint8(h.contentFmt))
	case h.groupShift > MaxPoolGroupShift:
		return header{}, 0, errUnsupportedf("pool group shift %d (max %d)", h.groupShift, MaxPoolGroupShift)
	case h.docCount == 0:
		return header{}, 0, errCorruptf("doc_count is zero")
	}
	return h, headerSizeV8, nil
}
