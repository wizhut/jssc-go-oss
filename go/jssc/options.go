package jssc

import "runtime"

// DefaultMaxDirectBytes is the default size threshold for in-memory encoding of
// JSONL/NDJSON. Larger inputs use the two-pass streaming encoder, which keeps
// only the string pool plus one document parse tree resident at a time.
const DefaultMaxDirectBytes int64 = 64 << 20 // 64 MiB

// DefaultMaxDepth bounds container nesting during encode and decode. The format
// itself sets no depth limit; this guards against stack exhaustion on hostile
// input (specification §14, §15.2).
const DefaultMaxDepth = 1000

// Options configures encoding.
type Options struct {
	// Algorithm names the string pool codec. Empty selects the registry
	// default ("fsst").
	Algorithm string

	// Structure selects the structure block codec. The zero value is
	// StructRaw; most callers want StructLZ4, which is what Encode selects.
	Structure StructCodec

	// Layout is the bitmask of structure walk aids written into a JSS3/JSS4
	// header. Zero selects LayoutDefault. Setting LayoutLegacy forces the
	// legacy JSS2 write path, which is valid for FormatJSON only.
	Layout Layout

	// Format declares the input's textual form. The zero value, FormatJSON,
	// encodes a single document as JSS3. FormatJSONL and FormatNDJSON encode a
	// JSS4 multi-document archive over one shared string pool. This is never
	// inferred from a filename or from the bytes.
	Format ContentFormat

	// MaxDirectBytes is the largest line-oriented input EncodeFile and
	// EncodeFrom will buffer entirely in memory. Zero selects
	// DefaultMaxDirectBytes; a negative value forces the streaming path.
	MaxDirectBytes int64

	// MaxDocs, when positive, encodes only the first MaxDocs non-empty lines of
	// a line-oriented input. Ignored for FormatJSON.
	MaxDocs int

	// MaxDepth bounds container nesting. Zero selects DefaultMaxDepth.
	MaxDepth int

	// PoolGroupShift sets the pool group size to 1<<PoolGroupShift entries.
	// Zero selects DefaultPoolGroupShift, which is now also 1 entry per group.
	//
	// Grouping exists for codecs that compress across entry boundaries, and
	// there are none left: every registered codec is a small-string codec that
	// encodes an entry on its own. Setting this above zero still produces a
	// valid archive, but it gains no compression and makes a reader walk a
	// group's framing to reach one string.
	PoolGroupShift uint8

	// PerEntryPool forces a pool group size of 1. It is now the default, and is
	// kept so a caller can state the requirement rather than rely on it.
	PerEntryPool bool

	// Legacy4 writes the older JSS3/JSS4 container and pool encodings instead
	// of JSS5. Useful for producing archives an older decoder can read, and for
	// measuring what the JSS5 changes are worth.
	Legacy4 bool

	// Version pins the archive version. Zero selects LatestVersion. It exists
	// so a caller can write an archive an older decoder can read, and so the
	// benchmark can measure one version against another; Legacy4 and
	// LayoutLegacy remain the shorthands for the pre-JSS5 shapes and take
	// precedence over this field.
	//
	// A pinned version writes only what its header can declare (Appendix E):
	// JSS1 and JSS2 carry no walk aids whatever Layout says, JSS1 accepts only
	// StructRaw, and line-oriented formats need JSS4 or later.
	Version int

	// StructBlockBytes is the target plain size of one structure block in JSS7.
	// Zero selects DefaultStructBlockBytes; a negative value stores the
	// structure as a single block, which is what JSS6 and earlier did.
	//
	// A block is the unit a reader inflates to reach any document inside it, so
	// this trades archive size against the cost of one cold read — the same
	// trade PoolGroupShift makes for the pool.
	StructBlockBytes int

	// OmitKeyDirectory suppresses the JSS6 key hash directory. The section is
	// still present, with a count of zero, so the archive stays readable by any
	// JSS6 decoder; only the lookup shortcut is gone. Useful for measuring what
	// the directory is worth, and for the rare document whose keys are so
	// numerous that the table is not worth its bytes.
	OmitKeyDirectory bool

	// OmitPathFilter suppresses the JSS8 key-path filter. The section is
	// emitted only when it is small relative to what it indexes, so most
	// documents that carry one wanted it; this exists so a benchmark can price
	// the section on its own rather than infer it.
	OmitPathFilter bool

	// Concurrency bounds the goroutines the encoder uses to compress pool
	// groups. Zero selects GOMAXPROCS; 1 encodes sequentially.
	//
	// Set it to 1 in a server that is already running many encodes at once —
	// per-request parallelism there competes with itself. The archive is
	// byte-identical either way.
	Concurrency int
}

