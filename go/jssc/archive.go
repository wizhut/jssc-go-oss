package jssc

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"io"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

// Archive is an opened JSSC archive that supports selective access: pool
// strings stay compressed until something asks for them, and the structure can
// be walked without materializing the JSON tree.
//
// The structure block is inflated once at open time when the archive uses
// StructDeflate — DEFLATE is not randomly seekable. The pool is never decoded
// up front.
//
// Multi-document archives (JSS4, JSONL/NDJSON) expose DocCount documents over
// one shared pool; JSONPath treats them as a virtual root array.
//
// An Archive is not safe for concurrent use: reads populate the pool cache and
// the child-offset caches.
type Archive struct {
	stats  Stats
	limits Limits

	codec      codec.Codec
	indexWidth int
	poolCount  int

	contentFmt ContentFormat
	docCount   int
	docOff     []int // structure offset of each document root

	// vals reads the value pool a segment at a time; groupSize is how many
	// entries each group holds. Decoding one entry decompresses its whole
	// group, and the entries that came with it are cached rather than thrown
	// away.
	vals      *poolReader
	groupSize int

	// keyIndex is the reverse of the pool for entries already decoded. Object
	// members are sorted by key index, so knowing a key's index turns field
	// lookup into an integer binary search.
	keyIndex      map[string]int
	keyIndexStale bool

	// poolMisses remembers strings searchPool has already reported absent, so a
	// path evaluated against many objects does not repeat the search.
	poolMisses map[string]struct{}

	// codecAppender is a.codec when it can decode into a caller's buffer.
	codecAppender codec.Appender

	// The key pool (JSS8, 5.3.0-draft): its own section, index space, and
	// width. Before the split keys is vals, so key indices and pool indices
	// coincide exactly as the old layout defined.
	split    bool
	keyCount int
	keyWidth int
	keys     *poolReader

	// paths is the JSS8 key-path filter, when the archive carries one. Its only
	// answer that means anything is "no": a path it rejects is not in the
	// document, and a path it accepts still has to be walked for.
	paths pathFilter

	// keyDir is the JSS6 key hash directory: hash of key to pool index, for
	// every string the document uses as an object key. When it is present a
	// field lookup resolves its key to an integer without decompressing any
	// pool group, and a key that is absent from it is absent from the document.
	keyDir keyDirectory

	// structure addresses the structure by plain offset. Through JSS6 it wraps
	// one inflated block; a JSS7 archive inflates blocks on demand, so opening
	// costs nothing and reading one document costs one block.
	structure *structBlocks

	lenPrefix     bool
	layoutOffsets bool
	// adaptiveOffsets is set for JSS5, where each container states its offset
	// table width instead of the reader inferring it from the child count.
	adaptiveOffsets bool

	// childOff maps a container's value start to its children's value starts;
	// childKeyOff does the same for object member keys.
	childOff    map[int][]int
	childKeyOff map[int][]int

	// DecodedStrings counts pool entries materialized during the last query,
	// including the ones that came along inside a decompressed group.
	DecodedStrings int
	// DecodedGroups counts how many pool groups had to be decompressed. With a
	// group size above 1 this, not DecodedStrings, is the work actually done.
	DecodedGroups int
	// SkippedValues counts subtrees jumped over rather than parsed during the
	// last query.
	SkippedValues int
	// FilteredOut records that the last query returned nothing because the
	// key-path filter rejected its path, so no walk was attempted at all.
	FilteredOut bool

	// rootOffsets is set for JSS7, where the root table holds absolute offsets.
	rootOffsets bool

	// matchLimit caps how many matches a query collects; 0 means unlimited.
	matchLimit int
	// wantPaths says whether the current query's answer includes the normalized
	// path of each match. When it does not, the walk names nothing, and naming
	// is what makes it decode keys. Defaults to true so that a walk started by
	// anything other than query() keeps the old behaviour.
	wantPaths bool
}

// Open reads a complete archive from r. Pool entries are indexed but not
// decoded; the structure block is inflated if it was deflated.
func Open(r io.Reader) (*Archive, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return OpenBytes(raw)
}

// OpenBytes is Open for an archive already in memory. It keeps references into
// raw, so callers must not modify it afterwards.
func OpenBytes(raw []byte) (*Archive, error) {
	return OpenBytesWith(raw, Limits{})
}

