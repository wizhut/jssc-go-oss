package jssc

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"math"
	"os"
)

// EncodeFile encodes the file at path into an archive written to w.
//
// A single JSON document is always read in full. Line-oriented input larger
// than Options.MaxDirectBytes takes the two-pass streaming path instead: pass
// one gathers the string pool a document at a time, pass two writes each
// document's structure to a temporary file. Peak memory is then the pool plus
// one parse tree, not the whole corpus.
func EncodeFile(path string, w io.Writer, opt Options) (*Stats, error) {
	if err := opt.validate(); err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !opt.Format.LineOriented() || useDirectEncode(fi.Size(), opt) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return EncodeWith(raw, w, opt)
	}
	return encodeLinesStreaming(path, w, opt, fi.Size())
}

// EncodeFrom encodes everything readable from r into an archive written to w.
//
// When r is a file of a line-oriented format above the direct-size threshold,
// the streaming encoder reads it twice by name. Otherwise the input is
// buffered: in memory for JSON, or spilled to a temporary file so line-oriented
// input can still be encoded in two passes.
func EncodeFrom(r io.Reader, w io.Writer, opt Options) (*Stats, error) {
	if err := opt.validate(); err != nil {
		return nil, err
	}
	if !opt.Format.LineOriented() {
		raw, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return EncodeWith(raw, w, opt)
	}

	if f, ok := r.(*os.File); ok && f.Name() != "" {
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			if useDirectEncode(fi.Size(), opt) {
				raw, err := io.ReadAll(f)
				if err != nil {
					return nil, err
				}
				return EncodeWith(raw, w, opt)
			}
			return encodeLinesStreaming(f.Name(), w, opt, fi.Size())
		}
	}

	tmp, err := os.CreateTemp("", "jssc-in-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if useDirectEncode(n, opt) {
		raw, err := os.ReadFile(tmpPath)
		if err != nil {
			return nil, err
		}
		return EncodeWith(raw, w, opt)
	}
	return encodeLinesStreaming(tmpPath, w, opt, n)
}

func useDirectEncode(size int64, opt Options) bool {
	if opt.MaxDirectBytes < 0 {
		return false
	}
	limit := opt.MaxDirectBytes
	if limit == 0 {
		limit = DefaultMaxDirectBytes
	}
	return size <= limit
}

