package jssc

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strconv"
	"sync"

	lz4 "github.com/pierrec/lz4/v4"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

// Encode parses JSON from raw, builds a string pool compressed with algoName,
// and writes a JSSC archive to w with the default structure codec.
func Encode(raw []byte, w io.Writer, algoName string) (*Stats, error) {
	return EncodeWith(raw, w, Options{Algorithm: algoName, Structure: StructLZ4})
}

// EncodeWith is Encode with explicit options. The whole input is held in
// memory; see EncodeFile and EncodeFrom for the streaming line-oriented path.
func EncodeWith(raw []byte, w io.Writer, opt Options) (*Stats, error) {
	if err := opt.validate(); err != nil {
		return nil, err
	}
	docs, truncated, err := parseDocs(raw, opt)
	if err != nil {
		return nil, err
	}

	sets := newStringSets()
	for _, d := range docs {
		sets.gather(d)
	}

	rawBytes := len(raw)
	if truncated {
		// Stats should describe the corpus actually encoded, not the whole input.
		if trimmed, err := marshalLineDocs(docs); err == nil {
			rawBytes = len(trimmed)
		}
	}
	return encodeDocs(w, opt, docs, sets, rawBytes)
}

// parseDocs turns the input text into one or more parsed roots, preserving
// number lexemes. It reports whether MaxDocs cut the input short.
func parseDocs(raw []byte, opt Options) (docs []any, truncated bool, err error) {
	if opt.Format.LineOriented() {
		return parseLineDocs(raw, opt.MaxDocs)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, errInvalidf("empty input")
	}
	root, err := parseJSONValue(raw)
	if err != nil {
		return nil, false, err
	}
	return []any{root}, false, nil
}

