package jssc

import (
	"encoding/binary"
	"hash/crc32"
	"slices"
)

// The key hash directory (JSS6, specification §7A).
//
// The pool is sorted lexicographically, so finding a string's index means a
// binary search that decompresses about log2(groups)+1 groups. That is cheap,
// but a field lookup does not want a string at all — it wants the integer the
// structure stores, because object members are laid out in ascending key-index
// order and the lookup is then an integer binary search.
//
// The directory closes that last gap. It maps the hash of every string used as
// an object key to that string's pool index, sits uncompressed between the pool
// and the structure, and turns "$.field" into a lookup that decompresses
// nothing. A key that is absent from the directory is absent from the document,
// which makes a negative result free as well.
//
// Only keys are listed. Keys are a small, heavily repeated subset of the pool —
// 5 of 1,505 entries in one benchmark corpus, 321 of 577 in the worst — so the
// section costs a fraction of a percent on most documents.

// keyHash is the directory's hash function.
func keyHash(s string) uint32 { return crc32.ChecksumIEEE([]byte(s)) }

// keyDirectory maps key hashes to pool indices.
//
// It is a view over the archive's own bytes, not a copy of them. The entries are
// already what a binary search wants — fixed stride, sorted by hash — so
// materializing them into two slices at open would allocate for every datum a
// caller touches and buy nothing. Opening validates the section and remembers
// where it is; a lookup reads it in place.
type keyDirectory struct {
	raw   []byte // the entries, count excluded
	count int
	width int // pool index width, so the stride is 4+width
}

func (d keyDirectory) len() int { return d.count }

// stride is the size of one entry: a hash and a pool index.
func (d keyDirectory) stride() int { return 4 + d.width }

// hashAt is the hash of entry i.
func (d keyDirectory) hashAt(i int) uint32 {
	return binary.LittleEndian.Uint32(d.raw[i*d.stride():])
}

// indexAt is the pool index of entry i. The extent was validated at open, so
// the read needs no further bounds check.
func (d keyDirectory) indexAt(i int) int {
	off := i*d.stride() + 4
	switch d.width {
	case 1:
		return int(d.raw[off])
	case 2:
		return int(binary.LittleEndian.Uint16(d.raw[off:]))
	default:
		return int(binary.LittleEndian.Uint32(d.raw[off:]))
	}
}

// candidates returns the half-open range of directory entries whose hash is h.
//
// Through JSS7 the range is never longer than one: the encoder declined to write
// a directory at all rather than emit a colliding pair, so a hit was exact. JSS8
// keeps the directory and lets the run be longer, which is why callers ask for a
// range rather than an index — the common case is still exactly one entry, and
// costs the same binary search.
//
// An empty range is a definitive miss in every version: the directory lists
// every key in the document, so a hash that is not in it belongs to no key.
func (d keyDirectory) candidates(h uint32) (lo, hi int) {
	lo = 0
	hi = d.count
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if d.hashAt(mid) < h {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= d.count || d.hashAt(lo) != h {
		return 0, 0
	}
	end := lo + 1
	for end < d.count && d.hashAt(end) == h {
		end++
	}
	return lo, end
}

// lookup returns the pool index of the key whose hash is h, when exactly one
// key has that hash. A collision reports false and sends the caller to
// candidates, which can settle it against the pool.
func (d keyDirectory) lookup(h uint32) (int, bool) {
	lo, hi := d.candidates(h)
	if hi-lo != 1 {
		return 0, false
	}
	return d.indexAt(lo), true
}

// buildKeyDirectory encodes the directory for the given key indices.
//
// Entries are emitted in ascending (hash, pool index) order, which keeps the
// archive deterministic.
//
// Collisions are the difference between the two forms:
//
//   - JSS6 and JSS7 (tolerant=false): two keys sharing a hash makes the whole
//     directory unwritable, and the encoder emits an empty one. That trades a
//     rare event for a total loss — one unlucky pair of keys costs the document
//     every integer key lookup it would have had, silently.
//   - JSS8 (tolerant=true): the colliding entries are both listed. A reader
//     that lands on a run of more than one settles it by comparing the pool
//     strings, which costs a group decode for the run and nothing for every
//     other key in the document.
//
// buildSplitKeyDirectory is the JSS8 (5.3.0-draft) directory: it lists the
// whole key pool — the directory is complete, and the key pool is exactly the
// set it lists — with entries of the key width (specification §7A.2).
func buildSplitKeyDirectory(keys *stringPool, keyWidth int) (out []byte, ok bool) {
	idx := make([]int, keys.len())
	for i := range idx {
		idx[i] = i
	}
	return buildKeyDirectory(keys, idx, keyWidth, true)
}

func buildKeyDirectory(pool *stringPool, keyIdx []int, width int, tolerant bool) (out []byte, ok bool) {
	type entry struct {
		hash uint32
		idx  int
	}
	entries := make([]entry, 0, len(keyIdx))
	strs := pool.strings()
	for _, i := range keyIdx {
		if i < 0 || i >= len(strs) {
			return nil, false
		}
		entries = append(entries, entry{hash: keyHash(strs[i]), idx: i})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		switch {
		case a.hash < b.hash:
			return -1
		case a.hash > b.hash:
			return 1
		default:
			return a.idx - b.idx
		}
	})
	if !tolerant {
		for i := 1; i < len(entries); i++ {
			if entries[i].hash == entries[i-1].hash {
				return nil, false
			}
		}
	}

	out = make([]byte, 0, 4+len(entries)*(4+width))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(entries)))
	for _, e := range entries {
		out = binary.LittleEndian.AppendUint32(out, e.hash)
		out = appendIndex(out, e.idx, width)
	}
	return out, true
}

