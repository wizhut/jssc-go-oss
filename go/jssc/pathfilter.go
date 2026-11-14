package jssc

import (
	"encoding/binary"
	"hash/crc32"
)

// The key-path filter (JSS8, specification §7B).
//
// The key directory (§7A) answers "is this string used as a key anywhere in
// this document". That makes a negative field lookup free, but a *hit* only
// says somewhere: reaching $.a.b.c still means walking a, then b, then c, one
// container at a time, and a document that holds all three keys in unrelated
// places still costs the full descent to discover that the path is not there.
//
// The filter answers the question the query actually asked. It holds the
// root-to-leaf key paths the document contains, so a concrete path can be
// rejected before the walk starts — no structure read, no pool decode.
//
// Two decisions shape it:
//
//   - **Array subscripts are elided.** $.name[0].the_name and $.name[7].the_name
//     are the same shape, and a filter that distinguished them would hold one
//     entry per element and answer a question nobody asked. A path is the
//     sequence of *keys* on the way down; an array on the route contributes
//     nothing to it.
//   - **It is a Bloom filter, so it may say yes when the answer is no.** That is
//     the safe direction and it is what makes the section cheap: a false
//     positive costs the walk that would have happened anyway, while a negative
//     is proof. There is nothing to verify and no fallback path to lose, which
//     is the difference between this section and the key directory, where a
//     collision had to be settled against the pool.

// PathHashCRC32 names the filter's hash function in the specification.
const PathHashCRC32 = "crc32-ieee"

// pathFilterBitsPerPath and pathFilterHashes size the filter. Ten bits and seven
// probes put the false-positive rate near 0.8%, and k = round((m/n) * ln 2) is
// the optimum for that ratio.
const (
	pathFilterBitsPerPath = 10
	pathFilterHashes      = 7
	// minPathFilterBits floors the bitmap so a document with one or two paths
	// does not get a filter whose every probe collides.
	minPathFilterBits = 64
	// maxPathFilterBits bounds what a decoder will allocate from a declared
	// size before it has seen the bytes.
	maxPathFilterBits = 1 << 27 // 16 MiB of bitmap
)

// pathFilterCostRatio is how much bigger the bytes a filter indexes must be
// than the filter itself for an encoder to emit it. The section only pays for
// itself on documents where a walk is worth skipping, and on a 200-byte row it
// would be a quarter of the archive.
//
// The rule is the one §8.4 applies to offset tables: an index is worth emitting
// when it is small relative to the payload it indexes.
const pathFilterCostRatio = 32

// pathHash extends a parent path hash by one key. The root has parent zero.
//
// Chaining rather than hashing the joined path means an encoder computes each
// path once as it descends, and a query computes the chain for its segments
// without building any strings.
func pathHash(parent uint32, key string) uint32 {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], parent)
	h := crc32.NewIEEE()
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(key))
	return h.Sum32()
}

// pathFilter is the parsed section: a bitmap and the probe count that filled it.
type pathFilter struct {
	bits []byte
	m    uint64 // bitmap size in bits
	k    uint8  // probes per path
}

func (f pathFilter) present() bool { return f.m > 0 }

// probes yields the bit positions a path hash occupies, by the standard
// double-hashing construction: two independent-enough values derived from one
// hash, then k terms of an arithmetic progression modulo m.
func (f pathFilter) probes(h uint32, fn func(bit uint64) bool) bool {
	h1 := uint64(h)
	// The second value must be non-zero, or every probe lands on the same bit.
	h2 := uint64(h)*0x9E3779B1 | 1
	for i := range uint64(f.k) {
		if !fn((h1 + i*h2) % f.m) {
			return false
		}
	}
	return true
}

// mayContain reports whether the document might hold the path. False is proof
// that it does not.
func (f pathFilter) mayContain(h uint32) bool {
	if !f.present() {
		return true // no filter is not the same as an empty one
	}
	return f.probes(h, func(bit uint64) bool {
		return f.bits[bit/8]&(1<<(bit%8)) != 0
	})
}

// buildPathFilter encodes the section for a set of path hashes, or nil when
// there are none to index.
//
// paths must be sorted; the caller keeps them that way so the archive does not
// depend on map iteration order. Nothing here reads the order — the bitmap is a
// set — but a sorted input is what makes the *decision* to emit deterministic
// when a future rule reads the paths themselves.
func buildPathFilter(paths []uint32) []byte {
	if len(paths) == 0 {
		return nil
	}
	m := uint64(len(paths)) * pathFilterBitsPerPath
	if m < minPathFilterBits {
		m = minPathFilterBits
	}
	m = (m + 7) &^ 7 // whole bytes, so the section has no partial trailing byte
	f := pathFilter{bits: make([]byte, m/8), m: m, k: pathFilterHashes}
	for _, h := range paths {
		f.probes(h, func(bit uint64) bool {
			f.bits[bit/8] |= 1 << (bit % 8)
			return true
		})
	}

	out := make([]byte, 0, 4+len(f.bits))
	out = appendUvarint(out, m)
	out = append(out, f.k)
	return append(out, f.bits...)
}

// readPathFilter parses the section at pos and returns the offset just past it.
func readPathFilter(raw []byte, pos int) (pathFilter, int, error) {
	m, n, err := readUvarintBytes(raw, pos)
	if err != nil {
		return pathFilter{}, 0, errTruncatedf("path filter size")
	}
	pos += n
	if m == 0 || m%8 != 0 {
		return pathFilter{}, 0, errCorruptf("path filter declares %d bits, want a positive multiple of 8", m)
	}
	if m > maxPathFilterBits {
		return pathFilter{}, 0, errLimitf("path filter declares %d bits (max %d)", m, maxPathFilterBits)
	}
	if pos >= len(raw) {
		return pathFilter{}, 0, errTruncatedf("path filter probe count")
	}
	k := raw[pos]
	pos++
	if k == 0 {
		return pathFilter{}, 0, errCorruptf("path filter declares zero probes")
	}
	// Validate the extent against the room remaining rather than by addition,
	// which can overflow (§15.3).
	size := int(m / 8)
	if size > len(raw)-pos {
		return pathFilter{}, 0, errTruncatedf("path filter of %d bits", m)
	}
	return pathFilter{bits: raw[pos : pos+size], m: m, k: k}, pos + size, nil
}

// worthPathFilter reports whether a filter of this size earns its place beside
// the bytes it indexes.
func worthPathFilter(filterBytes, indexedBytes int) bool {
	if filterBytes == 0 {
		return false
	}
	return filterBytes*pathFilterCostRatio <= indexedBytes
}

// appendUvarint appends v in LEB128.
func appendUvarint(dst []byte, v uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	return append(dst, buf[:binary.PutUvarint(buf[:], v)]...)
}
