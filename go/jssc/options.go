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

	// Layout is the bitmask of structure walk aids written into the header
	// (specification §5.3). Zero selects LayoutDefault. LayoutPathFilter is not
	// the caller's to set: the encoder decides per document whether the filter
	// earns its bytes (§7B.6).
	Layout Layout

	// Format declares the input's textual form. The zero value, FormatJSON,
	// encodes a single document. FormatJSONL and FormatNDJSON encode a
	// multi-document archive over one shared string pool. This is never
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

	// StructBlockBytes is the target plain size of one structure block
	// (specification §8A.3). Zero selects DefaultStructBlockBytes; a negative
	// value stores the structure as a single block.
	//
	// A block is the unit a reader inflates to reach any document inside it, so
	// this trades archive size against the cost of one cold read — the same
	// trade PoolGroupShift makes for the pool.
	StructBlockBytes int

	// OmitKeyDirectory suppresses the key hash directory. The section is still
	// present, with a count of zero (specification §7A.2), so a reader falls
	// back to searching the key pool; only the lookup shortcut is gone. Useful
	// for the rare document whose keys are so numerous that the table is not
	// worth its bytes.
	OmitKeyDirectory bool

	// OmitPathFilter suppresses the key-path filter. The section is emitted
	// only when it is small relative to what it indexes (specification §7B.6),
	// so most documents that carry one wanted it; this exists so a caller can
	// price the section on its own rather than infer it.
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

func (o Options) layout() Layout {
	if o.Layout == 0 {
		return LayoutDefault
	}
	return o.Layout
}

// validate checks the option combination before any work is done.
func (o Options) validate() error {
	if !o.Structure.valid() {
		return errUnsupportedf("structure codec %d", uint8(o.Structure))
	}
	if o.Format != FormatJSON && !o.Format.LineOriented() {
		return errUnsupportedf("content format %d", uint8(o.Format))
	}
	if reserved := o.layout() &^ layoutStored; reserved != 0 {
		return errUnsupportedf("struct_layout bits 0x%02x are not the caller's to set", uint8(reserved))
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
	StructLayout  Layout        // struct_layout as stored
	ContentFormat ContentFormat // declared textual form
	Version       int           // archive version, always 8
	DocCount      int           // top-level documents
	IndexWidth    int           // pool index width in bytes
	UniqueStrings int           // pool cardinality

	// KeyWidth sizes key-pool references; DistinctKeys is the key pool's
	// cardinality; KeyPoolBytes its stored section.
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
	// means the structure must be inflated whole.
	StructBlocks     int
	StructBlockBytes int

	// KeyDirEntries is how many keys the key hash directory lists, and
	// KeyDirBytes what the section costs including its count. Zero entries
	// means the archive carries no directory: the encoder was told not to
	// build one.
	KeyDirEntries int
	KeyDirBytes   int

	// PathFilterBytes is the size of the key-path filter section, and
	// PathFilterPaths how many distinct root-to-leaf key paths it indexes. Both
	// are zero when the archive carries no filter, which is the common case for
	// documents small enough that the section would not pay for itself.
	PathFilterBytes int
	PathFilterPaths int

	// PoolRawGroups counts the pool groups stored uncompressed because
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