// encodeLinesStreaming is the two-pass JSONL/NDJSON encoder.
func encodeLinesStreaming(path string, w io.Writer, opt Options, rawSize int64) (*Stats, error) {
	// Pass 1: every distinct string, one document at a time.
	sets := newStringSets()
	docCount := 0
	var rawBytes int64
	err := eachLineDocInFile(path, opt.MaxDocs, func(line []byte, doc any) error {
		sets.gather(doc)
		docCount++
		rawBytes += int64(len(line)) + 1 // the newline the document was written with
		return nil
	})
	if err != nil {
		return nil, err
	}
	if docCount == 0 {
		return nil, errInvalidf("empty input")
	}
	if rawBytes == 0 {
		rawBytes = rawSize
	}

	layout := opt.layout()
	h := header{
		version:    opt.writeVersion(),
		layout:     layout,
		contentFmt: opt.Format,
		docCount:   uint32(docCount),
		groupShift: opt.poolGroupShift(),
	}
	if h.version < 5 {
		// JSS4 has no group field and frames one entry per group.
		h.groupShift = 0
	}

	// The plan follows the version, exactly as the in-memory encoder's does —
	// the two paths must produce identical bytes.
	var plan poolPlan
	if h.splitPools() {
		plan = buildPool(sets)
	} else {
		plan = legacyBuildPool(sets)
	}
	if plan.values.len() > MaxPoolCount || plan.keys.len() > MaxPoolCount {
		return nil, errLimitf("pool holds %d strings (max %d)",
			max(plan.values.len(), plan.keys.len()), MaxPoolCount)
	}
	h.indexWidth = plan.width
	h.poolCount = uint32(plan.values.len())
	if h.splitPools() {
		h.keyCount = uint32(plan.keys.len())
	}

	// The key directory is built from the pool plan, which pass 1 already
	// produced, so the streaming path pays nothing extra for it.
	var keyDir []byte
	keyDirEntries := 0
	if h.keyDir() {
		keyDir = emptyKeyDirectory
		if !opt.OmitKeyDirectory {
			var enc []byte
			var ok bool
			if h.splitPools() {
				enc, ok = buildSplitKeyDirectory(plan.keys, plan.keyWidth)
			} else {
				enc, ok = buildKeyDirectory(plan.keys, plan.keyIdx, plan.width, h.completeKeyDirectory())
			}
			if ok {
				keyDir, keyDirEntries = enc, plan.keys.len()
				if !h.splitPools() {
					keyDirEntries = len(plan.keyIdx)
				}
			}
		}
	}

	c, keySec, pool, err := buildPoolSections(opt.Algorithm, plan, h, opt.concurrency())
	if err != nil {
		return nil, err
	}

	// Pass 2: each document's structure, straight to a scratch file.
	sw := &structWriter{
		values:          plan.values,
		keys:            plan.keys,
		width:           plan.width,
		keyWidth:        plan.keyWidth,
		layout:          layout,
		maxDepth:        opt.maxDepth(),
		adaptiveOffsets: h.adaptiveOffsets(),
	}

	body, err := os.CreateTemp("", "jssc-body-*")
	if err != nil {
		return nil, err
	}
	bodyPath := body.Name()
	defer os.Remove(bodyPath)

	lengths := make([]uint32, 0, docCount)
	var bodyBytes int64
	buf := new(bytes.Buffer)
	rootBase := 4 * int64(docCount)
	err = eachLineDocInFile(path, opt.MaxDocs, func(_ []byte, doc any) error {
		// A root offset is a uint32 (specification §4.2), so the plain structure
		// cannot place a document past 4 GiB. concatLengthsAndBody enforces that
		// where the table is written; checking here as well stops at the first
		// such document instead of after the whole body is on disk.
		if h.rootOffsets() && rootBase+bodyBytes > math.MaxUint32 {
			return errLimitf("document %d starts past uint32 bytes", len(lengths))
		}
		buf.Reset()
		if err := sw.writeValue(buf, doc); err != nil {
			return err
		}
		if buf.Len() > math.MaxUint32 {
			return errLimitf("document %d root exceeds uint32 bytes", len(lengths))
		}
		lengths = append(lengths, uint32(buf.Len()))
		if _, err := body.Write(buf.Bytes()); err != nil {
			return err
		}
		bodyBytes += int64(buf.Len())
		return nil
	})
	if err != nil {
		body.Close()
		return nil, err
	}
	if err := body.Close(); err != nil {
		return nil, err
	}

	plainLen := int64(len(lengths))*4 + bodyBytes
	if opt.Structure == StructRaw && plainLen > MaxStoredStructure {
		return nil, errLimitf("structure is %d bytes, past the uint32 struct_len limit; use deflate or MaxDocs", plainLen)
	}

	structPath, structCodec, storedLen, structBlocks, err := packStructureFile(
		bodyPath, lengths, opt.Structure, h, opt.structBlockBytes(), opt.concurrency())
	if err != nil {
		return nil, err
	}
	defer os.Remove(structPath)
	if storedLen > MaxStoredStructure {
		return nil, errLimitf("stored structure is %d bytes (struct_len max %d)", storedLen, uint64(MaxStoredStructure))
	}

	var stateBuf bytes.Buffer
	if err := c.WriteState(&stateBuf); err != nil {
		return nil, errInvalidf("write codec state: %v", err)
	}

	h.algo = c.ID()
	h.structCodec = structCodec

	// Decided on exactly the terms the in-memory encoder uses, against the same
	// two sizes, so the two paths still produce identical bytes.
	pathFilter := pathFilterSection(h, opt, plan.paths, pool.payload+int(storedLen))
	if len(pathFilter) > 0 {
		h.layout |= LayoutPathFilter
	}

	if err := h.writeTo(w); err != nil {
		return nil, err
	}
	if _, err := w.Write(stateBuf.Bytes()); err != nil {
		return nil, err
	}
	if _, err := w.Write(keySec); err != nil {
		return nil, err
	}
	if _, err := w.Write(pool.section); err != nil {
		return nil, err
	}
	if _, err := w.Write(keyDir); err != nil {
		return nil, err
	}
	if _, err := w.Write(pathFilter); err != nil {
		return nil, err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(storedLen))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return nil, err
	}
	sf, err := os.Open(structPath)
	if err != nil {
		return nil, err
	}
	copied, err := io.Copy(w, sf)
	sf.Close()
	if err != nil {
		return nil, err
	}
	if copied != int64(storedLen) {
		return nil, errCorruptf("structure write: copied %d bytes, expected %d", copied, storedLen)
	}

	st := &Stats{
		Algorithm:        c.Name(),
		StructCodec:      structCodec,
		StructLayout:     layout,
		ContentFormat:    opt.Format,
		Version:          h.version,
		DocCount:         docCount,
		IndexWidth:       plan.width,
		KeyWidth:         plan.keyWidth,
		UniqueStrings:    plan.values.len(),
		DistinctKeys:     plan.keys.len(),
		KeyPoolBytes:     len(keySec),
		RawBytes:         int(rawBytes),
		PoolBytes:        pool.payload,
		PoolRawBytes:     pool.rawBytes,
		PoolFraming:      len(pool.section) + len(keySec) - pool.payload,
		PoolGroupSize:    h.groupSize(),
		PoolGroups:       groupCount(plan.values.len(), h.groupSize()),
		OffsetTables:     sw.offsetTableBytes,
		StructBlocks:     structBlocks,
		StructBytes:      int(storedLen),
		StructPlain:      int(plainLen),
		StateBytes:       stateBuf.Len(),
		HeaderBytes:      h.size(),
		InlineStrings:    plan.inlineDistinct,
		InlineRefs:       sw.inlineRefs,
		InlineBytes:      sw.inlineBytes,
		KeyDirEntries:    keyDirEntries,
		KeyDirBytes:      len(keyDir),
		PathFilterBytes:  len(pathFilter),
		PathFilterPaths:  pathFilterPaths(pathFilter, plan.paths),
		PoolRawGroups:    pool.rawGroups,
		StructBlockBytes: opt.structBlockBytes(),
	}
	st.TotalBytes = st.HeaderBytes + st.StateBytes + len(keySec) + len(pool.section) + len(keyDir) +
		len(pathFilter) + 4 + st.StructBytes
	return st, nil
}