// OpenBytesWith is OpenBytes with explicit decode limits. Callers handling
// untrusted archives should set at least Limits.MaxPlainStructure.
func OpenBytesWith(raw []byte, lim Limits) (*Archive, error) {
	h, pos, err := parseHeader(raw)
	if err != nil {
		return nil, err
	}
	if lim.MaxPoolCount > 0 && int64(h.poolCount) > int64(lim.MaxPoolCount) {
		return nil, errLimitf("archive declares %d pool entries, limit is %d", h.poolCount, lim.MaxPoolCount)
	}

	a := &Archive{
		wantPaths:     true,
		limits:        lim,
		indexWidth:    h.indexWidth,
		poolCount:     int(h.poolCount),
		contentFmt:    h.contentFmt,
		docCount:      int(h.docCount),
		lenPrefix:     h.layout&LayoutLenPrefix != 0,
		layoutOffsets: h.layout&LayoutOffsets != 0,

		adaptiveOffsets: h.adaptiveOffsets(),
	}

	c, err := codec.NewByID(h.algo)
	if err != nil {
		return nil, errUnsupportedf("%v", err)
	}
	br := bytes.NewReader(raw[pos:])
	before := br.Len()
	if err := c.ReadState(br); err != nil {
		return nil, errCorruptf("read %s codec state: %v", c.Name(), err)
	}
	stateBytes := before - br.Len()
	pos += stateBytes
	a.codec = c
	// Resolved once: a group decode should not repeat a type assertion the
	// archive can answer at open.
	a.codecAppender, _ = c.(codec.Appender)

	a.split = h.splitPools()
	a.keyWidth = h.keyWidth()
	a.keyCount = int(h.keyCount)
	keyPoolBytes := 0
	if a.split {
		// The key pool section precedes the value pool section, each declaring
		// its own length (specification §7).
		if pos+4 > len(raw) {
			return nil, errTruncatedf("key pool section length")
		}
		klen := int(binary.LittleEndian.Uint32(raw[pos:]))
		pos += 4
		if klen > len(raw)-pos {
			return nil, errTruncatedf("key pool section: %d bytes declared, %d available", klen, len(raw)-pos)
		}
		a.keys, err = newPoolReader("key pool", raw, pos, pos+klen, a.keyCount, h.groupSize(),
			h.varintPool(), h.opportunisticPool(), h.groupDirectory())
		if err != nil {
			return nil, err
		}
		keyPoolBytes = 4 + klen
		pos += klen
	}

	poolBytes, pos, err := a.indexPool(raw, pos, h)
	if err != nil {
		return nil, err
	}
	if !a.split {
		// One pool, one index space: the key side is the value side, so key
		// indices are pool indices, which is what the pre-split layouts encode.
		a.keyCount = a.poolCount
		a.keys = a.vals
	}

	if h.keyDir() {
		a.keyDir, pos, err = readKeyDirectory(raw, pos, a.keyWidth, a.keyCount, h.completeKeyDirectory())
		if err != nil {
			return nil, err
		}
	}

	if h.pathFilter() {
		a.paths, pos, err = readPathFilter(raw, pos)
		if err != nil {
			return nil, err
		}
	}

	if pos+4 > len(raw) {
		return nil, errTruncatedf("structure length")
	}
	storedLen := int(binary.LittleEndian.Uint32(raw[pos:]))
	pos += 4
	if storedLen < 0 || pos > len(raw) || storedLen > len(raw)-pos {
		return nil, errTruncatedf("structure: %d bytes declared, %d available", storedLen, len(raw)-pos)
	}
	if h.blockedStructure() {
		a.structure, err = newStructBlocks(raw[pos:pos+storedLen], h.structCodec, lim.MaxPlainStructure)
	} else {
		var plain []byte
		plain, err = unpackStructure(raw[pos:pos+storedLen], h.structCodec, lim.MaxPlainStructure)
		if err == nil {
			a.structure = newSingleBlock(plain)
		}
	}
	if err != nil {
		return nil, err
	}
	a.rootOffsets = h.rootOffsets()

	if h.version >= 4 {
		if err := a.indexDocs(); err != nil {
			return nil, err
		}
	} else {
		a.docOff = []int{0}
	}

	a.stats = Stats{
		Algorithm:     c.Name(),
		StructCodec:   h.structCodec,
		StructLayout:  h.layout,
		ContentFormat: a.contentFmt,
		Version:       h.version,
		DocCount:      a.docCount,
		IndexWidth:    a.indexWidth,
		UniqueStrings: a.poolCount,
		PoolBytes:     poolBytes,
		StructBytes:   storedLen,
		StructPlain:   a.structure.len(),
		HeaderBytes:   h.size(),
		StateBytes:    stateBytes,
		KeyDirEntries: a.keyDir.len(),
	}
	if h.keyDir() {
		a.stats.KeyDirBytes = 4 + a.keyDir.len()*(4+a.keyWidth)
	}
	if a.split {
		a.stats.KeyWidth = a.keyWidth
		a.stats.DistinctKeys = a.keyCount
		a.stats.KeyPoolBytes = keyPoolBytes
	}
	return a, nil
}