// concurrency resolves how many workers the pool encoder may use.
func (o Options) concurrency() int {
	if o.Concurrency > 0 {
		return o.Concurrency
	}
	return runtime.GOMAXPROCS(0)
}

// poolGroupShift resolves the group size exponent for this encode.
func (o Options) poolGroupShift() uint8 {
	if o.PerEntryPool {
		return 0
	}
	if o.PoolGroupShift == 0 {
		return DefaultPoolGroupShift
	}
	return min(o.PoolGroupShift, MaxPoolGroupShift)
}

func (o Options) maxDepth() int {
	if o.MaxDepth > 0 {
		return o.MaxDepth
	}
	return DefaultMaxDepth
}

// structBlockBytes resolves the structure block target for this encode.
func (o Options) structBlockBytes() int {
	switch {
	case o.StructBlockBytes < 0:
		return 0 // one block
	case o.StructBlockBytes == 0:
		return DefaultStructBlockBytes
	default:
		return max(o.StructBlockBytes, MinStructBlockBytes)
	}
}

// version resolves the archive version this encode targets.
func (o Options) version() int {
	if o.Version > 0 {
		return o.Version
	}
	return LatestVersion
}

func (o Options) layout() Layout {
	if o.Layout == 0 {
		return LayoutDefault
	}
	return o.Layout
}

// writeVersion is the archive version an encode writes. LayoutLegacy and
// Legacy4 take precedence over Version: the first writes JSS2, the second JSS3
// for a single document and JSS4 for lines.
func (o Options) writeVersion() int {
	switch {
	case o.Layout&LayoutLegacy != 0:
		return 2
	case o.Legacy4 && o.Format.LineOriented():
		return 4
	case o.Legacy4:
		return 3
	}
	return o.version()
}

// validate checks the option combination before any work is done. A request
// the target version's header has no field to declare is refused rather than
// written for a reader to misparse (Appendix E).
func (o Options) validate() error {
	if !o.Structure.valid() {
		return errUnsupportedf("structure codec %d", uint8(o.Structure))
	}
	if o.Version != 0 && (o.Version < 1 || o.Version > LatestVersion) {
		return errUnsupportedf("archive version %d (want 1 through %d)", o.Version, LatestVersion)
	}
	v := o.writeVersion()
	// The lz4 structure codec exists only in JSS8 (specification §5.2); the
	// legacy write paths predate it.
	if o.Structure == StructLZ4 && v < 8 {
		return errUnsupportedf("structure codec lz4 requires JSS8")
	}
	// JSS1 has no struct_codec byte, and a reader implies raw.
	if o.Structure != StructRaw && v < 2 {
		return errUnsupportedf("structure codec %s requires JSS2 or later: JSS1 stores the structure raw", o.Structure)
	}
	if o.Format != FormatJSON && !o.Format.LineOriented() {
		return errUnsupportedf("content format %d", uint8(o.Format))
	}
	if o.Layout&LayoutLegacy != 0 && o.Format.LineOriented() {
		return errInvalidf("LayoutLegacy cannot encode %s: multi-document archives require JSS4 walk aids", o.Format)
	}
	if reserved := o.layout() &^ (layoutStored | LayoutLegacy); reserved != 0 {
		return errUnsupportedf("reserved struct_layout bits 0x%02x", uint8(reserved))
	}
	// JSS1–JSS3 headers have no content_fmt or doc_count and their structure no
	// root table: they hold exactly one document.
	if o.Format.LineOriented() && v < 4 {
		return errInvalidf("JSS%d cannot encode %s: multi-document archives require JSS4 or later", v, o.Format)
	}
	return nil
}

