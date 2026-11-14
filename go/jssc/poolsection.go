package jssc

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

// The pool section is a sequence of compressed *groups*. A group holds
// `1 << groupShift` consecutive pool entries and is the unit a reader must
// decompress to reach any entry inside it.
//
// With a group size of 1 — every version through JSS4, and the JSS5 default for
// codecs that carry their own dictionary — a group is exactly one entry, and
// the format keeps the original guarantee that decoding entry i touches nothing
// else. Larger groups let entries share compression context, which is where the
// size comes from: a general compressor over 64 neighbouring strings finds the
// cross-entry repetition that a per-entry codec structurally cannot.
//
// Measured on a GeoJSON pool, going from groups of 1 to groups of 64 cut the
// pool section by 63%. The cost is that a cold read of one string inflates its
// whole group, which is what a cold query pays.
//
// Group framing:
//
//	JSS1–JSS4   uint32 compressed_len  ‖  bytes        (one entry per group)
//	JSS5–JSS7   uvarint compressed_len ‖  bytes
//	JSS8        ceil(groups/8) bytes of stored-raw flags, then
//	            max(0, ceil(groups/64)-1) uint32 group directory entries, then
//	            uvarint stored_len ‖ bytes, per group
//
// The JSS8 group directory (5.6.0-draft) gives where every 64th group's length
// prefix begins, counted from group 0's. Without it the only way to find group
// g is to read every length ahead of it, so the first string read after an
// O(1) open walked the whole section; with it a reader walks one segment of at
// most 64 lengths whatever the pool size, and checks that segment against the
// boundaries on either side (specification §7.4).
//
// The JSS8 flags are the pool's version of the rule the structure codec has
// always had: the header describes the bytes that follow, not the bytes the
// encoder intended. A group whose compressed form is no smaller than its
// plaintext is stored plain and marked, which on short strings saves both the
// bytes deflate added and the decompression needed to take them off again. A raw
// group is also free to read — its entries point straight into the archive.
//
// The flags are a bitmap in front of the groups rather than a bit inside each
// length, which was the first design and was measurably worse: shifting a length
// left by one pushes every group whose length sits just under a uvarint boundary
// into an extra byte, and on the corpora measured that cost more than the rule
// saved. A bit per group costs a bit per group. It is also in front, where a
// reader that fetches the archive in pieces can have it early.
//
// Group plaintext:
//
//	group size 1   the entry bytes, nothing else
//	group size > 1 repeated: uvarint entry_len ‖ entry bytes

// groupPlaintext assembles the uncompressed content of one group.
func groupPlaintext(entries []string) []byte {
	if len(entries) == 1 {
		return []byte(entries[0])
	}
	var buf bytes.Buffer
	for _, s := range entries {
		writeUvarint(&buf, uint64(len(s)))
		buf.WriteString(s)
	}
	return buf.Bytes()
}

// groupCount is how many groups hold poolCount entries at the given size.
func groupCount(poolCount, size int) int {
	if poolCount == 0 {
		return 0
	}
	return (poolCount + size - 1) / size
}

// groupRange returns the half-open entry range group g covers.
func groupRange(g, size, poolCount int) (lo, hi int) {
	lo = g * size
	hi = min(lo+size, poolCount)
	return lo, hi
}

// segmentGroups is S of the group directory (specification §7.4): the
// directory records where every segmentGroups-th group begins, and a reader
// walks at most one segment's lengths to reach any group.
const segmentGroups = 64

// dirEntries is how many uint32 entries the group directory holds for a group
// count: one per segment after the first, since the first segment starts at 0
// and the last one ends where pool_len says.
func dirEntries(groups int) int {
	if groups <= segmentGroups {
		return 0
	}
	return (groups+segmentGroups-1)/segmentGroups - 1
}

// segmentCount is how many segments of the group directory a group count spans.
func segmentCount(groups int) int {
	return (groups + segmentGroups - 1) / segmentGroups
}