// indexPool prepares pool access without doing any of it.
//
// Opening notes where the section's parts are and stops there: no group index,
// no entry cache. On a query that returns a number, an inline string, or
// nothing at all — a rejected path, an absent key — none of those allocations
// are ever made, and that is the common case for a field lookup. The pool
// reader builds them a segment at a time as strings are actually wanted.
func (a *Archive) indexPool(raw []byte, pos int, h header) (poolBytes, next int, err error) {
	a.groupSize = h.groupSize()

	// From JSS8 the section declares its own stored length, so open finds the
	// key directory in O(1), and the group directory lets the first read walk
	// one segment rather than the section (specification §7.4). PoolBytes on
	// open is the declared section length — framing included — since the
	// payload split is not known until the framing is walked.
	if h.poolSectionLength() {
		if pos+4 > len(raw) {
			return 0, 0, errTruncatedf("pool section length")
		}
		plen := int(binary.LittleEndian.Uint32(raw[pos:]))
		pos += 4
		if plen > len(raw)-pos {
			return 0, 0, errTruncatedf("pool section: %d bytes declared, %d available", plen, len(raw)-pos)
		}
		a.vals, err = newPoolReader("pool", raw, pos, pos+plen, a.poolCount, a.groupSize,
			h.varintPool(), h.opportunisticPool(), h.groupDirectory())
		if err != nil {
			return 0, 0, err
		}
		return plen, pos + plen, nil
	}

	next, poolBytes, err = scanPoolSection(raw, pos, a.poolCount, a.groupSize,
		h.varintPool(), h.opportunisticPool())
	if err != nil {
		return 0, 0, err
	}
	a.vals, err = newPoolReader("pool", raw, pos, next, a.poolCount, a.groupSize,
		h.varintPool(), h.opportunisticPool(), false)
	if err != nil {
		return 0, 0, err
	}
	return poolBytes, next, nil
}

// indexDocs prepares document lookup.
//
// Through JSS6 the root table holds lengths, so every offset is a prefix sum and
// the whole table has to be walked at open: O(N) time and an int per document,
// which at a million rows is eight megabytes before anything is read. JSS7
// stores absolute offsets instead, so document i is one read of four bytes and
// opening does nothing at all.
func (a *Archive) indexDocs() error {
	base := int64(a.docCount) * 4
	if base > int64(a.structure.len()) {
		return errTruncatedf("multi-document length table")
	}
	if a.rootOffsets {
		return nil
	}
	a.docOff = make([]int, a.docCount)
	pos := base
	for i := range a.docCount {
		n, err := a.structure.u32(i * 4)
		if err != nil {
			return errTruncatedf("multi-document length table")
		}
		a.docOff[i] = int(pos)
		pos += int64(n)
		if pos > int64(a.structure.len()) {
			return errTruncatedf("document %d root", i)
		}
	}
	return nil
}

// docStart returns the plain offset of document i's root.
func (a *Archive) docStart(i int) (int, error) {
	if i < 0 || i >= a.docCount {
		return 0, errCorruptf("document %d out of range [0,%d)", i, a.docCount)
	}
	if !a.rootOffsets {
		return a.docOff[i], nil
	}
	off, err := a.structure.u32(i * 4)
	if err != nil {
		return 0, errTruncatedf("root offset %d", i)
	}
	if int(off) < 4*a.docCount || int(off) > a.structure.len() {
		return 0, errCorruptf("document %d starts at %d, outside the structure", i, off)
	}
	return int(off), nil
}

