package jssc

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"

	lz4 "github.com/pierrec/lz4/v4"
)

// The blocked structure (JSS7, specification §8A).
//
// Through JSS6 the structure was one DEFLATE stream. A stream cannot be entered
// in the middle, so reaching document i meant inflating every document before
// it: measured on a million-row archive, 186 ms and 270 MiB of resident memory
// to read one field, growing without bound. The pool had already been given
// this treatment in JSS5 and the structure had not.
//
// JSS7 compresses the structure in independently inflatable blocks with an
// index, so reaching a document inflates one block. Blocks are cut at document
// boundaries and never split a document — a document larger than the target
// becomes an oversized block of its own — which is what keeps every read inside
// a walk zero-copy, since a walk never leaves the document it entered.
//
// Measured on the same corpus: 64 KiB blocks cost 3.4% of the archive and turn
// that 186 ms into 63 µs against 64 KiB resident.

// structBlocks gives byte-addressed access to a structure that is stored as
// independently compressed blocks, inflating a block the first time something
// reads inside it.
//
// A version before JSS7 has exactly one block covering the whole structure, so
// every reader goes through the same path and the old behaviour is what a
// single block does.
type structBlocks struct {
	stored []byte      // the whole stored structure region, not copied
	codec  StructCodec //
	blocks []blockSpan
	total  int      // plain bytes across every block
	cache  [][]byte // inflated blocks, nil until touched

	// maxPlain bounds what one block may inflate to, mirroring the whole-block
	// limit of earlier versions (specification §14).
	maxPlain int64

	// InflatedBlocks counts how many blocks have been inflated. It is what makes
	// the difference between a seek and a scan visible.
	InflatedBlocks int

	// lastBlock is the block the previous read landed in. A walk reads the
	// structure in order, so nearly every lookup hits this block or the next
	// one; checking them first keeps a scan over a many-block structure from
	// paying a binary search per read. Like the cache, it assumes the single
	// reader the rest of this type already assumes.
	lastBlock int

	// inflater is reused across blocks. A fresh flate reader allocates tens of
	// kilobytes of window and tables, which on a 64 KiB block costs more than
	// the decompression it is there to do.
	inflater io.ReadCloser
}

// blockSpan locates one block in both coordinate systems.
type blockSpan struct {
	storedOff, storedLen int
	plainOff, plainLen   int
}

// newStructBlocks parses a JSS7 structure region: a block count, a table of
// per-block stored and plain lengths, then the blocks themselves.
func newStructBlocks(stored []byte, sc StructCodec, maxPlain int64) (*structBlocks, error) {
	if len(stored) < 4 {
		return nil, errTruncatedf("structure block count")
	}
	n := int64(binary.LittleEndian.Uint32(stored))
	pos := 4

	// Each block costs eight bytes of table and at least one stored byte, so a
	// count that cannot fit is corrupt before it sizes anything.
	if n > int64(len(stored)-pos)/9 {
		return nil, errCorruptf("structure declares %d blocks, %d bytes remain", n, len(stored)-pos)
	}
	sb := &structBlocks{stored: stored, codec: sc, maxPlain: maxPlain}
	sb.blocks = make([]blockSpan, n)
	sb.cache = make([][]byte, n)

	table := pos
	pos += 8 * int(n)
	for i := range int(n) {
		storedLen := int(binary.LittleEndian.Uint32(stored[table+8*i:]))
		plainLen := int(binary.LittleEndian.Uint32(stored[table+8*i+4:]))
		// Compare against the room remaining rather than summing, so a length
		// near MaxInt cannot overflow the check into passing.
		if storedLen < 0 || plainLen < 0 || pos > len(stored) || storedLen > len(stored)-pos {
			return nil, errTruncatedf("structure block %d", i)
		}
		sb.blocks[i] = blockSpan{
			storedOff: pos, storedLen: storedLen,
			plainOff: sb.total, plainLen: plainLen,
		}
		pos += storedLen
		sb.total += plainLen
		if maxPlain > 0 && int64(sb.total) > maxPlain {
			return nil, errLimitf("structure inflates past %d bytes", maxPlain)
		}
	}
	return sb, nil
}