// uvarintLen is the encoded width of v as a uvarint.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// encodePool compresses every group and returns the framed pool section.
//
// Groups are independent by construction — that is the format's own guarantee
// — so they compress in parallel when concurrency allows. The compressed
// blocks are collected into a slice indexed by group and only then concatenated,
// so the output is byte-identical to a sequential encode. Determinism is a
// normative requirement, not a nicety.
//
// directory selects the JSS8 group directory, written between the stored-raw
// flags and the groups. Every entry is known before a group is written, because
// each group's framed size is its length's uvarint width plus its length.
func encodePool(c codec.Codec, entries []string, groupShift uint8, varintFraming, opportunistic, directory bool, concurrency int) (section []byte, payload, rawGroups int, err error) {
	size := 1 << groupShift
	groups := groupCount(len(entries), size)
	if groups == 0 {
		return nil, 0, 0, nil
	}

	blocks := make([][]byte, groups)
	raw := make([]bool, groups)
	if concurrency > 1 && worthParallel(entries, groups) {
		err = encodeGroupsParallel(c, entries, size, blocks, raw, opportunistic, concurrency)
	} else {
		err = encodeGroupsSequential(c, entries, size, blocks, raw, opportunistic)
	}
	if err != nil {
		return nil, 0, 0, err
	}

	framedSize := func(enc []byte) int {
		if varintFraming {
			return uvarintLen(uint64(len(enc))) + len(enc)
		}
		return 4 + len(enc)
	}
	flagBytes, dirBytes, framed := 0, 0, 0
	if opportunistic {
		flagBytes = rawFlagBytes(groups)
	}
	if directory {
		dirBytes = 4 * dirEntries(groups)
	}
	for _, enc := range blocks {
		framed += framedSize(enc)
	}

	section = make([]byte, flagBytes, flagBytes+dirBytes+framed)
	if opportunistic {
		for g := range blocks {
			if raw[g] {
				section[g/8] |= 1 << (g % 8)
				rawGroups++
			}
		}
	}
	if directory {
		off := 0
		for g, enc := range blocks {
			if g > 0 && g%segmentGroups == 0 {
				// An offset past uint32 means a section past pool_len's range,
				// which framePoolSection refuses; refusing here says why.
				if off > math.MaxUint32 {
					return nil, 0, 0, errLimitf("pool group %d starts %d bytes in (group directory max %d)",
						g, off, uint64(math.MaxUint32))
				}
				section = binary.LittleEndian.AppendUint32(section, uint32(off))
			}
			off += framedSize(enc)
		}
	}
	for _, enc := range blocks {
		if varintFraming {
			section = binary.AppendUvarint(section, uint64(len(enc)))
		} else {
			section = binary.LittleEndian.AppendUint32(section, uint32(len(enc)))
		}
		section = append(section, enc...)
		payload += len(enc)
	}
	return section, payload, rawGroups, nil
}

// rawFlagBytes is the size of the JSS8 stored-raw bitmap for a group count.
func rawFlagBytes(groups int) int { return (groups + 7) / 8 }

// encodeGroup compresses one group, and under the opportunistic rule keeps the
// plaintext instead when compression did not strictly shrink it.
//
// Strictly: a tie is stored raw, because equal bytes that need no decompression
// are the better of the two.
func encodeGroup(c codec.Codec, plain []byte, opportunistic bool) (out []byte, storedRaw bool, err error) {
	enc, err := c.Encode(plain)
	if err != nil {
		return nil, false, err
	}
	if opportunistic && len(enc) >= len(plain) {
		return plain, true, nil
	}
	return enc, false, nil
}

// minParallelGroups and minParallelBytes are the point below which spawning
// workers costs more than it saves. Measured: a pool of a few hundred short
// strings spread over eleven groups came out marginally slower in parallel,
// because the goroutines cost more than the compression they were sharing.
const (
	minParallelGroups = 4
	minParallelBytes  = 32 << 10
)

// worthParallel reports whether a pool holds enough work to spread.
func worthParallel(entries []string, groups int) bool {
	if groups < minParallelGroups {
		return false
	}
	total := 0
	for _, s := range entries {
		total += len(s)
		if total >= minParallelBytes {
			return true
		}
	}
	return false
}

func encodeGroupsSequential(c codec.Codec, entries []string, size int, blocks [][]byte, raw []bool, opportunistic bool) error {
	for g := range blocks {
		lo, hi := groupRange(g, size, len(entries))
		enc, storedRaw, err := encodeGroup(c, groupPlaintext(entries[lo:hi]), opportunistic)
		if err != nil {
			return errInvalidf("compress pool group %d: %v", g, err)
		}
		blocks[g], raw[g] = enc, storedRaw
	}
	return nil
}