// unpackStructure reverses packStructure. maxPlain, when positive, bounds how
// far a deflated block is allowed to expand (specification §15.1).
func unpackStructure(stored []byte, sc StructCodec, maxPlain int64) ([]byte, error) {
	switch sc {
	case StructRaw:
		return stored, nil
	case StructDeflate:
		zr := flate.NewReader(bytes.NewReader(stored))
		defer zr.Close()
		var r io.Reader = zr
		if maxPlain > 0 {
			r = io.LimitReader(zr, maxPlain+1)
		}
		out, err := io.ReadAll(r)
		if err != nil {
			return nil, errCorruptf("inflate structure: %v", err)
		}
		if maxPlain > 0 && int64(len(out)) > maxPlain {
			return nil, errLimitf("structure inflates past %d bytes", maxPlain)
		}
		return out, nil
	default:
		return nil, errUnsupportedf("structure codec %d", uint8(sc))
	}
}

// DocCount returns the number of top-level documents: 1 for JSON, N for
// JSONL/NDJSON.
func (a *Archive) DocCount() int { return a.docCount }

// ContentFormat returns the textual form recorded in the archive header.
func (a *Archive) ContentFormat() ContentFormat { return a.contentFmt }

// MultiDoc reports whether the archive stores multiple top-level documents.
func (a *Archive) MultiDoc() bool {
	return a.contentFmt.LineOriented() || a.docCount > 1
}

// PoolCount returns the number of strings in the archive's pool.
func (a *Archive) PoolCount() int { return a.poolCount }

// Stats returns archive metadata collected at open time.
func (a *Archive) Stats() *Stats {
	st := a.stats
	return &st
}

// String returns pool entry i, decompressing and caching its group on first use.
//
// With a group size above 1, reaching one entry decompresses its neighbours
// too. They are cached rather than discarded, so the cost is paid once per
// group; DecodedGroups counts how often that happened, which is the honest
// measure of the work a selective query did.
func (a *Archive) String(i int) (string, error) {
	return a.poolString(a.vals, i)
}

// keyString returns key-pool entry i, decoding its group on first touch.
func (a *Archive) keyString(i int) (string, error) {
	return a.poolString(a.keys, i)
}

// poolString returns entry i of pool p. The first read prepares p; reaching
// the entry walks at most its own segment's lengths (specification
// §7.4), and the group it decodes stays cached.
//
// A full decode calls this once per string value, so the cached case is kept
// to a bounds check, a slice index and a bit test.
func (a *Archive) poolString(p *poolReader, i int) (string, error) {
	if uint(i) >= uint(p.count) {
		return "", errCorruptf("%s index %d out of range [0,%d)", p.name, i, p.count)
	}
	if !p.ready {
		if err := p.prepare(); err != nil {
			return "", err
		}
	}
	g := i
	if p.groupSize != 1 {
		g = i / p.groupSize
	}
	s := p.segs[uint(g)/segmentGroups]
	if s == nil {
		var err error
		if s, err = p.segment(g / segmentGroups); err != nil {
			return "", err
		}
	}
	j := uint(g) % segmentGroups
	if s.done&(1<<j) == 0 {
		if err := a.decodeGroup(p, s, g); err != nil {
			return "", err
		}
	}
	return s.strs[i-(g-int(j))*p.groupSize], nil
}

// decodeGroup inflates group g and points its cache entries at the inflated
// bytes. Nothing is copied out of the plaintext and nothing is boxed: the group
// costs one allocation however many entries it holds.
func (a *Archive) decodeGroup(p *poolReader, s *poolSegment, g int) error {
	plain, err := inflateGroup(a.codec, a.codecAppender, p, s, g)
	if err != nil {
		return errCorruptf("decompress %s group %d: %v", p.name, g, err)
	}
	lo, hi, err := p.fill(s, g, plain)
	if err != nil {
		return err
	}
	a.DecodedStrings += hi - lo
	a.DecodedGroups++
	if p == a.vals {
		// The reverse map is invalidated rather than extended: a full decode
		// never asks a reverse question, and should not pay to keep one up to
		// date.
		a.keyIndexStale = true
	}
	return nil
}