// packStructureFile assembles the plain structure — the root length table
// followed by the body — and applies the structure codec, all on disk. It
// returns a temporary path holding the stored bytes, the codec actually used,
// and the stored size. The caller owns and must remove the returned path;
// bodyPath is left alone.
func packStructureFile(bodyPath string, lengths []uint32, sc StructCodec, h header, blockTarget, blockWorkers int) (path string, used StructCodec, storedLen int64, blocks int, err error) {
	plainPath, plainLen, err := concatLengthsAndBody(bodyPath, lengths, h.rootOffsets())
	if err != nil {
		return "", 0, 0, 0, err
	}

	// A blocked structure has to be assembled the same way the in-memory
	// encoder assembles it, or the two paths stop producing the same archive —
	// which TestStreamingMatchesInMemory exists to prevent. The plain structure
	// is already on disk and is read back once.
	if h.blockedStructure() {
		plain, rerr := os.ReadFile(plainPath)
		os.Remove(plainPath)
		if rerr != nil {
			return "", 0, 0, 0, rerr
		}
		starts := rootStarts(lengths)
		plan := planStructBlocks(len(plain), blockTarget, 4*len(starts), starts, sc != StructRaw)
		stored, codec, perr := packStructBlocks(plain, plan, sc, blockWorkers)
		if perr != nil {
			return "", 0, 0, 0, perr
		}
		out, werr := os.CreateTemp("", "jssc-struct-*")
		if werr != nil {
			return "", 0, 0, 0, werr
		}
		if _, werr = out.Write(stored); werr != nil {
			out.Close()
			os.Remove(out.Name())
			return "", 0, 0, 0, werr
		}
		if werr = out.Close(); werr != nil {
			os.Remove(out.Name())
			return "", 0, 0, 0, werr
		}
		return out.Name(), codec, int64(len(stored)), len(plan.cuts) - 1, nil
	}

	if sc == StructRaw {
		return plainPath, StructRaw, plainLen, 1, nil
	}

	deflatePath, deflateLen, err := deflateFile(plainPath)
	if err != nil {
		os.Remove(plainPath)
		return "", 0, 0, 0, err
	}
	// Same fallback rule as the in-memory path: the header must describe the
	// bytes that follow, so a deflate that did not shrink is discarded.
	if deflateLen >= plainLen {
		os.Remove(deflatePath)
		return plainPath, StructRaw, plainLen, 1, nil
	}
	os.Remove(plainPath)
	return deflatePath, StructDeflate, deflateLen, 1, nil
}

