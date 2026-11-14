package fsst

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// Table is a trained FSST symbol table used for encode and decode.
// Construct with Train or TrainStrings, or with ReadFrom for a table read back
// from an archive; the zero value is not usable.
//
// A finalized Table is immutable and every method on it is safe for concurrent
// use, which is what lets an encoder compress pool groups in parallel against
// one table.
// encoderTables are the ~150 KB of lookup arrays only encoding needs. They
// hang off the Table behind a pointer so that opening an archive — which
// reads a table back purely to decode — allocates none of them: before this
// split every open paid the allocation and zeroing of the arrays even though
// decode-only ReadFrom never filled them in.
type encoderTables struct {
	shortCodes [65536]uint16
	byteCodes  [256]uint16
	hashTab    [hashTabSize]symbol
}

type Table struct {
	enc     *encoderTables
	symbols [codeMax]symbol

	nSymbols  uint16
	suffixLim uint16
	lenHisto  [8]uint16

	decLen    [255]byte
	decSymbol [255]uint64

	// A table read back from an archive is decode-only until something
	// actually encodes with it: the decoder needs 2.3 KB of arrays, while the
	// encoder-side indices above are ~350 KB of initialization that used to be
	// rebuilt on every archive open — a ~59µs floor under every cold read for
	// structures the read path never touches. encoderReady is set by finalize
	// (the training path) and cleared by ReadFrom; ensureEncoder builds the
	// indices on first use.
	encoderReady atomic.Bool
	encoderMu    sync.Mutex
}

const tableVersion uint64 = 20190218

var (
	// ErrBadVersion is returned when a serialized table has an unknown version.
	ErrBadVersion = errors.New("fsst: unsupported table version")
	// ErrCorrupted is returned when serialized table data is invalid.
	ErrCorrupted = errors.New("fsst: corrupted table data")
)

func newTable() *Table {
	t := &Table{enc: &encoderTables{}}
	for i := 0; i < 256; i++ {
		t.symbols[i] = newSymbolFromByte(byte(i), uint16(i))
	}
	empty := symbol{val: 0, icl: iclFree}
	for i := 256; i < codeMax; i++ {
		t.symbols[i] = empty
	}
	for i := range t.enc.hashTab {
		t.enc.hashTab[i] = empty
	}
	for i := 0; i < 256; i++ {
		t.enc.byteCodes[i] = packCodeLength(uint16(i), 1)
	}
	for i := 0; i < 65536; i++ {
		t.enc.shortCodes[i] = packCodeLength(uint16(i&mask8), 1)
	}
	return t
}

func (t *Table) clearSymbols() {
	for i := range t.lenHisto {
		t.lenHisto[i] = 0
	}
	for i := codeBase; i < int(codeBase)+int(t.nSymbols); i++ {
		sym := t.symbols[i]
		switch sym.length() {
		case 1:
			t.enc.byteCodes[sym.first()] = packCodeLength(uint16(sym.first()), 1)
		case 2:
			t.enc.shortCodes[sym.first2()] = packCodeLength(uint16(sym.first2()&mask8), 1)
		default:
			idx := sym.hash() & (hashTabSize - 1)
			t.enc.hashTab[idx] = symbol{val: 0, icl: iclFree}
		}
	}
	t.nSymbols = 0
}

func (t *Table) hashInsert(sym symbol) bool {
	idx := sym.hash() & (hashTabSize - 1)
	if t.enc.hashTab[idx].icl < iclFree {
		return false
	}
	mask := ^uint64(0) >> sym.ignoredBits()
	t.enc.hashTab[idx] = symbol{val: sym.val & mask, icl: sym.icl}
	return true
}

func (t *Table) addSymbol(sym symbol) bool {
	if int(codeBase)+int(t.nSymbols) >= codeMax {
		return false
	}
	length := sym.length()
	sym.setCodeLen(uint32(codeBase)+uint32(t.nSymbols), length)

	switch length {
	case 1:
		t.enc.byteCodes[sym.first()] = packCodeLength(uint16(codeBase+t.nSymbols), 1)
	case 2:
		t.enc.shortCodes[sym.first2()] = packCodeLength(uint16(codeBase+t.nSymbols), 2)
	default:
		if !t.hashInsert(sym) {
			return false
		}
	}
	t.symbols[int(codeBase)+int(t.nSymbols)] = sym
	t.nSymbols++
	t.lenHisto[length-1]++
	return true
}