// LookupIndex resolves a string to its pool index without decoding the pool if
// it can avoid it.
//
// With a JSS6 key hash directory and a string that is used as an object key,
// this decompresses nothing at all: the hash is computed, the directory is
// searched, and the answer is exact in both directions — a miss means the string
// is not a key anywhere in the document.
//
// Otherwise it falls back to a binary search over the pool, which is sorted, and
// costs about log2(groups)+1 group decompressions rather than the whole pool.
func (a *Archive) LookupIndex(s string) (int, bool, error) {
	if i, ok := a.knownPoolIndex(s); ok {
		return i, true, nil
	}
	if !a.split {
		// One index space: a directory hit is a pool index. After the split
		// the directory speaks the key space and cannot answer for a value.
		if i, ok := a.keyDir.lookup(keyHash(s)); ok {
			return i, true, nil
		}
	}
	if _, missing := a.poolMisses[s]; missing {
		return 0, false, nil
	}
	i, ok, err := a.searchPool(s)
	if err == nil && !ok {
		// A miss costs log2(groups) decompressions, and a query like "$..field"
		// asks the same question at every object. Remember the answer.
		if a.poolMisses == nil {
			a.poolMisses = make(map[string]struct{})
		}
		a.poolMisses[s] = struct{}{}
	}
	return i, ok, err
}

// LookupKeyIndex is LookupIndex restricted to the key hash directory. It never
// touches the pool, and reports ok=false both for a string that is not a key and
// for an archive that carries no directory — so a caller that wants a definitive
// answer should use LookupIndex.
func (a *Archive) LookupKeyIndex(s string) (int, bool) {
	return a.keyDir.lookup(keyHash(s))
}

// lookupFieldIndex resolves an object key to its pool index.
//
// When the archive carries a key directory the answer comes from it alone and
// is definitive in both directions, because the directory lists every key in
// the document: a miss means no object anywhere has that member, and no pool
// group is decompressed to establish it. Without a directory it falls back to
// searching the pool, which is still definitive — keys are always pooled — but
// costs log2(groups)+1 decompressions the first time.
func (a *Archive) lookupFieldIndex(key string) (int, bool, error) {
	if !a.hasKeyDir() {
		if a.split {
			return a.searchKeyPool(key)
		}
		return a.LookupIndex(key)
	}
	lo, hi := a.keyDir.candidates(keyHash(key))
	switch {
	case lo == hi:
		// The directory lists every key, so this is proof, not a hint.
		return 0, false, nil
	case hi-lo == 1:
		return a.keyDir.indexAt(lo), true, nil
	}
	// A JSS8 collision run: settle it against the key pool, which costs one
	// group for the run and leaves every other key in the document untouched.
	for i := lo; i < hi; i++ {
		s, err := a.keyString(a.keyDir.indexAt(i))
		if err != nil {
			return 0, false, err
		}
		if s == key {
			return a.keyDir.indexAt(i), true, nil
		}
	}
	return 0, false, nil
}

// hasKeyDir reports whether this archive can answer key lookups from the
// directory alone. When it can, a key absent from the directory is absent from
// the document, which is what lets a field lookup fail without reading the pool.
func (a *Archive) hasKeyDir() bool { return a.keyDir.len() > 0 }