// encodeDocs performs the shared tail of every in-memory encode: train the pool
// codec, serialize each root, pack the structure, and emit the archive.
func encodeDocs(w io.Writer, opt Options, docs []any, sets *stringSets, rawBytes int) (*Stats, error) {
	if len(docs) == 0 {
		return nil, errInvalidf("empty input")
	}

	layout := opt.layout()
	h := header{
		layout:     layout,
		contentFmt: opt.Format,
		docCount:   uint32(len(docs)),
		groupShift: opt.poolGroupShift(),
	}

	// Keys and values go into separate pools with separate index spaces
	// (specification §7).
	plan := buildPool(sets)
	if plan.values.len() > MaxPoolCount || plan.keys.len() > MaxPoolCount {
		return nil, errLimitf("pool holds %d strings (max %d)",
			max(plan.values.len(), plan.keys.len()), MaxPoolCount)
	}
	h.indexWidth = plan.width
	h.poolCount = uint32(plan.values.len())
	h.keyCount = uint32(plan.keys.len())

	keyDir, keyDirEntries := keyDirectorySection(plan, opt)

	c, keySec, pool, err := buildPoolSections(opt.Algorithm, plan, h, opt.concurrency())
	if err != nil {
		return nil, err
	}

	sw := &structWriter{
		values:   plan.values,
		keys:     plan.keys,
		width:    plan.width,
		keyWidth: plan.keyWidth,
		layout:   layout,
		maxDepth: opt.maxDepth(),
	}

	// The structure is a table of root offsets followed by the roots, a single
	// JSON document included, so document i opens with one read
	// (specification §4.2, §8A.4).
	var body bytes.Buffer
	docStarts := make([]int, len(docs))
	base := 4 * len(docs)
	for i, doc := range docs {
		start := body.Len()
		if err := sw.writeValue(&body, doc); err != nil {
			return nil, err
		}
		if body.Len()-start > math.MaxUint32 {
			return nil, errLimitf("document %d root exceeds uint32 bytes", i)
		}
		docStarts[i] = base + start
	}
	var structBuf bytes.Buffer
	structBuf.Grow(base + body.Len())
	for i, start := range docStarts {
		if start > math.MaxUint32 {
			return nil, errLimitf("document %d starts past uint32 bytes", i)
		}
		writeUint32(&structBuf, uint32(start))
	}
	structBuf.Write(body.Bytes())
	plain := structBuf.Bytes()

	blockPlan := planStructBlocks(len(plain), opt.structBlockBytes(), base, docStarts,
		opt.Structure != StructRaw)
	structBlocks := len(blockPlan.cuts) - 1
	stored, structCodec, err := packStructBlocks(plain, blockPlan, opt.Structure, opt.concurrency())
	if err != nil {
		return nil, err
	}
	if len(stored) > MaxStoredStructure {
		return nil, errLimitf("stored structure is %d bytes (struct_len max %d)", len(stored), uint64(MaxStoredStructure))
	}

	var stateBuf bytes.Buffer
	if err := c.WriteState(&stateBuf); err != nil {
		return nil, errInvalidf("write codec state: %v", err)
	}

	h.algo = c.ID()
	h.structCodec = structCodec

	// The filter is decided last, because the rule is relative: it is emitted
	// only when it is small beside the pool and structure it saves a walk
	// through, and neither of those sizes is known until here. The layout bit
	// is what announces it, so a reader that finds the bit clear never looks.
	pathFilter := pathFilterSection(opt, plan.paths, pool.payload+len(stored))
	if len(pathFilter) > 0 {
		h.layout |= LayoutPathFilter
	}

	if err := writeArchive(w, h, stateBuf.Bytes(), keySec, pool.section, keyDir, pathFilter, stored); err != nil {
		return nil, err
	}

	st := &Stats{
		Algorithm:     c.Name(),
		StructCodec:   structCodec,
		StructLayout:  h.layout,
		ContentFormat: opt.Format,
		Version:       LatestVersion,
		DocCount:      len(docs),
		IndexWidth:    plan.width,
		KeyWidth:      plan.keyWidth,
		UniqueStrings: plan.values.len(),
		DistinctKeys:  plan.keys.len(),
		RawBytes:      rawBytes,
		PoolBytes:     pool.payload,
		PoolRawBytes:  pool.rawBytes,
		StructBytes:   len(stored),
		StructPlain:   len(plain),
		StateBytes:    stateBuf.Len(),
		HeaderBytes:   h.size(),
		InlineStrings: plan.inlineDistinct,
		InlineRefs:    sw.inlineRefs,
		InlineBytes:   sw.inlineBytes,
		PoolGroupSize: h.groupSize(),
		PoolGroups:    groupCount(plan.values.len(), h.groupSize()),
		PoolFraming:   len(pool.section) + len(keySec) - pool.payload,
		KeyPoolBytes:  len(keySec),
		OffsetTables:  sw.offsetTableBytes,
		KeyDirEntries: keyDirEntries,
		KeyDirBytes:   len(keyDir),

		PathFilterBytes: len(pathFilter),
		PathFilterPaths: pathFilterPaths(pathFilter, plan.paths),
		PoolRawGroups:   pool.rawGroups,

		StructBlocks:     structBlocks,
		StructBlockBytes: opt.structBlockBytes(),
	}
	st.TotalBytes = st.HeaderBytes + st.StateBytes + len(keySec) + len(pool.section) +
		len(keyDir) + len(pathFilter) + 4 + st.StructBytes
	return st, nil
}

// keyDirectorySection builds the key hash directory over every key. An encoder
// that declines to build one still writes the count, so the section is not
// optional on the wire, only its contents are (specification §7A.5).
func keyDirectorySection(plan poolPlan, opt Options) (section []byte, entries int) {
	if opt.OmitKeyDirectory {
		return emptyKeyDirectory, 0
	}
	return buildKeyDirectory(plan.keys, plan.keyWidth), plan.keys.len()
}

// pathFilterSection builds the key-path filter, or nil when this archive
// should not carry one (specification §7B.6).
func pathFilterSection(opt Options, paths []uint32, indexedBytes int) []byte {
	if opt.OmitPathFilter {
		return nil
	}
	f := buildPathFilter(paths)
	if !worthPathFilter(len(f), indexedBytes) {
		return nil
	}
	return f
}