// encodeGroupsParallel compresses groups across workers. Codec.Encode must be
// safe for concurrent use; every registered codec is.
func encodeGroupsParallel(c codec.Codec, entries []string, size int, blocks [][]byte, raw []bool, opportunistic bool, workers int) error {
	if workers > len(blocks) {
		workers = len(blocks)
	}

	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				g := int(next.Add(1) - 1)
				if g >= len(blocks) {
					return
				}
				lo, hi := groupRange(g, size, len(entries))
				enc, storedRaw, err := encodeGroup(c, groupPlaintext(entries[lo:hi]), opportunistic)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = errInvalidf("compress pool group %d: %v", g, err)
					}
					mu.Unlock()
					return
				}
				// Each worker writes only its own index, so no lock is needed
				// for the results themselves. The raw flag is decided from the
				// group's own bytes, so it is as deterministic as the bytes.
				blocks[g], raw[g] = enc, storedRaw
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// walkPoolSection walks the group framing, reading only lengths, and reports
// where each group sits. It allocates nothing of its own; fn may be nil, which
// is how a caller that only needs the section's extent asks for it.
//
// Opening a JSS1–JSS7 archive uses the nil form: those versions store no
// section length, so open has to walk the framing to find what follows it.
// JSS8 opens without walking anything (specification §7.4).
func walkPoolSection(raw []byte, pos, poolCount, groupSize int, varintFraming, opportunistic bool,
	fn func(g, off, n int, storedRaw bool)) (end, payload int, err error) {

	groups := groupCount(poolCount, groupSize)

	// A group costs at least one framing byte, so a count that cannot fit in
	// the remaining buffer is corrupt however the rest is framed.
	if int64(groups) > int64(len(raw)-pos) {
		return 0, 0, errCorruptf("archive declares %d pool groups but has %d bytes left",
			groups, len(raw)-pos)
	}

	start := pos
	flags := 0
	if opportunistic {
		flags = rawFlagBytes(groups)
		if flags > len(raw)-pos {
			return 0, 0, errTruncatedf("pool stored-raw flags")
		}
		pos += flags
	}
	for g := range groups {
		var n int
		if varintFraming {
			v, size, err := readUvarintBytes(raw, pos)
			if err != nil {
				return 0, 0, errTruncatedf("pool group %d length", g)
			}
			pos += size
			n = int(v)
		} else {
			if pos+4 > len(raw) {
				return 0, 0, errTruncatedf("pool group %d length", g)
			}
			n = int(binary.LittleEndian.Uint32(raw[pos:]))
			pos += 4
		}
		// pos+n must not be evaluated before it is known to fit: a group length
		// near MaxInt overflows the addition to a negative, which then passes a
		// naive bounds check and slices with a huge negative index. Compare
		// against the room remaining instead, which cannot overflow.
		if n < 0 || pos < 0 || pos > len(raw) || n > len(raw)-pos {
			return 0, 0, errTruncatedf("pool group %d payload", g)
		}
		if fn != nil {
			storedRaw := opportunistic && raw[start+g/8]&(1<<(g%8)) != 0
			fn(g, pos-start, n, storedRaw)
		}
		payload += n
		pos += n
	}
	return pos, payload, nil
}

// scanPoolSection validates the framing and returns the section's end and its
// stored payload, without building an index or allocating.
func scanPoolSection(raw []byte, pos, poolCount, groupSize int, varintFraming, opportunistic bool) (end, payload int, err error) {
	return walkPoolSection(raw, pos, poolCount, groupSize, varintFraming, opportunistic, nil)
}

// poolReader reads one pool section lazily, a segment of groups at a time.
//
// Opening records where the section's parts are and reads none of them. The
// first string read allocates one pointer per segment. A segment's lengths are
// walked the first time a group inside it is wanted, checked against the
// boundaries on either side, and its entries are cached as they are decoded.
//
// A JSS8 archive stores those boundaries in its group directory, so the first
// read costs one segment whatever the size of the pool. Earlier versions store
// none: there the first read walks every length once to find them — the cost
// the directory exists to remove — and reads the same way afterwards.
type poolReader struct {
	name          string // "pool" or "key pool", for errors
	raw           []byte // the whole archive
	count         int    // entries
	groupSize     int
	groups        int
	varint        bool
	opportunistic bool
	flags         int // absolute offset of the stored-raw bitmap
	dir           int // absolute offset of the group directory; -1 when the version has none
	framed        int // absolute offset of group 0's length prefix
	end           int // absolute end of the framed groups

	ready  bool
	bounds []int          // segment starts found by one walk, when there is no directory
	segs   []*poolSegment // segs[k] stays nil until segment k has been walked
}

// poolSegment is one walked segment: where its groups are, and the entries
// decoded from them so far. It is one allocation — a read touches a new segment
// for most strings it returns from a large pool, so its size is the price of a
// warm read — which is why it keeps only where each group's length prefix sits
// and rereads that prefix, already validated, to find the payload.
type poolSegment struct {
	base   int                   // absolute offset of the segment's first length prefix
	at     [segmentGroups]uint32 // each group's length prefix, relative to base
	done   uint64                // bit j set once the segment's group j has been decoded
	strs   []string              // the segment's entries, valid where done says
	inline [segmentGroups]string // strs when a group is one entry
}