func (t *Table) findLongestSymbol(sym symbol) uint16 {
	idx := sym.hash() & (hashTabSize - 1)
	entry := t.enc.hashTab[idx]
	if entry.icl <= sym.icl {
		mask := ^uint64(0) >> entry.ignoredBits()
		if entry.val == (sym.val & mask) {
			return entry.code() & codeMask
		}
	}
	if sym.length() >= 2 {
		code := t.enc.shortCodes[sym.first2()] & codeMask
		if code >= codeBase {
			return code
		}
	}
	return t.enc.byteCodes[sym.first()] & codeMask
}

// finalize renumbers symbols by length for fast encoding and builds decoder tables.
func (t *Table) finalize() {
	newCode := make([]uint8, 256)
	var codeStart [8]uint8
	byteLim := uint8(t.nSymbols) - uint8(t.lenHisto[0])

	codeStart[0] = byteLim
	codeStart[1] = 0
	for i := 1; i < 7; i++ {
		codeStart[i+1] = codeStart[i] + uint8(t.lenHisto[i])
	}

	t.suffixLim = uint16(codeStart[1])
	if t.nSymbols > 0 {
		t.symbols[0] = t.symbols[codeBase]
		newCode[0] = 0
	}

	conflictCode := int(codeStart[2])
	for i := 0; i < int(t.nSymbols); i++ {
		sym := t.symbols[int(codeBase)+i]
		length := sym.length()

		if length == 2 {
			hasConflict := false
			first2 := sym.first2()
			for k := 0; k < int(t.nSymbols); k++ {
				if k == i {
					continue
				}
				other := t.symbols[int(codeBase)+k]
				if other.length() > 1 && other.first2() == first2 {
					hasConflict = true
					break
				}
			}
			if hasConflict {
				conflictCode--
				newCode[i] = uint8(conflictCode)
			} else {
				newCode[i] = uint8(t.suffixLim)
				t.suffixLim++
			}
		} else {
			li := int(length - 1)
			newCode[i] = codeStart[li]
			codeStart[li]++
		}

		sym.setCodeLen(uint32(newCode[i]), length)
		t.symbols[int(newCode[i])] = sym
	}

	t.rebuildIndices()
	t.buildDecoderTables()
	t.encoderReady.Store(true)
}

// ensureEncoder builds the encoder-side indices for a table that was read
// back decode-only. Symbols read from an archive are already in finalized
// numbering, so rebuildIndices reproduces exactly the state finalize left.
func (t *Table) ensureEncoder() {
	if t.encoderReady.Load() {
		return
	}
	t.encoderMu.Lock()
	defer t.encoderMu.Unlock()
	if t.encoderReady.Load() {
		return
	}
	if t.enc == nil {
		t.enc = &encoderTables{}
	}
	t.rebuildIndices()
	t.encoderReady.Store(true)
}

func (t *Table) rebuildIndices() {
	for i := 0; i < 256; i++ {
		t.enc.byteCodes[i] = packCodeLength(codeMask, 1) // escape: uint8(511)=255
	}
	empty := symbol{val: 0, icl: iclFree}
	for i := range t.enc.hashTab {
		t.enc.hashTab[i] = empty
	}

	for i := 0; i < int(t.nSymbols); i++ {
		sym := t.symbols[i]
		if sym.length() == 1 {
			t.enc.byteCodes[sym.first()] = packCodeLength(uint16(i), 1)
		}
	}
	for i := 0; i < 65536; i++ {
		t.enc.shortCodes[i] = t.enc.byteCodes[i&mask8]
	}
	for i := 0; i < int(t.nSymbols); i++ {
		sym := t.symbols[i]
		if sym.length() == 2 {
			t.enc.shortCodes[sym.first2()] = packCodeLength(uint16(i), 2)
		}
	}
	for i := 0; i < int(t.nSymbols); i++ {
		sym := t.symbols[i]
		if sym.length() >= 3 {
			t.hashInsert(sym)
		}
	}
}