// rootStarts turns per-document lengths into the plain offsets a block boundary
// may fall on.
func rootStarts(lengths []uint32) []int {
	starts := make([]int, len(lengths))
	off := 4 * len(lengths)
	for i, n := range lengths {
		starts[i] = off
		off += int(n)
	}
	return starts
}

func concatLengthsAndBody(bodyPath string, lengths []uint32, asOffsets bool) (path string, size int64, err error) {
	plain, err := os.CreateTemp("", "jssc-plain-*")
	if err != nil {
		return "", 0, err
	}
	plainPath := plain.Name()
	fail := func(err error) (string, int64, error) {
		plain.Close()
		os.Remove(plainPath)
		return "", 0, err
	}

	table := make([]byte, 0, 4*len(lengths))
	if asOffsets {
		// The same bound the in-memory encoder applies; without it an offset
		// past 4 GiB would wrap and the archive would open the wrong bytes.
		for i, off := range rootStarts(lengths) {
			if off > math.MaxUint32 {
				return fail(errLimitf("document %d starts past uint32 bytes", i))
			}
			table = binary.LittleEndian.AppendUint32(table, uint32(off))
		}
	} else {
		for _, n := range lengths {
			table = binary.LittleEndian.AppendUint32(table, n)
		}
	}
	if _, err := plain.Write(table); err != nil {
		return fail(err)
	}
	body, err := os.Open(bodyPath)
	if err != nil {
		return fail(err)
	}
	n, err := io.Copy(plain, body)
	body.Close()
	if err != nil {
		return fail(err)
	}
	if err := plain.Close(); err != nil {
		os.Remove(plainPath)
		return "", 0, err
	}
	return plainPath, int64(len(table)) + n, nil
}

func deflateFile(srcPath string) (path string, size int64, err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()

	dst, err := os.CreateTemp("", "jssc-deflate-*")
	if err != nil {
		return "", 0, err
	}
	dstPath := dst.Name()
	fail := func(err error) (string, int64, error) {
		dst.Close()
		os.Remove(dstPath)
		return "", 0, errInvalidf("deflate structure: %v", err)
	}

	zw, err := flate.NewWriter(dst, flate.BestCompression)
	if err != nil {
		return fail(err)
	}
	if _, err := io.Copy(zw, src); err != nil {
		zw.Close()
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	fi, err := dst.Stat()
	if err != nil {
		return fail(err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(dstPath)
		return "", 0, err
	}
	return dstPath, fi.Size(), nil
}

func eachLineDocInFile(path string, maxDocs int, fn func(line []byte, doc any) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return forEachLineDoc(f, maxDocs, DefaultMaxLineBytes, fn)
}