// newPoolReader lays out a pool section that occupies raw[start:end] after any
// length prefix, without reading a single group.
//
// The bitmap and the directory are sized by the group count alone, and every
// group costs at least one framing byte, so a count the section cannot hold is
// rejected here, before it sizes anything (specification §7.4).
func newPoolReader(name string, raw []byte, start, end, count, groupSize int,
	varint, opportunistic, directory bool) (*poolReader, error) {

	groups := groupCount(count, groupSize)
	p := &poolReader{
		name: name, raw: raw, count: count, groupSize: groupSize, groups: groups,
		varint: varint, opportunistic: opportunistic,
		flags: start, dir: -1, end: end,
	}
	need := int64(groups)
	pos := start
	if opportunistic {
		need += int64(rawFlagBytes(groups))
		pos += rawFlagBytes(groups)
	}
	if directory {
		need += 4 * int64(dirEntries(groups))
		p.dir = pos
		pos += 4 * dirEntries(groups)
	}
	if need > int64(end-start) {
		return nil, errCorruptf("%s: %d groups need at least %d bytes, the section has %d",
			name, groups, need, end-start)
	}
	p.framed = pos
	return p, nil
}

// prepare allocates the segment table, once, on the first string read. With no
// directory it also walks every length to find where the segments start, and
// checks the walk ends where the section does.
func (p *poolReader) prepare() error {
	if p.ready {
		return nil
	}
	nseg := segmentCount(p.groups)
	if p.dir < 0 && nseg > 1 {
		p.bounds = make([]int, nseg)
		pos := p.framed
		for g := range p.groups {
			if g%segmentGroups == 0 {
				p.bounds[g/segmentGroups] = pos
			}
			start, n, err := p.frameAt(pos, p.end, g)
			if err != nil {
				return err
			}
			pos = start + n
		}
		if pos != p.end {
			return errCorruptf("%s groups end at %d, the section ends at %d", p.name, pos, p.end)
		}
	}
	p.segs = make([]*poolSegment, nseg)
	p.ready = true
	return nil
}

// boundary is where segment k begins, as an absolute offset; segment count k
// is the end of the framed groups. int64, because a directory entry is a
// uint32 that an int on a 32-bit platform cannot hold.
func (p *poolReader) boundary(k int) int64 {
	switch {
	case k == 0:
		return int64(p.framed)
	case k == len(p.segs):
		return int64(p.end)
	case p.dir >= 0:
		return int64(p.framed) + int64(binary.LittleEndian.Uint32(p.raw[p.dir+4*(k-1):]))
	default:
		return int64(p.bounds[k])
	}
}

// frameAt reads group g's stored length at pos and returns where its payload
// starts and how long it is, refusing a length or payload that runs past limit.
// pos+n is never formed before n is known to fit: a length near MaxInt would
// overflow it to a negative that passes a naive bounds check.
func (p *poolReader) frameAt(pos, limit, g int) (start, n int, err error) {
	if p.varint {
		v, size, err := readUvarintBytes(p.raw[:limit], pos)
		if err != nil {
			return 0, 0, errTruncatedf("%s group %d length", p.name, g)
		}
		pos += size
		if v > uint64(limit-pos) {
			return 0, 0, errTruncatedf("%s group %d payload", p.name, g)
		}
		return pos, int(v), nil
	}
	if pos < 0 || limit-pos < 4 {
		return 0, 0, errTruncatedf("%s group %d length", p.name, g)
	}
	n = int(binary.LittleEndian.Uint32(p.raw[pos:]))
	pos += 4
	if n > limit-pos {
		return 0, 0, errTruncatedf("%s group %d payload", p.name, g)
	}
	return pos, n, nil
}