// emptyKeyDirectory is the section a JSS6 encoder writes when it has no
// directory to offer: a count of zero, and nothing else.
var emptyKeyDirectory = []byte{0, 0, 0, 0}

// appendIndex writes a pool index in the archive's index width.
func appendIndex(dst []byte, v, width int) []byte {
	switch width {
	case 1:
		return append(dst, byte(v))
	case 2:
		return binary.LittleEndian.AppendUint16(dst, uint16(v))
	default:
		return binary.LittleEndian.AppendUint32(dst, uint32(v))
	}
}

// readKeyDirectory parses the section at pos and returns the offset just past
// it. A count of zero is a valid, empty directory.
func readKeyDirectory(raw []byte, pos, width, poolCount int, tolerant bool) (keyDirectory, int, error) {
	var d keyDirectory
	if pos+4 > len(raw) {
		return d, 0, errTruncatedf("key directory count")
	}
	n := int64(binary.LittleEndian.Uint32(raw[pos:]))
	pos += 4
	if n == 0 {
		return d, pos, nil
	}

	// Validate the extent before it sizes an allocation: a directory cannot
	// hold more entries than the pool has strings, and it must fit in the
	// bytes that remain.
	if n > int64(poolCount) {
		return d, 0, errCorruptf("key directory declares %d entries, pool holds %d", n, poolCount)
	}
	stride := int64(4 + width)
	if n*stride > int64(len(raw)-pos) {
		return d, 0, errTruncatedf("key directory of %d entries", n)
	}

	// Validated once, here, so every later read can be a bare slice index. The
	// scan reads and allocates nothing, which is the point of not copying the
	// section out: what open pays for is a bounds check per entry, not a heap
	// object per entry.
	d = keyDirectory{raw: raw[pos : pos+int(n)*int(stride)], count: int(n), width: width}
	for i := range int(n) {
		if idx := d.indexAt(i); idx >= poolCount {
			return keyDirectory{}, 0, errCorruptf("key directory entry %d points at pool index %d of %d", i, idx, poolCount)
		}
		if i == 0 {
			continue
		}
		// Sorted, and through JSS7 strictly so: a repeated hash there means the
		// encoder broke its own collision-free promise. JSS8 permits the repeat
		// and only requires the order.
		prev, cur := d.hashAt(i-1), d.hashAt(i)
		switch {
		case cur < prev:
			return keyDirectory{}, 0, errCorruptf("key directory is not sorted at entry %d", i)
		case cur == prev && !tolerant:
			return keyDirectory{}, 0, errCorruptf("key directory repeats hash %#08x at entry %d", cur, i)
		case cur == prev && d.indexAt(i) <= d.indexAt(i-1):
			return keyDirectory{}, 0, errCorruptf("key directory collision run is not ordered at entry %d", i)
		}
	}
	return d, pos + int(n)*int(stride), nil
}