// pathFilterPaths reports how many paths a section indexes, and zero when there
// is no section — so the stat says "not carried" rather than "carried none".
func pathFilterPaths(section []byte, paths []uint32) int {
	if len(section) == 0 {
		return 0
	}
	return len(paths)
}

// builtPool is the finished pool section: the framed, compressed groups plus
// the accounting the report wants.
type builtPool struct {
	section  []byte // framing and payload, ready to write
	payload  int    // stored bytes, framing excluded
	rawBytes int    // the strings before compression
	// rawGroups counts the groups stored uncompressed because compressing them
	// did not help. It is reported so a corpus can say how often the
	// opportunistic rule fired rather than leaving it to be inferred.
	rawGroups int
}

// buildPoolSections instantiates the named codec, trains it on every pooled
// string — both pools together, so one symbol table serves keys and values
// (specification §6.1) — and compresses each pool into its own section.
func buildPoolSections(algoName string, plan poolPlan, h header, concurrency int) (codec.Codec, []byte, *builtPool, error) {
	c, err := codec.NewByName(algoName)
	if err != nil {
		return nil, nil, nil, err
	}
	samples := append(append([][]byte{}, plan.keys.samples()...), plan.values.samples()...)
	if err := c.Train(samples); err != nil {
		return nil, nil, nil, errInvalidf("train %s: %v", c.Name(), err)
	}

	ks, _, _, err := encodePool(c, plan.keys.strings(), h.groupShift, concurrency)
	if err != nil {
		return nil, nil, nil, err
	}
	keySection, err := framePoolSection(ks)
	if err != nil {
		return nil, nil, nil, err
	}

	section, payload, rawGroups, err := encodePool(c, plan.values.strings(), h.groupShift, concurrency)
	if err != nil {
		return nil, nil, nil, err
	}
	section, err = framePoolSection(section)
	if err != nil {
		return nil, nil, nil, err
	}

	out := &builtPool{section: section, payload: payload, rawGroups: rawGroups}
	for _, s := range plan.values.strings() {
		out.rawBytes += len(s)
	}
	for _, s := range plan.keys.strings() {
		out.rawBytes += len(s)
	}
	return c, keySection, out, nil
}

// framePoolSection prefixes a pool section with its stored length
// (specification §7.4).
func framePoolSection(section []byte) ([]byte, error) {
	if len(section) > math.MaxUint32 {
		return nil, errLimitf("pool section is %d bytes (pool_len max %d)",
			len(section), uint64(math.MaxUint32))
	}
	framed := make([]byte, 4+len(section))
	binary.LittleEndian.PutUint32(framed, uint32(len(section)))
	copy(framed[4:], section)
	return framed, nil
}

// writeArchive emits the sections in the order of specification §4.1: header,
// codec state, key pool, value pool, key directory, path filter, struct_len
// and the structure.
func writeArchive(w io.Writer, h header, state, keyPool, pool, keyDir, pathFilter, structure []byte) error {
	if err := h.writeTo(w); err != nil {
		return err
	}
	if _, err := w.Write(state); err != nil {
		return err
	}
	if _, err := w.Write(keyPool); err != nil {
		return err
	}
	if _, err := w.Write(pool); err != nil {
		return err
	}
	if _, err := w.Write(keyDir); err != nil {
		return err
	}
	if _, err := w.Write(pathFilter); err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(structure)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(structure)
	return err
}

// deflateBlock compresses one structure block unconditionally, so every block
// in a region shares the codec its header declares.
func deflateBlock(plain []byte) ([]byte, error) {
	var out bytes.Buffer
	zw, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		return nil, errInvalidf("deflate structure block: %v", err)
	}
	if _, err := zw.Write(plain); err != nil {
		return nil, errInvalidf("deflate structure block: %v", err)
	}
	if err := zw.Close(); err != nil {
		return nil, errInvalidf("deflate structure block: %v", err)
	}
	return out.Bytes(), nil
}

// lz4Compressors recycles the high-compression searcher, whose match tables
// are large enough that allocating one per 64 KiB block would rival the work
// of compressing it.
var lz4Compressors = sync.Pool{New: func() any { return &lz4.CompressorHC{Level: lz4.Level9} }}