// searchPool binary searches the lexicographically sorted pool. It probes group
// boundaries first, then the one group that can hold s, so a lookup costs about
// log2(groups)+1 decompressions instead of decoding the pool.
func (a *Archive) searchPool(s string) (int, bool, error) {
	if a.poolCount == 0 {
		return 0, false, nil
	}
	groups := groupCount(a.poolCount, a.groupSize)

	// The last group whose first entry is <= s is the only one that can hold s.
	lo, hi := 0, groups-1
	for lo < hi {
		mid := int(uint(lo+hi+1) >> 1)
		first, err := a.String(mid * a.groupSize)
		if err != nil {
			return 0, false, err
		}
		if first <= s {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	gLo, gHi := groupRange(lo, a.groupSize, a.poolCount)
	for i := gLo; i < gHi; i++ {
		e, err := a.String(i)
		if err != nil {
			return 0, false, err
		}
		switch {
		case e == s:
			return i, true, nil
		case e > s:
			return 0, false, nil
		}
	}
	return 0, false, nil
}

// knownPoolIndex returns the pool index of s when s has already been decoded.
func (a *Archive) knownPoolIndex(s string) (int, bool) {
	if a.keyIndexStale || a.keyIndex == nil {
		a.buildKeyIndex()
	}
	if a.keyIndex == nil {
		return 0, false
	}
	i, ok := a.keyIndex[s]
	return i, ok
}

// buildKeyIndex fills the reverse map from the groups decoded so far. It is
// built on the first reverse lookup, not on the decode path: a full decode
// never asks a reverse question and should not pay for the map.
func (a *Archive) buildKeyIndex() {
	if a.keyIndex == nil {
		a.keyIndex = make(map[string]int, a.DecodedStrings)
	}
	p := a.vals
	for k, s := range p.segs {
		if s == nil {
			continue
		}
		first := k * segmentGroups
		for g := first; g < min(first+segmentGroups, p.groups); g++ {
			if !s.cached(g) {
				continue
			}
			lo, hi := groupRange(g, p.groupSize, p.count)
			for i := lo; i < hi; i++ {
				a.keyIndex[p.entry(s, i)] = i
			}
		}
	}
	a.keyIndexStale = false
}

// Doc materializes document i. For a single-document archive the only valid
// index is 0.
func (a *Archive) Doc(i int) (any, error) {
	if i < 0 || i >= a.docCount {
		return nil, errCorruptf("document %d out of range [0,%d)", i, a.docCount)
	}
	start, err := a.docStart(i)
	if err != nil {
		return nil, err
	}
	v, _, err := a.readValue(start)
	return v, err
}

// DecodeAll materializes the whole archive. Prefer Query for selective access.
// A multi-document archive yields []any of its roots.
func (a *Archive) DecodeAll() (any, error) {
	// Every string will be read, so inflate the pool up front and in parallel
	// rather than one group at a time along the walk.
	if err := a.WarmPool(); err != nil {
		return nil, err
	}
	if a.MultiDoc() {
		docs := make([]any, a.docCount)
		for i := range a.docCount {
			v, err := a.Doc(i)
			if err != nil {
				return nil, errCorruptf("document %d: %v", i, err)
			}
			docs[i] = v
		}
		return docs, nil
	}
	return a.Doc(0)
}

// MarshalJSON reconstructs the archive as a single JSON value. Multi-document
// archives become a JSON array of roots; use MarshalText for JSONL/NDJSON.
func (a *Archive) MarshalJSON() ([]byte, error) {
	v, err := a.DecodeAll()
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// MarshalText reconstructs the original textual form: one JSON value for
// FormatJSON, newline-delimited documents for JSONL and NDJSON.
func (a *Archive) MarshalText() ([]byte, error) {
	if !a.contentFmt.LineOriented() {
		return a.MarshalJSON()
	}
	docs := make([]any, a.docCount)
	for i := range a.docCount {
		v, err := a.Doc(i)
		if err != nil {
			return nil, errCorruptf("document %d: %v", i, err)
		}
		docs[i] = v
	}
	return marshalLineDocs(docs)
}

func (a *Archive) readIndexAt(pos int) (idx, next int, err error) {
	return readIndex(a.structure, pos, a.indexWidth)
}

// skipValue jumps past the value at pos and counts the skip.
func (a *Archive) skipValue(pos int) (int, error) {
	a.SkippedValues++
	return a.valueEndPos(pos)
}

// peekTag returns the tag byte of the value at pos without decoding it.
func (a *Archive) peekTag(pos int) (byte, error) {
	tagPos, err := valueTagPos(a.structure, pos, a.lenPrefix)
	if err != nil {
		return 0, err
	}
	return a.structure.byteAt(tagPos)
}

// readValue materializes the framed value at pos and returns the offset just
// past it.
func (a *Archive) readValue(pos int) (any, int, error) {
	return a.readFramed(pos, a.structure.len(), a.limits.maxDepth())
}

// readFramed reads one framed value, honoring the enclosing container's limit.
func (a *Archive) readFramed(pos, limit, depth int) (any, int, error) {
	if !a.lenPrefix {
		return a.readTagged(pos, limit, depth)
	}
	payloadLen, n, err := readUvarint(a.structure, pos)
	if err != nil {
		return nil, 0, err
	}
	start := pos + n
	end := start + int(payloadLen)
	if end < start || end > limit || end > a.structure.len() {
		return nil, 0, errTruncatedf("value at %d claims %d payload bytes", pos, payloadLen)
	}
	v, _, err := a.readTagged(start, end, depth)
	return v, end, err
}

func (a *Archive) readTagged(pos, limit, depth int) (any, int, error) {
	if depth <= 0 {
		return nil, 0, errLimitf("nesting deeper than the decode limit")
	}
	if pos >= limit || pos >= a.structure.len() {
		return nil, 0, errTruncatedf("value at %d", pos)
	}
	tag, err := a.structure.byteAt(pos)
	if err != nil {
		return nil, 0, err
	}
	pos++

	switch tag {
	case tagNull:
		return nil, pos, nil
	case tagFalse:
		return false, pos, nil
	case tagTrue:
		return true, pos, nil

	case tagNumber:
		if pos >= limit {
			return nil, 0, errTruncatedf("number length")
		}
		nb, err := a.structure.byteAt(pos)
		if err != nil {
			return nil, 0, errTruncatedf("number length")
		}
		n := int(nb)
		pos++
		if pos+n > limit {
			return nil, 0, errTruncatedf("number payload")
		}
		raw, err := a.structure.at(pos, n)
		if err != nil {
			return nil, 0, err
		}
		return json.Number(raw), pos + n, nil

	case tagString:
		idx, next, err := a.readIndexAt(pos)
		if err != nil {
			return nil, 0, err
		}
		if next > limit {
			return nil, 0, errTruncatedf("pool index at %d", pos)
		}
		s, err := a.String(idx)
		if err != nil {
			return nil, 0, err
		}
		return s, next, nil

	case tagArray:
		count, childStart, err := readContainerCount(a.structure, pos, limit, a.layoutOffsets, a.adaptiveOffsets)
		if err != nil {
			return nil, 0, err
		}
		pos = childStart
		arr := make([]any, count)
		for i := range count {
			var el any
			el, pos, err = a.readFramed(pos, limit, depth-1)
			if err != nil {
				return nil, 0, err
			}
			arr[i] = el
		}
		return arr, pos, nil

	case tagObject:
		count, childStart, err := readContainerCount(a.structure, pos, limit, a.layoutOffsets, a.adaptiveOffsets)
		if err != nil {
			return nil, 0, err
		}
		pos = childStart
		obj := make(map[string]any, count)
		for range count {
			idx, next, err := a.readKeyIndexAt(pos)
			if err != nil {
				return nil, 0, err
			}
			if next > limit {
				return nil, 0, errTruncatedf("key index at %d", pos)
			}
			key, err := a.keyString(idx)
			if err != nil {
				return nil, 0, err
			}
			var val any
			val, pos, err = a.readFramed(next, limit, depth-1)
			if err != nil {
				return nil, 0, err
			}
			obj[key] = val
		}
		return obj, pos, nil

	default:
		if tag >= tagInline0 && tag <= tagInlineMax {
			n := int(tag - tagInline0)
			if pos+n > limit {
				return nil, 0, errTruncatedf("inline string")
			}
			raw, err := a.structure.at(pos, n)
			if err != nil {
				return nil, 0, err
			}
			return string(raw), pos + n, nil
		}
		return nil, 0, errCorruptf("unknown structure tag 0x%02x at %d", tag, pos-1)
	}
}

// ---------------------------------------------------------------- key pool --
//
// The key pool is read by the same poolReader as the value pool (keyString,
// above). Before the split (JSS1–JSS7) the two sides are one reader over one
// section, and either accessor answers with the same bytes.

// searchKeyPool binary searches the sorted key pool, for archives written
// without a key directory. Keys are always pooled, so a miss is proof.
func (a *Archive) searchKeyPool(s string) (int, bool, error) {
	if a.keyCount == 0 {
		return 0, false, nil
	}
	groups := groupCount(a.keyCount, a.groupSize)
	lo, hi := 0, groups-1
	for lo < hi {
		mid := int(uint(lo+hi+1) >> 1)
		first, err := a.keyString(mid * a.groupSize)
		if err != nil {
			return 0, false, err
		}
		if first <= s {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	gLo, gHi := groupRange(lo, a.groupSize, a.keyCount)
	for i := gLo; i < gHi; i++ {
		e, err := a.keyString(i)
		if err != nil {
			return 0, false, err
		}
		switch {
		case e == s:
			return i, true, nil
		case e > s:
			return 0, false, nil
		}
	}
	return 0, false, nil
}

// readKeyIndexAt reads a member's key reference, which is keyWidth bytes.
func (a *Archive) readKeyIndexAt(pos int) (idx, next int, err error) {
	return readIndex(a.structure, pos, a.keyWidth)
}
