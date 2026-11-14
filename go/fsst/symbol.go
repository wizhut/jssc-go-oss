package fsst

import "encoding/binary"

const (
	lenBits  = 12
	codeBits = 9
	codeBase = 256           // first code for learned symbols during training
	codeMax  = 1 << codeBits // 512
	codeMask = codeMax - 1   // 0x1FF

	hashLog2Size = 10
	hashTabSize  = 1 << hashLog2Size // 1024
	hashPrime    = uint64(2971215073)
	hashShift    = 15

	// iclFree marks an unused hash-table slot (impossible length=15).
	iclFree = (uint64(15) << 28) | (uint64(codeMask) << 16)

	escapeCode = 255
	maxSymbols = 255
	chunkSize  = 511
	chunkPad   = 8
	outputPad  = 7

	mask8  = 0xFF
	mask16 = 0xFFFF
	mask24 = 0xFFFFFF
)

func unalignedLoad(b []byte) uint64 {
	return binary.LittleEndian.Uint64(b)
}

func hashWord(w uint64) uint64 {
	x := w * hashPrime
	return x ^ (x >> hashShift)
}

// packCodeLength packs (length<<12)|code for byteCodes/shortCodes entries.
func packCodeLength(code uint16, length int) uint16 {
	return uint16(code) | uint16(length<<lenBits)
}

// symbol is a 1–8 byte sequence with packed metadata in icl:
//
//	bits 28–31: length
//	bits 16–27: code
//	bits  0–15: ignoredBits = (8-length)*8  (mask helper for 8-byte compares)
type symbol struct {
	val uint64
	icl uint64
}

func newSymbolFromByte(b byte, code uint16) symbol {
	return symbol{
		val: uint64(b),
		icl: (uint64(1) << 28) | (uint64(code) << 16) | 56,
	}
}

func newSymbolFromBytes(in []byte) symbol {
	n := len(in)
	if n > 8 {
		n = 8
	}
	var v uint64
	for i := 0; i < n; i++ {
		v |= uint64(in[i]) << (8 * i)
	}
	s := symbol{val: v}
	s.setCodeLen(codeMax, uint32(n))
	return s
}

func (s *symbol) setCodeLen(code, length uint32) {
	s.icl = (uint64(length) << 28) | (uint64(code) << 16) | uint64((8-length)*8)
}

func (s symbol) length() uint32      { return uint32(s.icl >> 28) }
func (s symbol) code() uint16        { return uint16((s.icl >> 16) & codeMask) }
func (s symbol) ignoredBits() uint32 { return uint32(s.icl & mask16) }
func (s symbol) first() byte         { return byte(s.val & mask8) }
func (s symbol) first2() uint16      { return uint16(s.val & mask16) }
func (s symbol) hash() uint64        { return hashWord(s.val & mask24) }

func concatSymbols(a, b symbol) symbol {
	la, lb := a.length(), b.length()
	n := la + lb
	if n > 8 {
		n = 8
	}
	v := (b.val << (8 * la)) | a.val
	out := symbol{val: v}
	out.setCodeLen(codeMask, n)
	return out
}