// lz4Block compresses one structure block, or returns the plain bytes when
// compression would not shrink them. The caller's length table is the signal:
// a stored length equal to the plain length means verbatim (specification §8A,
// the verbatim block), which is what keeps LZ4's worst-case expansion out of
// the archive without a flag bit.
func lz4Block(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return plain, nil
	}
	c := lz4Compressors.Get().(*lz4.CompressorHC)
	defer lz4Compressors.Put(c)
	dst := make([]byte, lz4.CompressBlockBound(len(plain)))
	n, err := c.CompressBlock(plain, dst)
	if err != nil {
		return nil, errInvalidf("lz4 structure block: %v", err)
	}
	if n == 0 || n >= len(plain) {
		return plain, nil
	}
	return dst[:n], nil
}

// structWriter serializes value trees against a fixed pool, tallying the string
// values it writes inline.
type structWriter struct {
	// values resolves string values at width; keys resolves object keys at
	// keyWidth — two pools, two index spaces (specification §7, §8.6).
	values   *stringPool
	keys     *stringPool
	width    int
	keyWidth int
	layout   Layout
	maxDepth int

	inlineRefs       int
	inlineBytes      int
	offsetTableBytes int
}

// writeValue writes one framed value: a LEB128 payload length followed by the
// tagged payload when LayoutLenPrefix is set, otherwise the payload alone.
func (sw *structWriter) writeValue(w *bytes.Buffer, v any) error {
	return sw.writeValueAt(w, v, 0)
}

func (sw *structWriter) writeValueAt(w *bytes.Buffer, v any, depth int) error {
	if sw.layout&LayoutLenPrefix == 0 {
		return sw.writeTagged(w, v, depth)
	}
	// The prefix counts the payload only, so the payload has to exist before it
	// can be measured.
	var payload bytes.Buffer
	if err := sw.writeTagged(&payload, v, depth); err != nil {
		return err
	}
	writeUvarint(w, uint64(payload.Len()))
	_, err := w.Write(payload.Bytes())
	return err
}

func (sw *structWriter) writeTagged(w *bytes.Buffer, v any, depth int) error {
	switch x := v.(type) {
	case nil:
		return w.WriteByte(tagNull)
	case bool:
		if x {
			return w.WriteByte(tagTrue)
		}
		return w.WriteByte(tagFalse)
	case json.Number:
		return sw.writeNumber(w, x.String())
	case float64:
		// Reachable when a caller hands us a tree parsed without UseNumber.
		return sw.writeNumber(w, strconv.FormatFloat(x, 'g', -1, 64))
	case int:
		return sw.writeNumber(w, strconv.Itoa(x))
	case int64:
		return sw.writeNumber(w, strconv.FormatInt(x, 10))
	case string:
		return sw.writeString(w, x)
	case []any:
		return sw.writeArray(w, x, depth)
	case map[string]any:
		return sw.writeObject(w, x, depth)
	default:
		return errInvalidf("unsupported json type %T", v)
	}
}

func (sw *structWriter) writeNumber(w *bytes.Buffer, s string) error {
	if len(s) > MaxNumberLexeme {
		return errLimitf("number lexeme is %d bytes (max %d)", len(s), MaxNumberLexeme)
	}
	w.WriteByte(tagNumber)
	w.WriteByte(uint8(len(s)))
	_, err := w.WriteString(s)
	return err
}

func (sw *structWriter) writeString(w *bytes.Buffer, s string) error {
	if inlinable(s, sw.width) {
		w.WriteByte(tagInline0 + uint8(len(s)))
		if _, err := w.WriteString(s); err != nil {
			return err
		}
		sw.inlineRefs++
		sw.inlineBytes += 1 + len(s)
		return nil
	}
	idx, ok := sw.values.lookup(s)
	if !ok {
		return errInvalidf("string %q missing from the value pool", s)
	}
	w.WriteByte(tagString)
	return writeIndex(w, idx, sw.width)
}