// newSingleBlock wraps an already-plain structure as one block, which is what
// every version before JSS7 has.
func newSingleBlock(plain []byte) *structBlocks {
	return &structBlocks{
		blocks: []blockSpan{{plainLen: len(plain)}},
		cache:  [][]byte{plain},
		total:  len(plain),
	}
}

func (s *structBlocks) len() int { return s.total }

// blockOf finds the block holding a plain offset, checking the previous
// read's block and its successor before falling back to binary search.
func (s *structBlocks) blockOf(pos int) int {
	if i := s.lastBlock; i < len(s.blocks) {
		if sp := s.blocks[i]; sp.plainOff <= pos && pos-sp.plainOff < sp.plainLen {
			return i
		}
		if i+1 < len(s.blocks) {
			if sp := s.blocks[i+1]; sp.plainOff <= pos && pos-sp.plainOff < sp.plainLen {
				s.lastBlock = i + 1
				return i + 1
			}
		}
	}
	lo, hi := 0, len(s.blocks)-1
	for lo < hi {
		mid := int(uint(lo+hi+1) >> 1)
		if s.blocks[mid].plainOff <= pos {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	s.lastBlock = lo
	return lo
}

// block returns block i's plain bytes, inflating it the first time.
func (s *structBlocks) block(i int) ([]byte, error) {
	if b := s.cache[i]; b != nil {
		return b, nil
	}
	sp := s.blocks[i]
	raw := s.stored[sp.storedOff : sp.storedOff+sp.storedLen]

	var plain []byte
	var err error
	switch s.codec {
	case StructRaw:
		plain = raw
	case StructDeflate:
		// The table states the plain size, so the buffer is allocated once at
		// the right size instead of being grown, and the reader is reset rather
		// than rebuilt.
		plain, err = s.inflateBlock(raw, sp.plainLen)
	case StructLZ4:
		if sp.storedLen == sp.plainLen {
			// The verbatim block (specification §8A): an incompressible block
			// is stored plain under lz4, and the lengths are the whole signal.
			plain = raw
		} else {
			plain, err = lz4DecompressBlock(raw, sp.plainLen)
		}
	default:
		return nil, errUnsupportedf("structure codec %d", uint8(s.codec))
	}
	if err != nil {
		return nil, err
	}
	if len(plain) != sp.plainLen {
		return nil, errCorruptf("structure block %d inflates to %d bytes, table says %d",
			i, len(plain), sp.plainLen)
	}
	s.cache[i] = plain
	s.InflatedBlocks++
	return plain, nil
}

// inflateBlock decompresses one block into a buffer of exactly the declared
// size. A block that inflates to anything else is corrupt, which the caller
// checks; reading one byte past the buffer is how that is detected.
func (s *structBlocks) inflateBlock(raw []byte, plainLen int) ([]byte, error) {
	src := bytes.NewReader(raw)
	if r, ok := s.inflater.(flate.Resetter); ok {
		if err := r.Reset(src, nil); err != nil {
			return nil, errCorruptf("inflate structure block: %v", err)
		}
	} else {
		s.inflater = flate.NewReader(src)
	}

	out := make([]byte, plainLen)
	if _, err := io.ReadFull(s.inflater, out); err != nil {
		return nil, errCorruptf("inflate structure block: %v", err)
	}
	// Anything left means the block does not match its declared plain length.
	var tail [1]byte
	if n, _ := s.inflater.Read(tail[:]); n > 0 {
		return nil, errCorruptf("structure block inflates past its declared %d bytes", plainLen)
	}
	return out, nil
}

// lz4DecompressBlock decompresses one lz4 structure block into a buffer of
// exactly the declared size; the caller rejects any other outcome. The output
// bound is the table's plain length, so a hostile block cannot size the
// allocation (specification §15.1).
func lz4DecompressBlock(raw []byte, plainLen int) ([]byte, error) {
	out := make([]byte, plainLen)
	n, err := lz4.UncompressBlock(raw, out)
	if err != nil {
		return nil, errCorruptf("lz4 structure block: %v", err)
	}
	return out[:n], nil
}

// at returns n bytes at plain offset pos.
//
// The result aliases the inflated block whenever the range lies inside one,
// which is every read a walk makes: blocks are cut at document boundaries, and
// a walk never leaves the document it entered. Only the root offset table
// spans blocks, and only by a few bytes at a time.
func (s *structBlocks) at(pos, n int) ([]byte, error) {
	if pos < 0 || n < 0 || pos+n > s.total || pos+n < pos {
		return nil, errTruncatedf("%d bytes at %d, structure is %d", n, pos, s.total)
	}
	i := s.blockOf(pos)
	sp := s.blocks[i]
	b, err := s.block(i)
	if err != nil {
		return nil, err
	}
	off := pos - sp.plainOff
	if off+n <= sp.plainLen {
		return b[off : off+n], nil
	}

	out := make([]byte, 0, n)
	out = append(out, b[off:]...)
	for len(out) < n {
		i++
		if i >= len(s.blocks) {
			return nil, errTruncatedf("%d bytes at %d", n, pos)
		}
		if b, err = s.block(i); err != nil {
			return nil, err
		}
		take := min(n-len(out), len(b))
		out = append(out, b[:take]...)
	}
	return out, nil
}

// byteAt reads one byte.
func (s *structBlocks) byteAt(pos int) (byte, error) {
	b, err := s.at(pos, 1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// u32 reads a little-endian uint32.
func (s *structBlocks) u32(pos int) (uint32, error) {
	b, err := s.at(pos, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// upTo returns the bytes from pos to the end of pos's block, or to limit,
// whichever comes first. It is for readers that scan forward an unknown but
// bounded distance, such as a LEB128 value.
func (s *structBlocks) upTo(pos, limit int) ([]byte, error) {
	if pos < 0 || pos >= s.total {
		return nil, errTruncatedf("value at %d", pos)
	}
	limit = min(limit, s.total)
	i := s.blockOf(pos)
	sp := s.blocks[i]
	b, err := s.block(i)
	if err != nil {
		return nil, err
	}
	end := min(sp.plainOff+sp.plainLen, limit)
	if end <= pos {
		return nil, errTruncatedf("value at %d", pos)
	}
	if end == sp.plainOff+sp.plainLen && end < limit {
		// A LEB128 value could in principle straddle the boundary; take the
		// slow path so the reader still sees continuous bytes.
		return s.at(pos, min(binary.MaxVarintLen64, limit-pos))
	}
	return b[pos-sp.plainOff : end-sp.plainOff], nil
}

// ---------------------------------------------------------------- encoding --

// structBlockPlan is the encoder's side: where the blocks were cut.
type structBlockPlan struct {
	// cuts holds the plain offset each block starts at, with a final entry at
	// the end of the plain structure.
	cuts []int
}

// planStructBlocks cuts a plain structure into blocks of about target bytes.
//
// Two regions with two different rules. The root offset table, [0,tableEnd), is
// a uint32 array, so it may be cut anywhere on a four-byte boundary — and it
// must be, because it contains no documents and would otherwise become one
// block as large as the table. At a million documents that table is four
// megabytes, and leaving it whole put the entire scaling problem back: reaching
// any document meant inflating all four megabytes to read one offset.
//
// The document region prefers document starts, so that a walk's reads stay
// inside one block. With subdivide set — a compressing codec — a span larger
// than the target is then cut at target-sized intervals rather than left
// oversized (specification §8A); under raw there is nothing a cut bounds, and
// extra blocks only deepen every read's block lookup, so raw keeps oversized
// spans whole.
func planStructBlocks(plainLen, target, tableEnd int, docStarts []int, subdivide bool) structBlockPlan {
	if target <= 0 || plainLen <= target {
		return structBlockPlan{cuts: []int{0, plainLen}}
	}
	// A four-byte-aligned cut keeps any single table entry inside one block.
	aligned := max(target&^3, 4)

	cuts := []int{0}
	last := 0
	for at := aligned; at < min(tableEnd, plainLen); at += aligned {
		cuts = append(cuts, at)
		last = at
	}
	for _, b := range docStarts {
		if b <= last || b >= plainLen {
			continue
		}
		if b-last >= target {
			cuts = append(cuts, b)
			last = b
		}
	}
	cuts = append(cuts, plainLen)
	if !subdivide {
		return structBlockPlan{cuts: cuts}
	}

	// A span still larger than the target — a document bigger than the block,
	// or the whole structure of a single-document archive — is cut at
	// target-sized intervals rather than left oversized, so a cold read
	// decompresses a bounded amount wherever it lands (specification §8A,
	// where a block may end). Before 5.1.0-draft a single-document archive
	// was one block however large, and a point read inflated all of it.
	out := make([]int, 0, len(cuts))
	out = append(out, 0)
	for i := 1; i < len(cuts); i++ {
		for at := cuts[i-1] + target; at < cuts[i]; at += target {
			out = append(out, at)
		}
		out = append(out, cuts[i])
	}
	return structBlockPlan{cuts: out}
}

// packStructBlocks compresses each block and renders the structure region:
// a block count, the per-block length table, then the blocks.
//
// The codec is chosen once for the whole region rather than per block, so the
// header keeps describing what follows. A block that does not shrink still
// stores its compressed form, because mixing raw and compressed blocks under
// one codec byte would make the region undecodable.
func packStructBlocks(plain []byte, plan structBlockPlan, sc StructCodec, workers int) ([]byte, StructCodec, error) {
	n := len(plan.cuts) - 1
	parts := make([][]byte, n)

	// Blocks are independent by construction, so they compress concurrently and
	// are assembled in index order — the same shape, and the same determinism
	// guarantee, as the pool groups of §10.4.
	if err := compressBlocks(plain, plan, sc, parts, workers); err != nil {
		return nil, 0, err
	}
	totalStored := 0
	for _, p := range parts {
		totalStored += len(p)
	}

	// If compressing does not pay, fall back to storing the structure raw as
	// one block, so the archive never grows to gain a property it cannot use
	// (specification §5.2, the fallback rule).
	if (sc == StructDeflate || sc == StructLZ4) && totalStored >= len(plain) {
		return packStructBlocks(plain, structBlockPlan{cuts: []int{0, len(plain)}}, StructRaw, workers)
	}

	out := make([]byte, 0, 4+8*len(parts)+totalStored)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(parts)))
	for i, p := range parts {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(p)))
		out = binary.LittleEndian.AppendUint32(out, uint32(plan.cuts[i+1]-plan.cuts[i]))
	}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out, sc, nil
}

// compressBlocks fills parts[i] with block i's stored bytes.
//
// Workers claim blocks by index and write only their own slot, so the result
// does not depend on how many of them there are. A structure small enough not to
// be worth the goroutines is done in line.
func compressBlocks(plain []byte, plan structBlockPlan, sc StructCodec, parts [][]byte, workers int) error {
	seg := func(i int) []byte { return plain[plan.cuts[i]:plan.cuts[i+1]] }
	// deflateBlock rather than packStructure: the latter abandons deflate per
	// block when it does not shrink, and a region whose blocks disagree about
	// their codec cannot be described by one header byte. lz4Block may return
	// a block verbatim, because under lz4 the length table says which blocks
	// are (specification §8A, the verbatim block).
	one := func(i int) error {
		var stored []byte
		var err error
		switch sc {
		case StructDeflate:
			stored, err = deflateBlock(seg(i))
		case StructLZ4:
			stored, err = lz4Block(seg(i))
		default:
			parts[i] = seg(i)
			return nil
		}
		if err != nil {
			return err
		}
		parts[i] = stored
		return nil
	}

	if workers <= 1 || len(parts) < minParallelBlocks || len(plain) < minParallelBytes {
		for i := range parts {
			if err := one(i); err != nil {
				return err
			}
		}
		return nil
	}

	var (
		next atomic.Int64
		wg   sync.WaitGroup
		mu   sync.Mutex
		bad  error
	)
	wg.Add(min(workers, len(parts)))
	for range min(workers, len(parts)) {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(parts) {
					return
				}
				if err := one(i); err != nil {
					mu.Lock()
					if bad == nil {
						bad = err
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return bad
}

// minParallelBlocks is the block count below which the goroutines cost more
// than the compression they overlap.
const minParallelBlocks = 4