func (t *Table) buildDecoderTables() {
	clear(t.decLen[:])
	clear(t.decSymbol[:])
	for code := uint16(0); code < t.nSymbols; code++ {
		sym := t.symbols[code]
		t.decLen[code] = byte(sym.length())
		t.decSymbol[code] = sym.val
	}
}

// NumSymbols returns the number of learned symbols.
func (t *Table) NumSymbols() int { return int(t.nSymbols) }

// WriteTo serializes the table in the FSST on-disk format.
func (t *Table) WriteTo(w io.Writer) (int64, error) {
	ver := (tableVersion << 32) |
		(uint64(t.suffixLim) << 16) |
		(uint64(t.nSymbols) << 8) |
		1
	var n int64
	var buf8 [8]byte
	binary.LittleEndian.PutUint64(buf8[:], ver)
	nn, err := w.Write(buf8[:])
	n += int64(nn)
	if err != nil {
		return n, err
	}

	var lh [8]byte
	var hist [8]uint16
	for i := 0; i < int(t.nSymbols); i++ {
		length := t.symbols[i].length()
		if length >= 1 && length <= 8 {
			hist[length-1]++
		}
	}
	for i := 0; i < 8; i++ {
		lh[i] = byte(hist[i])
	}
	nn, err = w.Write(lh[:])
	n += int64(nn)
	if err != nil {
		return n, err
	}

	for i := 0; i < int(t.nSymbols); i++ {
		sym := t.symbols[i]
		sl := int(sym.length())
		for b := 0; b < sl; b++ {
			buf8[b] = byte(sym.val >> (8 * b))
		}
		nn, err = w.Write(buf8[:sl])
		n += int64(nn)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ReadFrom deserializes a table written by WriteTo.
// ReadFrom deserializes a table in the FSST on-disk format, decode-only: it
// fills the symbol list and the decoder arrays and leaves the ~350 KB of
// encoder indices unbuilt until ensureEncoder. This is the archive-open path,
// and it is why opening an archive no longer pays for a compressor it is not
// going to run.
func (t *Table) ReadFrom(r io.Reader) (int64, error) {
	t.nSymbols = 0
	t.suffixLim = 0
	clear(t.lenHisto[:])
	t.encoderReady.Store(false)
	var n int64
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return n, err
	}
	n += 8
	ver := binary.LittleEndian.Uint64(hdr[:])
	if ver>>32 != tableVersion {
		return n, ErrBadVersion
	}
	// suffixLim occupies bits 31..16 and nSymbols bits 15..8. Only the low byte
	// of suffixLim is ever set in practice, but the field is a uint16.
	t.suffixLim = uint16((ver >> 16) & mask16)
	t.nSymbols = uint16((ver >> 8) & mask8)
	if t.nSymbols > maxSymbols {
		return n, ErrCorrupted
	}

	var lh [8]byte
	if _, err := io.ReadFull(r, lh[:]); err != nil {
		return n, err
	}
	n += 8
	var histoSum uint16
	for i := 0; i < 8; i++ {
		t.lenHisto[i] = uint16(lh[i])
		histoSum += t.lenHisto[i]
	}
	if histoSum != t.nSymbols {
		return n, ErrCorrupted
	}

	lens := make([]uint8, t.nSymbols)
	pos := 0
	for l := 2; l <= 8; l++ {
		for range int(t.lenHisto[l-1]) {
			lens[pos] = uint8(l)
			pos++
		}
	}
	for range int(t.lenHisto[0]) {
		lens[pos] = 1
		pos++
	}

	for i := 0; i < int(t.nSymbols); i++ {
		sl := int(lens[i])
		if sl < 1 || sl > 8 {
			return n, ErrCorrupted
		}
		var b8 [8]byte
		if _, err := io.ReadFull(r, b8[:sl]); err != nil {
			return n, err
		}
		n += int64(sl)
		var v uint64
		for b := 0; b < sl; b++ {
			v |= uint64(b8[b]) << (8 * b)
		}
		sym := symbol{val: v}
		sym.setCodeLen(uint32(i), uint32(sl))
		t.symbols[i] = sym
	}

	t.buildDecoderTables()
	return n, nil
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (t *Table) MarshalBinary() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := t.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler.
func (t *Table) UnmarshalBinary(data []byte) error {
	_, err := t.ReadFrom(bytes.NewReader(data))
	return err
}

// Encode compresses input into a newly allocated buffer.
func (t *Table) Encode(input []byte) []byte {
	return t.EncodeInto(nil, input)
}

// EncodeInto compresses input, reusing buf when large enough.
func (t *Table) EncodeInto(buf, input []byte) []byte {
	t.ensureEncoder()
	need := 2*len(input) + outputPad
	if buf == nil || cap(buf) < need {
		buf = make([]byte, need)
	} else {
		buf = buf[:cap(buf)]
	}

	outPos := 0
	pos := 0
	n := len(input)

	for pos+8 <= n {
		chunkEnd := pos + chunkSize
		if chunkEnd > n-7 {
			chunkEnd = n - 7
		}
		outPos = t.encodeChunk(buf, outPos, input[pos:], chunkEnd-pos)
		pos = chunkEnd
	}
	if pos < n {
		// The tail is copied into a padded local buffer so encodeChunk's
		// unaligned eight-byte loads stay in bounds. It is local rather than
		// held on the Table so that Encode is reentrant: pool groups are
		// compressed in parallel against one shared table.
		var tailBuf [chunkSize + chunkPad]byte
		tail := n - pos
		copy(tailBuf[:tail], input[pos:])
		outPos = t.encodeChunk(buf, outPos, tailBuf[:], tail)
	}
	return buf[:outPos]
}

// encodeChunk encodes buf[0:end] with safe 8-byte loads past end (caller pads).
func (t *Table) encodeChunk(dst []byte, dstPos int, buf []byte, end int) int {
	suffixLim := uint8(t.suffixLim)
	pos := 0

	for pos < end {
		word := unalignedLoad(buf[pos:])

		code := t.enc.shortCodes[uint16(word&mask16)]
		if uint8(code) < suffixLim && pos+2 <= end {
			dst[dstPos] = uint8(code)
			dstPos++
			pos += 2
			continue
		}

		idx := hashWord(word&mask24) & (hashTabSize - 1)
		entry := t.enc.hashTab[idx]
		if entry.icl < iclFree {
			mask := ^uint64(0) >> uint32(entry.icl&mask16)
			symLen := int(entry.icl >> 28)
			if entry.val == (word&mask) && pos+symLen <= end {
				dst[dstPos] = uint8(entry.icl >> 16)
				dstPos++
				pos += symLen
				continue
			}
		}

		advance := int(code >> lenBits)
		if pos+advance > end {
			code = t.enc.byteCodes[uint8(word)]
			advance = 1
		}
		dst[dstPos] = uint8(code)
		dstPos++
		if code&codeBase != 0 {
			dst[dstPos] = uint8(word)
			dstPos++
		}
		pos += advance
	}
	return dstPos
}

// Decode decompresses src into a newly allocated buffer (or reuses buf).
func (t *Table) Decode(buf, src []byte) []byte {
	needed := len(src)*2 + 8
	if buf == nil || cap(buf) < needed {
		buf = make([]byte, needed)
	} else {
		buf = buf[:cap(buf)]
	}

	bufPos := 0
	srcPos := 0
	bufCap := len(buf)

	for srcPos < len(src) {
		for srcPos < len(src) && bufPos+8 <= bufCap {
			code := src[srcPos]
			srcPos++
			if code < escapeCode {
				binary.LittleEndian.PutUint64(buf[bufPos:bufPos+8], t.decSymbol[code])
				bufPos += int(t.decLen[code])
			} else {
				if srcPos >= len(src) {
					return buf[:bufPos]
				}
				buf[bufPos] = src[srcPos]
				bufPos++
				srcPos++
			}
		}
		if srcPos < len(src) && bufPos+8 > bufCap {
			newCap := bufCap * 2
			if newCap < bufPos+len(src)*8+8 {
				newCap = bufPos + len(src)*8 + 8
			}
			nb := make([]byte, newCap)
			copy(nb, buf[:bufPos])
			buf = nb
			bufCap = newCap
		}
	}
	return buf[:bufPos]
}

// DecodeAll decompresses src into a newly allocated slice.
func (t *Table) DecodeAll(src []byte) []byte {
	return t.Decode(nil, src)
}