func (sw *structWriter) writeArray(w *bytes.Buffer, arr []any, depth int) error {
	if depth >= sw.maxDepth {
		return errLimitf("nesting deeper than %d", sw.maxDepth)
	}
	// Children go to a scratch buffer first: whether the container earns an
	// offset table, and how wide its entries need to be, both depend on how
	// many bytes the children turn out to occupy.
	var body bytes.Buffer
	offsets := make([]int, len(arr))
	for i, el := range arr {
		offsets[i] = body.Len()
		if err := sw.writeValueAt(&body, el, depth+1); err != nil {
			return err
		}
	}
	return sw.writeContainer(w, tagArray, len(arr), offsets, body.Bytes())
}

func (sw *structWriter) writeObject(w *bytes.Buffer, obj map[string]any, depth int) error {
	if depth >= sw.maxDepth {
		return errLimitf("nesting deeper than %d", sw.maxDepth)
	}

	// Members are emitted in ascending key-index order so readers can binary
	// search on integers alone (specification §8.5).
	type member struct {
		idx int
		val any
	}
	members := make([]member, 0, len(obj))
	for k, val := range obj {
		idx, ok := sw.keys.lookup(k)
		if !ok {
			return errInvalidf("object key %q missing from the key pool", k)
		}
		members = append(members, member{idx: idx, val: val})
	}
	slices.SortFunc(members, func(a, b member) int { return a.idx - b.idx })

	var body bytes.Buffer
	offsets := make([]int, len(members))
	for i, m := range members {
		// An object offset addresses the key, not the value.
		offsets[i] = body.Len()
		if err := writeIndex(&body, m.idx, sw.keyWidth); err != nil {
			return err
		}
		if err := sw.writeValueAt(&body, m.val, depth+1); err != nil {
			return err
		}
	}
	return sw.writeContainer(w, tagObject, len(members), offsets, body.Bytes())
}

// writeContainer emits the container header, its offset table if it earns one,
// and the already-serialized children.
func (sw *structWriter) writeContainer(w *bytes.Buffer, tag byte, count int, offsets []int, body []byte) error {
	w.WriteByte(tag)
	writeUint32(w, uint32(count))

	width := sw.offsetWidth(count, len(body))
	if sw.layout&LayoutOffsets != 0 {
		// The table width is stated per container, so a reader knows what to
		// step over without recomputing the encoder's decision (specification
		// §8.3). The descriptor only exists when the layout asked for tables at
		// all — otherwise there is no decision to record.
		w.WriteByte(uint8(width))
	}
	if width != offsetsNone {
		for _, off := range offsets {
			if err := writeIndex(w, off, width); err != nil {
				return err
			}
		}
		sw.offsetTableBytes += count * width
	}
	_, err := w.Write(body)
	return err
}

// offsetWidth decides whether a container carries an offset table, and how wide
// its entries are (specification §8.4).
//
// The payload condition is there because the table replaces a linear skip
// whose cost is what the children occupy: indexing eight three-element
// coordinate arrays costs more bytes than the scan it saves. Entry width then
// follows the span the offsets must reach.
func (sw *structWriter) offsetWidth(count, span int) int {
	if sw.layout&LayoutOffsets == 0 || count < MinOffsetChildren {
		return offsetsNone
	}
	if span < MinOffsetPayload {
		return offsetsNone
	}
	return offsetWidthFor(span)
}

func writeUint32(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

func writeIndex(w *bytes.Buffer, idx, width int) error {
	if idx < 0 {
		return errInvalidf("negative pool index %d", idx)
	}
	switch width {
	case 1:
		if idx > math.MaxUint8 {
			return errLimitf("pool index %d exceeds 1-byte width", idx)
		}
		return w.WriteByte(uint8(idx))
	case 2:
		if idx > math.MaxUint16 {
			return errLimitf("pool index %d exceeds 2-byte width", idx)
		}
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(idx))
		_, err := w.Write(b[:])
		return err
	case 4:
		if uint64(idx) > math.MaxUint32 {
			return errLimitf("pool index %d exceeds 4-byte width", idx)
		}
		writeUint32(w, uint32(idx))
		return nil
	default:
		return errUnsupportedf("index width %d", width)
	}
}
