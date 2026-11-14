package jssc

import (
	"bytes"
	"encoding/binary"
)

// This file holds the primitives for moving over a plain structure block
// without materializing values: LEB128 framing, tag location, subtree skipping,
// and container headers.

func writeUvarint(w *bytes.Buffer, v uint64) {
	var b [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(b[:], v)
	w.Write(b[:n])
}

// readUvarintBytes is the plain-slice form, for the pool section, which is
// not blocked and is addressed as an ordinary buffer.
func readUvarintBytes(buf []byte, pos int) (uint64, int, error) {
	if pos < 0 || pos >= len(buf) {
		return 0, 0, errTruncatedf("uvarint at %d", pos)
	}
	v, n := binary.Uvarint(buf[pos:])
	if n <= 0 {
		return 0, 0, errCorruptf("invalid uvarint at %d", pos)
	}
	return v, n, nil
}

// readUvarint reads a LEB128 value at pos, returning it and its encoded width.
func readUvarint(sb *structBlocks, pos int) (uint64, int, error) {
	raw, err := sb.upTo(pos, sb.len())
	if err != nil {
		return 0, 0, err
	}
	v, n := binary.Uvarint(raw)
	if n <= 0 {
		return 0, 0, errCorruptf("invalid uvarint at %d", pos)
	}
	return v, n, nil
}

// valueTagPos returns the position of the tag byte of the value that starts at
// pos, stepping over the length prefix when the layout has one.
func valueTagPos(sb *structBlocks, pos int, lenPrefix bool) (int, error) {
	if !lenPrefix {
		if pos < 0 || pos >= sb.len() {
			return 0, errTruncatedf("value at %d", pos)
		}
		return pos, nil
	}
	_, n, err := readUvarint(sb, pos)
	if err != nil {
		return 0, err
	}
	tagPos := pos + n
	if tagPos >= sb.len() {
		return 0, errTruncatedf("value tag at %d", tagPos)
	}
	return tagPos, nil
}

// valueEndPos returns the offset just past the value starting at pos. With
// length prefixes this is arithmetic; without them it needs a recursive walk.
func (a *Archive) valueEndPos(pos int) (int, error) {
	if a.lenPrefix {
		payloadLen, n, err := readUvarint(a.structure, pos)
		if err != nil {
			return 0, err
		}
		end := pos + n + int(payloadLen)
		if end < pos || end > a.structure.len() {
			return 0, errTruncatedf("value at %d claims %d payload bytes", pos, payloadLen)
		}
		return end, nil
	}
	return skipTagged(a.structure, pos, a.indexWidth, a.keyWidth, a.layoutOffsets, a.limits.maxDepth())
}

// skipTagged walks past a bare tagged value at pos (no length prefix).
func skipTagged(sb *structBlocks, pos, indexWidth, keyWidth int, layoutOffsets bool, depth int) (int, error) {
	if depth <= 0 {
		return 0, errLimitf("nesting deeper than the decode limit")
	}
	tag, err := sb.byteAt(pos)
	if err != nil {
		return 0, err
	}
	pos++

	switch tag {
	case tagNull, tagFalse, tagTrue:
		return pos, nil
	case tagNumber:
		nb, err := sb.byteAt(pos)
		if err != nil {
			return 0, errTruncatedf("number length")
		}
		n := int(nb)
		pos++
		if pos+n > sb.len() {
			return 0, errTruncatedf("number payload")
		}
		return pos + n, nil
	case tagString:
		_, next, err := readIndex(sb, pos, indexWidth)
		return next, err
	case tagArray, tagObject:
		count, next, err := readContainerCount(sb, pos, sb.len(), layoutOffsets)
		if err != nil {
			return 0, err
		}
		pos = next
		for range count {
			if tag == tagObject {
				_, valPos, err := readIndex(sb, pos, keyWidth)
				if err != nil {
					return 0, err
				}
				pos = valPos
			}
			pos, err = skipTagged(sb, pos, indexWidth, keyWidth, layoutOffsets, depth-1)
			if err != nil {
				return 0, err
			}
		}
		return pos, nil
	default:
		if tag >= tagInline0 && tag <= tagInlineMax {
			n := int(tag - tagInline0)
			if pos+n > sb.len() {
				return 0, errTruncatedf("inline string")
			}
			return pos + n, nil
		}
		return 0, errCorruptf("unknown structure tag 0x%02x at %d", tag, pos-1)
	}
}

// readContainerCount reads a container's element count at pos (just past the
// tag) and returns the offset of its first child, skipping any offset table.
func readContainerCount(sb *structBlocks, pos, limit int, layoutOffsets bool) (count, childStart int, err error) {
	if pos+4 > limit {
		return 0, 0, errTruncatedf("container count at %d", pos)
	}
	n, err := sb.u32(pos)
	if err != nil {
		return 0, 0, err
	}
	pos += 4

	width, pos, err := readOffsetWidth(sb, pos, layoutOffsets)
	if err != nil {
		return 0, 0, err
	}
	if width != offsetsNone {
		// Validate against the buffer before trusting a width·n advance.
		if int64(pos)+int64(width)*int64(n) > int64(limit) {
			return 0, 0, errTruncatedf("offset table for %d children", n)
		}
		pos += width * int(n)
	} else if int64(n) > int64(limit-pos) {
		// Each child costs at least one byte, so a count larger than the
		// remaining buffer is corrupt however it is framed.
		return 0, 0, errCorruptf("container declares %d children, %d bytes remain", n, limit-pos)
	}
	return int(n), pos, nil
}

// readOffsetWidth reads a container's offset-width descriptor and returns the
// position just past it. The descriptor exists whenever the layout carries
// offset tables, because the encoder's decision depends on the payload size —
// something a reader cannot recompute without doing the walk the table exists
// to avoid (specification §8.3, §8.4).
func readOffsetWidth(sb *structBlocks, pos int, layoutOffsets bool) (width, next int, err error) {
	if !layoutOffsets {
		return offsetsNone, pos, nil
	}
	w, err := sb.byteAt(pos)
	if err != nil {
		return 0, 0, errTruncatedf("offset table descriptor at %d", pos)
	}
	width = int(w)
	pos++
	if !validOffsetWidth(width) {
		return 0, 0, errCorruptf("offset table width %d (want 0, 1, 2, or 4)", width)
	}
	return width, pos, nil
}

func readIndex(sb *structBlocks, pos, width int) (idx, next int, err error) {
	if width != 1 && width != 2 && width != 4 {
		return 0, 0, errUnsupportedf("index width %d", width)
	}
	b, err := sb.at(pos, width)
	if err != nil {
		return 0, 0, errTruncatedf("pool index at %d", pos)
	}
	switch width {
	case 1:
		return int(b[0]), pos + 1, nil
	case 2:
		return int(binary.LittleEndian.Uint16(b)), pos + 2, nil
	default:
		return int(binary.LittleEndian.Uint32(b)), pos + 4, nil
	}
}

// containerMeta is a parsed array or object header.
type containerMeta struct {
	tagPos     int
	isObject   bool
	count      int
	elemsStart int // first child byte: value_0, or the first key index
	end        int // just past the whole container value

	// tablePos and tableWidth locate the container's on-disk offset table, when
	// it has one. The entries are deliberately *not* decoded here.
	//
	// Almost nothing wants the whole table. A sequential walk wants none of it
	// and steps through children with valueEndPos; a $[n] lookup wants exactly
	// one entry; a field lookup binary searches and wants log(m). Decoding all
	// m up front was measured, on a GeoJSON corpus of large coordinate arrays,
	// as 94% of everything a full walk allocated — two int slices per container,
	// rebuilt on every visit, for a table the walk never read.
	tablePos   int
	tableWidth int
}

// hasTable reports whether the container carries an on-disk offset table.
func (m containerMeta) hasTable() bool { return m.tableWidth != offsetsNone }

func (a *Archive) containerMetaAt(pos int) (containerMeta, error) {
	var m containerMeta
	end, err := a.valueEndPos(pos)
	if err != nil {
		return m, err
	}
	m.end = end

	tagPos, err := valueTagPos(a.structure, pos, a.lenPrefix)
	if err != nil {
		return m, err
	}
	m.tagPos = tagPos

	tag, err := a.structure.byteAt(tagPos)
	if err != nil {
		return m, err
	}
	switch tag {
	case tagArray:
		m.isObject = false
	case tagObject:
		m.isObject = true
	default:
		return m, errCorruptf("value at %d is not a container", pos)
	}

	p := tagPos + 1
	count, err := a.structure.u32(p)
	if err != nil {
		return m, errTruncatedf("container count at %d", p)
	}
	p += 4

	// Every child costs at least one byte inside the container, so a count that
	// cannot fit is corrupt. Checking here keeps a bad count from sizing an
	// allocation anywhere downstream.
	if int64(count) > int64(end-p) {
		return m, errCorruptf("container declares %d children, %d bytes remain", count, end-p)
	}
	m.count = int(count)

	width, p, err := readOffsetWidth(a.structure, p, a.layoutOffsets)
	if err != nil {
		return m, err
	}
	if width == offsetsNone {
		m.elemsStart = p
		return m, nil
	}

	// Validate the table's extent now, so every later per-entry read is a plain
	// bounds-checked slice of a region already known to be inside the buffer.
	tabEnd := int64(p) + int64(width)*int64(m.count)
	if tabEnd > int64(a.structure.len()) {
		return m, errTruncatedf("offset table for %d children", m.count)
	}
	m.tablePos = p
	m.tableWidth = width
	m.elemsStart = int(tabEnd)
	return m, nil
}

// memberOff reads entry i of a container's offset table: where member i begins,
// which is its key index for an object and its value for an array — exactly what
// the table stores.
//
// Each entry is validated on the way out rather than in bulk at open time, which
// is the same guarantee at a fraction of the work.
func (a *Archive) memberOff(m containerMeta, i int) (int, error) {
	if i < 0 || i >= m.count {
		return 0, errCorruptf("child %d out of range [0,%d)", i, m.count)
	}
	rel, _, err := readIndex(a.structure, m.tablePos+i*m.tableWidth, m.tableWidth)
	if err != nil {
		return 0, err
	}
	off := m.elemsStart + rel
	if off < m.elemsStart || off >= m.end {
		return 0, errCorruptf("child offset %d escapes its container", i)
	}
	return off, nil
}

// childIndex gives random access to one container's members without decoding
// the whole offset table.
//
// With a table it reads entries on demand and allocates nothing. Without one the
// offsets can only come from a linear scan, so it uses the Archive's cache,
// built once and kept for the life of the archive.
type childIndex struct {
	a *Archive
	m containerMeta
	// scanned is set only when the offsets came from a linear scan.
	scanned []int
}

// childIndexFor prepares random access to the container at pos.
func (a *Archive) childIndexFor(pos int, m containerMeta) (childIndex, error) {
	if m.hasTable() {
		return childIndex{a: a, m: m}, nil
	}
	offs, err := a.ensureKeyOff(pos)
	if err != nil {
		return childIndex{}, err
	}
	return childIndex{a: a, m: m, scanned: offs}, nil
}

// at is where member i begins: its key index for an object, its value for an
// array.
func (ci childIndex) at(i int) (int, error) {
	if ci.scanned != nil {
		if i < 0 || i >= len(ci.scanned) {
			return 0, errCorruptf("child %d out of range [0,%d)", i, len(ci.scanned))
		}
		return ci.scanned[i], nil
	}
	return ci.a.memberOff(ci.m, i)
}

// ensureChildOff returns the value start of every child of the container at
// pos, caching the result for the life of the Archive.
//
// This is for callers that genuinely visit every child. A caller that wants one
// child should use childIndexFor instead, which reads a single table entry.
func (a *Archive) ensureChildOff(pos int) ([]int, error) {
	if offs, ok := a.childOff[pos]; ok {
		return offs, nil
	}
	m, err := a.containerMetaAt(pos)
	if err != nil {
		return nil, err
	}

	valueOffs := make([]int, m.count)
	keyOffs := make([]int, m.count)

	if m.hasTable() {
		for i := range m.count {
			k, err := a.memberOff(m, i)
			if err != nil {
				return nil, err
			}
			keyOffs[i] = k
			if !m.isObject {
				valueOffs[i] = k
				continue
			}
			_, v, err := readIndex(a.structure, k, a.keyWidth)
			if err != nil {
				return nil, err
			}
			valueOffs[i] = v
		}
		a.cacheChildOff(pos, valueOffs, keyOffs)
		return valueOffs, nil
	}

	cur := m.elemsStart
	for i := range m.count {
		keyOffs[i] = cur
		if m.isObject {
			_, valStart, err := readIndex(a.structure, cur, a.keyWidth)
			if err != nil {
				return nil, err
			}
			cur = valStart
		}
		valueOffs[i] = cur
		if cur, err = a.skipValue(cur); err != nil {
			return nil, err
		}
	}
	if !m.isObject {
		keyOffs = valueOffs
	}
	a.cacheChildOff(pos, valueOffs, keyOffs)
	return valueOffs, nil
}

// ensureKeyOff returns the key-index start of every member of the object at pos.
func (a *Archive) ensureKeyOff(pos int) ([]int, error) {
	if offs, ok := a.childKeyOff[pos]; ok {
		return offs, nil
	}
	if _, err := a.ensureChildOff(pos); err != nil {
		return nil, err
	}
	offs, ok := a.childKeyOff[pos]
	if !ok {
		return nil, errCorruptf("value at %d has no key table", pos)
	}
	return offs, nil
}

func (a *Archive) cacheChildOff(pos int, valueOffs, keyOffs []int) {
	if a.childOff == nil {
		a.childOff = make(map[int][]int)
		a.childKeyOff = make(map[int][]int)
	}
	a.childOff[pos] = valueOffs
	if keyOffs != nil {
		a.childKeyOff[pos] = keyOffs
	}
}