// segment returns segment k, walking its lengths on first use. The walk is
// held to the segment's own boundaries and must end exactly at the next one,
// which is the check that keeps a corrupt length or directory entry from
// mislocating a group (specification §7.4).
func (p *poolReader) segment(k int) (*poolSegment, error) {
	if s := p.segs[k]; s != nil {
		return s, nil
	}
	lo, hi := p.boundary(k), p.boundary(k+1)
	if lo >= hi || hi > int64(p.end) {
		return nil, errCorruptf("%s segment %d spans [%d, %d), outside [%d, %d)",
			p.name, k, lo, hi, p.framed, p.end)
	}
	// A JSS8 segment lies inside a pool_len-bounded section and always fits;
	// only an earlier version's sixty-four uint32-framed groups could not.
	if hi-lo > math.MaxUint32 {
		return nil, errLimitf("%s segment %d spans %d bytes, past what this reader indexes",
			p.name, k, hi-lo)
	}
	s := &poolSegment{base: int(lo)}
	first := k * segmentGroups
	last := min(first+segmentGroups, p.groups)
	pos, limit := int(lo), int(hi)
	for g := first; g < last; g++ {
		s.at[g-first] = uint32(pos - s.base)
		start, n, err := p.frameAt(pos, limit, g)
		if err != nil {
			return nil, err
		}
		pos = start + n
	}
	if pos != limit {
		return nil, errCorruptf("%s segment %d ends at %d, the next begins at %d", p.name, k, pos, limit)
	}
	eLo, _ := groupRange(first, p.groupSize, p.count)
	_, eHi := groupRange(last-1, p.groupSize, p.count)
	if n := eHi - eLo; n <= segmentGroups {
		s.strs = s.inline[:n]
	} else {
		s.strs = make([]string, n)
	}
	p.segs[k] = s
	return s, nil
}

// storedRaw reports whether group g holds its plaintext rather than a
// compressed form, in which case the pool codec must not be applied to it.
func (p *poolReader) storedRaw(g int) bool {
	return p.opportunistic && p.raw[p.flags+g/8]&(1<<(g%8)) != 0
}

// stored returns the stored bytes of group g, which lives in segment s. The
// length prefix it rereads was validated against the segment when the segment
// was walked, so it cannot run past it now.
func (p *poolReader) stored(s *poolSegment, g int) []byte {
	pos := s.base + int(s.at[g%segmentGroups])
	if p.varint {
		n, w := binary.Uvarint(p.raw[pos:])
		return p.raw[pos+w : pos+w+int(n)]
	}
	n := int(binary.LittleEndian.Uint32(p.raw[pos:]))
	return p.raw[pos+4 : pos+4+n]
}

// cached reports whether group g of segment s has been decoded.
func (s *poolSegment) cached(g int) bool {
	return s.done&(1<<(uint(g)%segmentGroups)) != 0
}

// inflateGroup returns group g's plaintext: its stored bytes when the
// stored-raw flag is set, the codec's output over them otherwise. app is c as
// an Appender, or nil.
func inflateGroup(c codec.Codec, app codec.Appender, p *poolReader, s *poolSegment, g int) ([]byte, error) {
	b := p.stored(s, g)
	switch {
	case p.storedRaw(g):
		// A group the encoder declined to compress is already its own
		// plaintext. Nothing is decompressed and nothing is allocated: the
		// entries end up pointing into the archive itself, which is the same
		// contract Open already makes about the bytes it was given.
		return b, nil
	case app != nil:
		return app.DecodeAppend(nil, b)
	default:
		return c.Decode(b)
	}
}

// fill points the cache entries of group g at its plaintext and marks it done.
func (p *poolReader) fill(s *poolSegment, g int, plain []byte) (lo, hi int, err error) {
	lo, hi = groupRange(g, p.groupSize, p.count)
	base := (g - g%segmentGroups) * p.groupSize
	if err := sliceGroupPlaintext(plain, s.strs[lo-base:hi-base]); err != nil {
		return 0, 0, errCorruptf("%s group %d: %v", p.name, g, err)
	}
	s.done |= 1 << (uint(g) % segmentGroups)
	return lo, hi, nil
}

// entry returns entry i from a segment whose group holding it is cached.
func (p *poolReader) entry(s *poolSegment, i int) string {
	g := i / p.groupSize
	return s.strs[i-(g-g%segmentGroups)*p.groupSize]
}

// sliceGroupPlaintext points each entry of out at its bytes inside plain. len(out)
// is how many entries the group is expected to contain, which the reader derives
// from the pool count and the group size rather than trusting the payload.
//
// The plaintext is retained for as long as the entries are, and nothing ever
// mutates it, so the entries share it instead of each owning a copy. That is one
// allocation per group rather than one per entry, and it is most of why a walk
// over a string-heavy pool costs what it does.
func sliceGroupPlaintext(plain []byte, out []string) error {
	if len(out) == 1 {
		out[0] = unsafe.String(unsafe.SliceData(plain), len(plain))
		return nil
	}
	pos := 0
	for k := range out {
		n, size, err := readUvarintBytes(plain, pos)
		if err != nil {
			return errCorruptf("pool group entry length: %v", err)
		}
		pos += size
		end := pos + int(n)
		if end < pos || end > len(plain) {
			return errCorruptf("pool group entry claims %d bytes, %d remain", n, len(plain)-pos)
		}
		if n == 0 {
			out[k] = ""
		} else {
			out[k] = unsafe.String(&plain[pos], int(n))
		}
		pos = end
	}
	return nil
}