// Limits bounds the work a decoder will do for an archive. The zero value
// applies DefaultMaxDepth and no size ceiling, which suits trusted input;
// callers handling untrusted archives should set MaxPlainStructure.
type Limits struct {
	// MaxDepth bounds container nesting while materializing values. Zero
	// selects DefaultMaxDepth.
	MaxDepth int

	// MaxPlainStructure bounds the inflated size of a compressed structure
	// block, in bytes. Zero means unlimited. A small stored block can inflate
	// to a very large one, so untrusted input should set this.
	MaxPlainStructure int64

	// MaxPoolCount rejects archives declaring more pool entries than this.
	// Zero means unlimited (subject to the archive fitting in memory).
	MaxPoolCount int
}

func (l Limits) maxDepth() int {
	if l.MaxDepth > 0 {
		return l.MaxDepth
	}
	return DefaultMaxDepth
}

// Stats reports what an encode or open produced.
//
// Encoding fills every field. Opening an archive fills the fields that are
// readable from the header and framing — everything except RawBytes,
// PoolRawBytes, TotalBytes, and the inline-string tallies.
type Stats struct {
	Algorithm     string        // pool codec name
	StructCodec   StructCodec   // structure codec actually used
	StructLayout  Layout        // struct_layout as stored (0 for JSS1/JSS2)
	ContentFormat ContentFormat // declared textual form
	Version       int           // archive version, 1 through 6
	DocCount      int           // top-level documents (1 unless JSS4)
	IndexWidth    int           // pool index width in bytes
	UniqueStrings int           // pool cardinality

	// KeyWidth sizes key-pool references; DistinctKeys is the key pool's
	// cardinality; KeyPoolBytes its stored section (JSS8, 5.3.0-draft).
	KeyWidth     int
	DistinctKeys int
	KeyPoolBytes int

	RawBytes     int // original text size
	PoolBytes    int // compressed pool payload, excluding framing and state
	PoolRawBytes int // pooled strings before compression
	PoolFraming  int // pool_len, stored-raw flags, group directories, length prefixes
	StructBytes  int // structure as stored, after StructCodec
	StructPlain  int // structure before StructCodec
	StateBytes   int // codec_state size
	HeaderBytes  int // fixed header size
	TotalBytes   int // whole archive

	// PoolGroupSize is how many entries share one compressed group, and
	// PoolGroups how many groups the pool was split into. A group is the unit
	// a reader decompresses to reach any entry inside it.
	PoolGroupSize int
	PoolGroups    int

	// OffsetTables is how many bytes the structure spends on child offset
	// tables. Filled on encode only.
	OffsetTables int

	// StructBlocks is how many independently inflatable blocks the structure was
	// cut into, and StructBlockBytes the target each was aimed at. One block
	// means the structure must be inflated whole, as in JSS6 and earlier.
	StructBlocks     int
	StructBlockBytes int

	// KeyDirEntries is how many keys the JSS6 key hash directory lists, and
	// KeyDirBytes what the section costs including its count. Zero entries
	// means the archive carries no directory, either because the encoder was
	// told not to build one or because two keys collided.
	KeyDirEntries int
	KeyDirBytes   int

	// PathFilterBytes is the size of the JSS8 key-path filter section, and
	// PathFilterPaths how many distinct root-to-leaf key paths it indexes. Both
	// are zero when the archive carries no filter, which is the common case for
	// documents small enough that the section would not pay for itself.
	PathFilterBytes int
	PathFilterPaths int

	// PoolRawGroups counts the pool groups JSS8 stored uncompressed because
	// compressing them did not make them smaller.
	PoolRawGroups int

	// Strings short enough to be written inline instead of pooled.
	InlineStrings int // distinct string values kept out of the pool
	InlineRefs    int // occurrences written inline
	InlineBytes   int // bytes those occurrences occupy, tag included
}

// Ratio returns the compression ratio, original over archive, or 0 when the
// archive size is unknown.
func (s *Stats) Ratio() float64 {
	if s == nil || s.TotalBytes == 0 {
		return 0
	}
	return float64(s.RawBytes) / float64(s.TotalBytes)
}
