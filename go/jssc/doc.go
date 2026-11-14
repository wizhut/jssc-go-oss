// Package jssc is the reference implementation of JSSC — JSON String-pool
// Compressed — as defined by the JSSC specification (5.0.0-draft).
//
// JSSC compresses JSON and newline-delimited JSON by collecting every distinct
// string into a pool, compressing each pool entry independently with a
// short-string codec, and encoding the value tree as a compact binary structure
// of tags and pool indices. The archive is self-contained: no external schema
// or shared dictionary is needed to decode it.
//
// # Archive shape
//
// A JSS8 archive, the current revision, is:
//
//	"JSS8" algo struct_codec struct_layout index_width content_fmt
//	pool_group_shift pool_count doc_count
//	codec_state pool_groups key_directory [path_filter]
//	struct_len structure
//
// where the structure region is a block index followed by independently
// inflatable blocks. The path filter is present only when struct_layout bit 2
// is set; a reader must honour that bit to find struct_len, since the section
// occupies bytes rather than merely describing them.
//
// One header shape serves both single and multi-document archives, and the
// structure always opens with a table of per-document root offsets, so any
// document can be reached without scanning its predecessors.
//
// The pool is compressed in groups of 1<<pool_group_shift entries, and the
// default is one entry per group, so decoding one string touches no other. That
// is what the pool codec is built for: fsst is a small-string codec that
// compresses a key or a short value on its own, which is the size a pool entry
// actually is.
//
// A group larger than one still encodes and decodes correctly, but nothing
// gains from it. Grouping existed for a general compressor that could span
// entries and find the redundancy between them; no such codec is registered any
// more, so a larger group only makes a reader walk neighbours' framing to reach
// the string it wanted.
//
// # The key hash directory
//
// Between the pools and the structure an archive carries a table mapping the
// CRC-32 of every object key to its key-pool index, stored uncompressed and
// sorted by hash. Object members are laid out in
// ascending key-index order, so a field lookup is an integer binary search over
// the structure — and the directory supplies that integer without decompressing
// any pool group at all. A key absent from the directory is absent from the
// document, so a query for a field nothing has costs no pool work either.
//
// Keys whose hashes collide are all listed, and a reader settles the run by
// comparing the key-pool strings. Options.OmitKeyDirectory writes a count of
// zero instead, for callers who would rather have the bytes — it is a fraction
// of a percent of the archive on most documents, and about 10% on one whose
// object keys are themselves data; readers then search the sorted key pool,
// which costs about log2(groups)+1 decompressions.
//
// # The blocked structure
//
// The structure is compressed in blocks of about Options.StructBlockBytes plain
// bytes, cut at document boundaries so no document spans a block, with an index
// of per-block stored and plain lengths. Opening inflates nothing; reading a
// document inflates the one block that holds it. The per-document root table
// holds absolute offsets rather than lengths, so reaching document i is a single
// four-byte read instead of a sum over every document before it.
//
// A single DEFLATE stream cannot be entered in the middle: on a
// million-document archive, reading one field that way cost 186 ms and 270 MiB
// and grew with the archive. Blocking makes that one block — measured at 686
// microseconds and 8.3 MiB — for about 3% of the archive. On documents small
// enough to fit in a single block it costs the twelve-byte index and nothing
// else.
//
// This package reads and writes JSS8 alone. The draft versions JSS1–JSS7 are
// described in the specification's Appendix E for reference; opening one
// returns ErrUnsupported.
//
// # Structure tags
//
//	0x00       null
//	0x01       false
//	0x02       true
//	0x03       number, uint8 length + JSON number lexeme
//	0x04       pooled string, index_width bytes little-endian
//	0x05       array,  uint32 count [+ offset table] + values
//	0x06       object, uint32 count [+ offset table] + (key index, value)*
//	0x07+n     inline string of n bytes, n in [0,4]
//
// A string value no longer than index_width is written inline rather than
// pooled: the reference would cost at least as many bytes as the string
// itself, plus a pool entry. Object keys are always pooled.
//
// # Reading
//
// [Decode] restores the original text. [Open] and [OpenBytes] return an
// [Archive] for selective access: the pool stays undecoded until a string is
// actually needed, and [Archive.Query] walks the binary structure directly,
// skipping subtrees using length prefixes and child offset tables.
//
// Package sdk wraps this package with the stable, high-level API
// (Compress/Decompress/Query); this package exposes the format internals.
package jssc
