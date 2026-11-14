# JSSC: JSON String-pool Compressed Archives

```
Document:       draft-jssc-00
Format version: JSS8 (specification revision 5.6.0-draft)
Category:       Experimental
Status:         Draft. Written in the style of an RFC; not submitted to any
                standards body.
License:        Apache License 2.0 (see LICENSE at the repository root)
```

## Abstract

JSSC is a binary archive format for JSON values. It removes the two
redundancies of JSON text separately: every distinct string is stored once in a
**string pool**, compressed entry by entry with a short-string codec; and the
value tree becomes a **structure** of one-byte tags and fixed-width pool
references, carrying length prefixes and offset tables so that a reader can
skip what it does not want. An archive can be opened without decompressing
anything, walked without parsing, and queried with a path expression while
most of its strings stay compressed.

This document defines version **JSS8** of the format normatively: the
container, the header registries, the codec state, the pools, the key
directory, the key-path filter, the structure and its blocking, and the
procedures and obligations of encoders and decoders.

## Table of Contents

1. [Introduction](#1-introduction)
2. [Conventions](#2-conventions)
3. [Data Model](#3-data-model)
4. [Container Layout](#4-container-layout)
5. [Header Field Registries](#5-header-field-registries)
6. [Codec State](#6-codec-state)
7. [The String Pools](#7-the-string-pools)
7A. [The Key Hash Directory](#7a-the-key-hash-directory)
7B. [The Key-Path Filter](#7b-the-key-path-filter)
8. [Structure Encoding](#8-structure-encoding)
8A. [The Blocked Structure](#8a-the-blocked-structure)
9. [Structure Compression](#9-structure-compression)
10. [Encoding Procedures](#10-encoding-procedures)
11. [Decoding Procedures](#11-decoding-procedures)
12. [Query Protocol](#12-query-protocol)
13. [Round-Trip Semantics](#13-round-trip-semantics)
14. [Limits and Rejection](#14-limits-and-rejection)
15. [Security Considerations](#15-security-considerations)
16. [Conformance](#16-conformance)
17. [Extension Points](#17-extension-points)
18. [IANA Considerations](#18-iana-considerations)
19. [References](#19-references)
- [Appendix A. Grammar](#appendix-a-grammar)
- [Appendix B. A Worked Example](#appendix-b-a-worked-example)
- [Appendix E. Earlier Versions](#appendix-e-earlier-versions)

---

## 1. Introduction

### 1.1. Purpose

A JSON document carries two kinds of redundancy. The first is *textual*: object
keys repeat once per record, enumerated values repeat across a corpus. The
second is *syntactic*: braces, brackets, quotation marks, colons and commas,
which carry no information once the document is parsed.

General-purpose compressors remove both, and remove them well. What they cannot
do is give the bytes back one field at a time: a DEFLATE or Zstandard stream is
a chain of back-references, so reading the last record means decompressing
every record before it and parsing all of them.

JSSC removes the two redundancies separately, so that each can be undone
independently:

1. every distinct string is collected into a **pool** and compressed there,
   once, one entry at a time;
2. the value tree becomes a **structure** of one-byte tags and fixed-width pool
   references, with the syntax gone;
3. the structure carries **walk aids** — a length prefix on every value and a
   child offset table on containers large enough to warrant one — so a reader
   can jump over what it does not want.

### 1.2. Design Goals

| ID | Goal | Meaning |
|----|------|---------|
| G1 | Self-contained | An archive carries its codec identifier, codec state, pools and structure. Decoding requires nothing external. |
| G2 | Deduplicated strings | Each distinct string appears at most once in its pool. |
| G3 | Bounded decode dependency | Reading pool entry *i* decompresses at most *B* entries, *B* being the group size in the header. *B* = 1 (the default) makes entries independent. |
| G4 | Codec-agnostic structure | The structure encoding does not depend on the pool codec. |
| G5 | Scalable references | Reference widths follow pool cardinality: 1, 2 or 4 bytes. |
| G6 | Value fidelity | A round trip preserves JSON values, number lexemes included. |
| G7 | Transmission-ready | Archives may be sent as they are; structure compression is optional and self-describing. |
| G8 | Multi-document pooling | Newline-delimited input shares one pool across every document. |
| G9 | Selective access | A path expression can be evaluated against a stored archive without materializing the document. |

### 1.3. Non-Goals

JSSC does not attempt to be:

- a whole-file compressor competitive with DEFLATE [RFC1951], Zstandard
  [RFC8878] or Brotli [RFC7932]. Compressing each string alone, so that it can
  be read alone, costs size: on most documents a general-purpose compressor
  over the JSON text produces a smaller result;
- a schema-driven encoding;
- a preserver of insignificant whitespace or of object member order;
- a streaming decoder of a compressed structure block without first
  decompressing that block.

### 1.4. Relationship to Other Formats

CBOR [RFC8949] and MessagePack encode the JSON value model in binary, removing
the syntax but not the textual repetition. Columnar formats transpose records
into columns; JSSC declines to, keeping documents whole and individually
addressable. The pool codec, FSST [FSST], was designed for random access to
individually compressed short strings.

### 1.5. Conformance Language

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD",
"SHOULD NOT", "RECOMMENDED", "MAY" and "OPTIONAL" in this document are to be
interpreted as described in [RFC2119].

- A **conformant decoder** MUST accept every archive produced by a conformant
  encoder under this document.
- A **conformant encoder** MUST produce archives that a conformant decoder
  restores to an equivalent JSON value under Section 13.
- An implementation MAY also read archives of the earlier versions `JSS1`
  through `JSS7` (Appendix E). Writing them is forbidden.

### 1.6. Document Status

This draft matches the reference implementation in the `go/` directory of the
repository that carries it. Where the two disagree, one of them is wrong, and
the disagreement is a bug in whichever proves to be at fault.

---

## 2. Conventions

### 2.1. Terminology

| Term | Definition |
|------|------------|
| Archive | A complete JSSC byte sequence: header, codec state, pools, key directory, optional path filter, structure. |
| Pool | An ordered sequence of distinct strings. An archive has two: the **key pool** and the **value pool**. |
| Pool entry | One string in a pool, addressed by its zero-based index. |
| Pool group | A run of consecutive pool entries stored as a unit. |
| Segment | A run of 64 consecutive pool groups, located through the group directory. |
| Index width *w* | The byte width of a value-pool reference: 1, 2 or 4. |
| Key width | The byte width of a key-pool reference: 1, 2 or 4. |
| Structure | The binary encoding of one or more JSON value trees. |
| Root | The top-level value of one document. |
| Walk aids | The optional length prefixes and child offset tables selected by `struct_layout`. |
| Content format | The declared textual form: `json`, `jsonl` or `ndjson`. |

### 2.2. Byte Conventions

1. An archive is an ordered sequence of octets. Offsets count from zero.
2. All fixed-width multi-byte integers are **little-endian**.
3. **uvarint** is the unsigned LEB128 encoding (the encoding of Protocol
   Buffers varints and of Go's `binary.Uvarint`): seven bits per byte, least
   significant group first, high bit set on every byte but the last.
4. There is no padding between fields.
5. The archive ends with the last byte of the structure. A decoder MAY reject
   trailing bytes when its caller expects an exact buffer.
6. Tag values are given in hexadecimal; other constants are decimal unless
   prefixed `0x`.

---

## 3. Data Model

An archive represents an ordered, non-empty sequence of JSON values [RFC8259],
called *documents*. A single-document archive represents one JSON value; a
multi-document archive represents the values of a newline-delimited stream, in
order. The value model is JSON's: objects, arrays, strings, numbers, `true`,
`false`, `null`. JSSC adds no types and removes none.

Two decisions in the layout make selective access possible:

- **The pool codec and the structure codec are independent.** Either can change
  without affecting the other.
- **The pools are addressed, not scanned.** The structure holds indices, and a
  reader resolves an index to a string only when a comparison or an output
  needs it.

---

## 4. Container Layout

### 4.1. The Archive

```
Offset  Size  Field
------  ----  --------------------------------------------------
0       4     magic             "JSS8"
4       1     algo              pool codec id           (5.1)
5       1     struct_codec      structure codec id      (5.2)
6       1     struct_layout     walk-aid flags          (5.3)
7       1     index_width       1 | 2 | 4               (5.5)
8       1     content_fmt       0=json 1=jsonl 2=ndjson (5.4)
9       1     pool_group_shift  group size = 1 << shift (5.6)
10      4     key_count         K   (uint32 LE)
14      4     pool_count        N   (uint32 LE)
18      4     doc_count         D   (uint32 LE), D >= 1
22      var   codec_state       codec-defined           (6)
...     var   key pool          len, flags, dir, groups (7)
...     var   value pool        len, flags, dir, groups (7)
...     var   key_directory     key hash table          (7A)
...     var   path_filter       only if layout bit 2    (7B)
...     4     struct_len        stored structure size (uint32 LE)
...     L     structure         block index + blocks    (8A)
```

A decoder MUST reject an archive whose magic is not `JSS8`, whose pool codec is
unregistered, whose structure codec is unknown, whose `index_width` is outside
{1, 2, 4}, whose `pool_group_shift` exceeds 12, whose `content_fmt` exceeds 2,
or whose `doc_count` is zero.

No section carries its own offset. Every section is fixed-size or
self-delimiting, so a reader reaches the structure by reading lengths, never
payloads.

### 4.2. The Document Table

The plain structure — the concatenation of the structure blocks of Section 8A,
after decompression — MUST begin with one entry per document, followed by the
document roots:

```
D x uint32 LE   root_offset[i]     // plain offset of document i's root
root_0 || root_1 || ... || root_{D-1}
```

`root_offset[i]` is the absolute offset of document *i*'s root within the plain
structure. Each MUST be at least 4D and at most the plain structure length, and
they MUST be ascending. A decoder MUST be able to open document *i* without
reading documents 0 … *i*−1; storing offsets rather than lengths makes that one
four-byte read. A single document is the case D = 1 and carries the table
anyway.

---

## 5. Header Field Registries

### 5.1. Pool Codec (`algo`)

| ID | Name | State | Notes |
|----|------|-------|-------|
| 0 | `fsst` | FSST symbol table (6.1) | The default and only registered codec. |

Every registered codec MUST be a **small-string codec**: it compresses one pool
entry at a time and MUST NOT require more than one entry to decode an entry.
A decoder that encounters an unregistered `algo` MUST fail.

**Retired identifiers.** 1, 2 and 3 MUST NOT be assigned. They were used by
earlier versions (1 `unishox`, a per-entry codec; 2 a raw DEFLATE stream over a
whole pool group; 3 an LZ4 block over a whole group). A decoder MUST fail on
any of them and SHOULD distinguish that failure from an unassigned identifier,
so that an archive written by an older encoder is rejected rather than misread.

### 5.2. Structure Codec (`struct_codec`)

| ID | Name | Stored form |
|----|------|-------------|
| 0 | `raw` | The structure blocks verbatim. A reader traverses them in place. |
| 1 | `deflate` | Each block a raw DEFLATE stream [RFC1951], with no zlib or gzip wrapper. |
| 2 | `lz4` | Each block an LZ4 block [LZ4] (no frame), with the verbatim-block rule of 8A.2. The default an encoder writes. |

Unknown values MUST cause the decoder to fail.

**Fallback rule.** If an encoder selects `deflate` or `lz4` but the stored
blocks together are not strictly smaller than the plain structure, it MUST
store the plain bytes as a single block and MUST set `struct_codec` to `raw`.
The header describes the bytes that follow it, never the bytes the encoder
intended.

**Size limit.** `struct_len` is a uint32. An encoder MUST fail if the stored
structure would exceed 2^32 − 1 bytes.

### 5.3. Structure Layout (`struct_layout`)

A bit mask:

| Bit | Name | Value | Meaning |
|-----|------|-------|---------|
| 0 | `LayoutLenPrefix` | 0x01 | Every value is preceded by a uvarint payload length (8.1). |
| 1 | `LayoutOffsets` | 0x02 | Every container carries an offset-width descriptor, and those that earn one a child offset table (8.3, 8.4). |
| 2 | `LayoutPathFilter` | 0x04 | A key-path filter section is present between the key directory and `struct_len` (7B). |

The default an encoder writes is 0x03. Bit 2 is set by the encoder per
document (7B.6), never as a mode. Bits 3 through 7 are reserved: writers MUST
NOT set them and readers MUST ignore them.

Bit 2 differs from the others: bits 0 and 1 describe how bytes are to be
*interpreted*; bit 2 says a section is *physically present*. A reader MUST
honour it to locate `struct_len`, and a reader with no use for the filter MUST
still parse the filter's two header fields to learn its extent.

### 5.4. Content Format (`content_fmt`)

| ID | Name | Line-oriented | Notes |
|----|------|---------------|-------|
| 0 | `json` | no | A single JSON value. |
| 1 | `jsonl` | yes | One JSON value per non-empty line. |
| 2 | `ndjson` | yes | Identical wire encoding to `jsonl`. |

`jsonl` and `ndjson` share an encoding; the distinct identifier lets
decompression restore the name the caller chose. An encoder MUST NOT infer the
format from a file name or from the bytes (10.5).

### 5.5. Index Widths

`index_width` sizes references into the **value** pool:

| Value | Permitted value-pool cardinality N |
|-------|------------------------------------|
| 1 | N ≤ 256 |
| 2 | N ≤ 65536 |
| 4 | N ≤ 2^32 − 1 |

Other widths MUST be rejected. Decoders MUST take the width from the header and
MUST NOT recompute it.

References into the **key** pool — object member keys (8.3) and key directory
entries (7A.2) — use the **key width**: the smallest of 1, 2, 4 whose row above
admits `key_count`, with a `key_count` of zero taking width 1. It is derived
from `key_count`, not stored, and an encoder MUST size key references exactly
so.

### 5.6. Pool Group Shift (`pool_group_shift`)

The pool group size is *B* = 2^`pool_group_shift` entries. A shift of 0 gives
groups of one entry, which is what an encoder SHOULD write and what the
reference encoder writes by default. The shift MUST be at most 12, bounding a
group at 4096 entries; a decoder MUST reject a larger value. A decoder MUST
handle any legal shift.

---

## 6. Codec State

`codec_state` sits immediately after the fixed header and is consumed by the
pool codec before any group is read. Its length is not stored: the codec's own
framing determines how many bytes it takes. One codec state serves both pools.

### 6.1. FSST State (`algo` = 0)

The state is a serialized FSST symbol table:

```
uint64 LE   version_word
uint8[8]    length_histogram    // counts of symbols of length 1..8
bytes       symbols             // concatenated symbol bodies
```

`version_word`:

```
bits 63..32   table_version = 20190218
bits 31..16   suffix_lim    (encoder hint; a decoder ignores it)
bits 15..8    n_symbols     (at most 255)
bits  7..0    1             (marker)
```

A decoder MUST reject a table whose `table_version` is not 20190218 or whose
`n_symbols` exceeds 255.

`length_histogram[i]`, for *i* in [0, 7], is the number of symbols of length
*i*+1. Its sum MUST equal `n_symbols`; a decoder MUST reject a table where it
does not.

`symbols` is the concatenation of the `n_symbols` symbol bodies, ordered by
length group **2, 3, 4, 5, 6, 7, 8, then 1** — not ascending — with the
histogram counts giving each group's size. A symbol of length ℓ occupies ℓ
bytes, in string order.

An encoder trains the table on the strings of both pools together. How it
trains is not specified: any valid table decodes. A table trained on no strings
is a valid table with no symbols.

### 6.2. FSST Decoding

Symbols take codes 0 to `n_symbols` − 1 in the order they appear in `symbols`.
A compressed string is a sequence of codes:

- a code *c* < `n_symbols` emits the bytes of symbol *c*;
- the code 255 is an **escape**: the next byte is emitted literally.

An encoder MUST NOT emit a code in [`n_symbols`, 255) or end a string with an
escape. A decoder MAY reject either.

---

## 7. The String Pools

There are two pools, separated by *role*: the **key pool** holds strings that
appear as object keys, the **value pool** holds strings that appear as string
values. They are stored as two consecutive sections of identical framing, key
pool first, each addressed by its own index space. A string used in both roles
appears in both pools.

### 7.1. Membership

The key pool is the set of distinct object keys, without exception. The value
pool is the set of distinct string values that are not written inline (8.6).
Numbers, booleans and `null` are never pooled.

Keys are always pooled because a key is written as a bare index with no tag
byte; there is no room to distinguish an inline key from a reference.

### 7.2. Ordering

An encoder MUST sort each pool's strings lexicographically by **unsigned byte
order**, independently. The order is not needed for correctness, but it makes
encoding deterministic, and, with object members written in key-index order
(8.5), it makes decoded members come back in lexicographic key order.

### 7.3. Groups

Pool entries are stored in groups of *B* = 2^`pool_group_shift` consecutive
entries. Group *g* covers entries [*gB*, min((*g*+1)*B*, *N*)), where *N* is
the pool's cardinality (`key_count` for the key pool, `pool_count` for the
value pool), and there are *G* = ⌈*N*/*B*⌉ groups. A group is the unit a reader
decompresses to reach any entry inside it.

### 7.4. Group Framing

Each pool section is:

```
uint32 LE                         pool_len          // bytes below: flags + group_dir + groups
ceil(G/8) bytes                   stored_raw flags  // bit g = group g is plaintext
max(0, ceil(G/64)-1) x uint32 LE  group_dir         // entry k-1 = offset of group 64k
G x { uvarint stored_len || stored_bytes }
```

**The section length.** `pool_len` counts every byte after itself: the flag
bitmap, the group directory and the framed groups. A writer MUST set it to
exactly that count; a reader MUST validate it against the bytes remaining
before using it to locate the section that follows. It is what makes opening
an archive O(1): a reader finds the next section without reading a group.

**The flags.** Bit *g* is byte ⌊*g*/8⌋, bit *g* mod 8, counting from the least
significant. Bits past group *G*−1 MUST be zero on write and MUST be ignored on
read.

**The group directory.** The groups are counted off in **segments** of
*S* = 64: segment *k* covers groups [*kS*, min((*k*+1)*S*, *G*)), and there are
⌈*G*/*S*⌉ segments. Let *E* be the length of the framed groups — `pool_len`
less the bitmap and the directory — and let segment *k* begin at *b_k*, counted
from the first byte of group 0's length prefix, so that *b_0* = 0 and
*b*_⌈*G*/*S*⌉ = *E*. The directory stores the boundaries in between: entry
*k*−1, for 1 ≤ *k* < ⌈*G*/*S*⌉, is *b_k*. A pool of at most 64 groups has an
empty directory.

A writer MUST write every entry exactly. A reader MUST reject a section whose
bitmap and directory do not fit in `pool_len`. Before it reads any group in
segment *k*, a reader MUST check that *b_k* < *b_k+1* ≤ *E*, MUST validate each
length in the segment against *b_k+1*, and MUST reject the section if the
segment's groups do not end exactly at *b_k+1*. These checks bind however the
reader arrived at the segment; one that walks the framing from group 0 checks
each boundary as it crosses it. A decoder MUST reject a pool section that
declares more groups than its bytes could hold (every group costs at least one
byte of framing).

A reader SHOULD locate group *g* through the directory: seek to
*b*_⌊*g*/*S*⌋ and walk the lengths of that one segment, which reads at most 64
lengths whatever the size of the pool.

### 7.5. Opportunistic Groups

A group whose compressed form is not strictly smaller than its plaintext MUST
be stored as the plaintext, with its flag bit set. A tie is stored raw.

The flag describes the bytes, not the encoder's intention: a decoder MUST apply
the pool codec to a group whose bit is clear and MUST NOT apply it to a group
whose bit is set. Applying a codec to plaintext rarely fails loudly; it returns
a wrong string. The plaintext of a raw group is exactly the group plaintext of
7.6, so a reader may point entries directly into the archive.

### 7.6. Group Plaintext

The bytes a group's codec compresses depend on how many entries the group
holds:

```
one entry        the entry bytes, and nothing else
several entries  repeated, once per entry in the group:
                   uvarint entry_len || entry_bytes
```

The rule follows the number of entries actually in the group, not the
configured group size: the last group of a pool with *B* > 1 may hold a single
entry, and is then the bare entry bytes. A decoder MUST take the number of
entries in a group from the pool cardinality and the group size, not from the
payload, and MUST reject a group whose entry lengths do not fit its plaintext.

### 7.7. Empty Pool

A cardinality of 0 is valid: a document of only numbers, booleans and `null`,
or one whose every string value is short enough to inline, has an empty value
pool. Its section is then a `pool_len` of 0.

### 7.8. Decoder Obligations

1. A decoder MUST be able to locate every group without decompressing any.
2. A decoder MAY decompress groups lazily and SHOULD cache the entries a
   decompressed group yields.
3. A decoder MUST reject an index outside [0, *N*).

---

## 7A. The Key Hash Directory

Between the value pool and `struct_len` (or the path filter), an archive
carries a table mapping a hash of every object key to its key-pool index. The
section is always present.

### 7A.1. Purpose

Object members are stored in ascending key-index order (8.5), so knowing a
field name's index turns a field lookup into an integer binary search over the
structure, decoding no strings. The directory gives that index by hashing a
string the caller already holds and searching a flat, uncompressed array. It
lists **every** key, so a name absent from it names no member anywhere: a
negative field lookup is free.

### 7A.2. Encoding

```
uint32 LE   dir_count = K
K x {
  uint32 LE  hash        // 7A.3
  uintW  LE  key_index   // W = the key width (5.5)
}
```

Entries MUST be sorted by `hash`, ascending. Entries sharing a `hash` form a
**collision run** and MUST be ordered by `key_index`, ascending, within it.
Each `key_index` MUST be less than `key_count`. *K* MUST be either 0 or
`key_count`: when present the directory is complete. *K* = 0 is a valid,
suppressed directory (only the count is written), and a reader MUST then fall
back to searching the key pool.

### 7A.3. The Hash

`hash` is the CRC-32 of the key's UTF-8 bytes under the IEEE 802.3 polynomial
0x04C11DB7, reflected input and output, initial and final value 0xFFFFFFFF —
the function commonly named `crc32-ieee`. Test vector:
hash(`"id"`) = 0xBF396750.

### 7A.4. Collisions

The directory is **complete**: a hash absent from it belongs to no key. What a
hit promises depends on its run:

1. A run of exactly one entry is **exact**. The reader MAY take its
   `key_index` without verifying it against the key pool.
2. A run of more than one entry is a set of **candidates**. The reader MUST
   compare the sought key against the key-pool string of each candidate, and
   MUST conclude the key is absent if none matches.

An encoder MUST list every colliding key and MUST NOT write *K* = 0 because a
collision exists.

### 7A.5. Encoder Requirements

An encoder MUST write the section. It SHOULD list every key; if it lists any,
it MUST list all of them. It MAY write *K* = 0 at its caller's request and
MUST NOT do so for any other reason.

### 7A.6. Decoder Requirements

A decoder MUST validate the section before trusting it: reject *K* greater than
`key_count`, a section that does not fit in the bytes remaining, any
`key_index` at or beyond `key_count`, entries not in ascending `hash` order,
and a collision run not in ascending `key_index` order. The extent MUST be
checked before it sizes an allocation.

A decoder MAY ignore the directory and search the key pool; the results MUST be
identical. A decoder that uses a non-empty directory MAY treat a single-entry
run as exact, MUST verify a longer run (7A.4), and MAY treat a miss as proof
that no object in the document has that member.

---

## 7B. The Key-Path Filter

An archive MAY carry, between the key directory and `struct_len`, a
probabilistic set of the key paths the document contains. The section is
present if and only if `struct_layout` bit 2 is set.

### 7B.1. Purpose

The key directory says whether a string is used as a key *anywhere*. The filter
answers the question a path query asks: a path it rejects is not in the
document, and the query returns nothing without reading the structure.

### 7B.2. Paths

A **key path** is the sequence of object keys on the route from a document root
to a value. Arrays contribute nothing: subscripts are elided, so
`$.name[0].the_name` and `$.name[7].the_name` are the same path. The indexed set
is every key path the document contains, every prefix included.

### 7B.3. The Path Hash

The hash of the empty path is 0. Extending a path whose hash is *p* by a key
*k* gives

```
pathhash(p, k) = crc32( le32(p) || utf8(k) )
```

where `le32` is the four-byte little-endian encoding of *p* and `crc32` is the
function of 7A.3.

### 7B.4. Encoding

```
uvarint   m          // bitmap size in bits
uint8     k          // probes per path
m/8 bytes bitmap     // bit i is byte i/8, bit i mod 8, from the least significant
```

*m* MUST be positive and a multiple of 8; *k* MUST be at least 1. A path hash
*h* occupies *k* bit positions, derived by double hashing on unsigned 64-bit
values, modulo 2^64:

```
h1 = h
h2 = (h * 0x9E3779B1) | 1
bit_i = (h1 + i * h2) mod m       for i in [0, k)
```

A path is present when every one of its *k* bits is set.

### 7B.5. Sizing

An encoder that writes the section SHOULD size it at 10 bits per distinct path,
floored at 64 bits, with *k* = 7. These are not normative: *m* and *k* are
stored.

### 7B.6. When to Write It

An encoder SHOULD write the section only when it is small beside what it saves
a walk through — the reference encoder requires the value-pool payload plus the
stored structure to be at least 32 times the filter — and MUST clear bit 2 when
it does not write it.

### 7B.7. Decoder Requirements

A decoder MUST reject a filter whose *m* is zero or not a multiple of 8, whose
*k* is zero, or whose bitmap does not fit in the bytes remaining. It SHOULD
bound *m* before allocating; the reference decoder refuses more than 2^27 bits.

A decoder MAY ignore the filter. If it uses it: a path whose bits are not all
set is not in the document, and the decoder MAY return the empty result without
walking; a path whose bits are all set MUST be walked as usual.

The filter may only be wrong in one direction. An implementation MUST NOT apply
it to an expression whose key path it cannot determine exactly. A wildcard over
an *object* stands for a key and is part of the path; a wildcard MAY be elided
only where the container it selects from is known to be an array. Descent,
filter expressions and unions MUST NOT be filtered. A leading subscript over the
documents of a multi-document archive is elided like any other array step.

---

## 8. Structure Encoding

### 8.1. Value Framing

When `LayoutLenPrefix` is set, every value is

```
uvarint(payload_len) || tagged_payload
```

where `payload_len` counts the bytes of `tagged_payload` — the tag and its
payload, nested values included with their own prefixes — and not the uvarint
itself. Skipping a value is: read *L*, advance past the uvarint and *L* more
bytes. When the bit is clear, a value is a bare tagged payload and skipping it
requires parsing it. An encoder SHOULD set the bit.

### 8.2. Tags

| Tag | Node | Payload |
|-----|------|---------|
| 0x00 | `null` | none |
| 0x01 | `false` | none |
| 0x02 | `true` | none |
| 0x03 | number | uint8 `nlen`, then `nlen` bytes of the JSON number lexeme |
| 0x04 | pooled string | value-pool index, `index_width` bytes, little-endian |
| 0x05 | array | see 8.3 |
| 0x06 | object | see 8.3 |
| 0x07 + *n* | inline string | *n* raw bytes, *n* in [0, 4] |
| 0x0C–0xFF | reserved | MUST NOT be written; a decoder MUST fail on one |

### 8.3. Containers

```
array:   0x05  uint32 LE m  [offset_width]  [offset table]  value*
object:  0x06  uint32 LE m  [offset_width]  [offset table]  (key_index value)*
```

*m* is the child count. Each `value` is a framed value (8.1). Each `key_index`
is a key-pool reference of the key width (5.5); object keys are always pooled,
never inline.

`offset_width` is one descriptor byte, present whenever `LayoutOffsets` is set
and absent otherwise:

| Value | Meaning |
|-------|---------|
| 0 | No offset table for this container. |
| 1 | Table present; each entry one byte. |
| 2 | Table present; each entry two bytes, little-endian. |
| 4 | Table present; each entry four bytes, little-endian. |

Any other value MUST cause the decoder to fail.

### 8.4. Offset Tables

When present, the table follows `offset_width` and holds *m* entries. Each
entry is a byte offset **relative to the first child byte** — the first byte of
the first element for an array, of the first key index for an object. Array
offsets address element values; object offsets address member **keys**, since
a lookup reads the key before it decides whether it wants the value.

An encoder emits a table only if `LayoutOffsets` is set, *m* ≥ 8, **and** the
children span at least 2048 bytes. The entry width is then the narrowest that
addresses the span: 1 byte below 256, 2 below 65536, otherwise 4. A reader
cannot recompute the decision, which is why it is stored per container.

### 8.5. Object Member Order

An encoder MUST emit object members sorted by ascending key-pool index of the
key. A reader can then binary search an object on integers alone. Duplicate
keys in the input collapse to one member; an encoder SHOULD keep the last, as
common JSON parsers do.

### 8.6. Inline Short Strings

A string **value** whose byte length |*s*| satisfies |*s*| ≤ *w* (the
`index_width`) MUST be written inline under tag 0x07 + |*s*| and MUST NOT
appear in the value pool. A pooled value costs 1 + *w* bytes plus a pool entry;
an inline value costs 1 + |*s*| and no entry, so for |*s*| ≤ *w* inline is
never larger. Object keys are excluded from this rule (7.1).

### 8.7. Numbers

Tag 0x03 stores the original JSON number lexeme as ASCII, prefixed by a uint8
length. A lexeme MUST be at most 255 bytes. Spelling survives exactly: `1e3`
and `1000` are distinct on the wire and both come back as written. There is no
binary numeric encoding.

### 8.8. Index Width Selection

`index_width` and the value pool's membership depend on each other: the width
follows the cardinality, and the cardinality follows the width through the
inline rule. An encoder MUST resolve them together:

1. Collect every candidate string value (keys are not candidates).
2. Let *w* be the width implied by the candidate cardinality.
3. Build the value pool at width *w*, excluding values with |*s*| ≤ *w*.
4. While the pool built at width *w* would fit a narrower width, set *w* to
   that width and rebuild the membership. Stop when narrowing would push the
   pool back over the boundary.
5. Emit `index_width` = *w*.

The key width needs no such procedure: it follows `key_count` (5.5). Decoders
take both widths from the header.

---

## 8A. The Blocked Structure

### 8A.1. Purpose

The structure region is a block index followed by independently stored blocks,
so that reaching a document decompresses the block that holds it rather than
everything before it.

### 8A.2. Encoding

The bytes counted by `struct_len` are:

```
uint32 LE   block_count = B
B x {
  uint32 LE  stored_len   // bytes this block occupies below
  uint32 LE  plain_len    // bytes it decompresses to
}
B x bytes   block         // in order
```

*B* MUST be at least 1. Every block MUST be stored under the codec named by
`struct_codec`; blocks MUST NOT mix codecs. The plain structure is the
concatenation of the blocks in order, and is what Sections 4.2 and 8 define.

**The verbatim block.** Under `struct_codec` = `lz4`, a block whose
`stored_len` equals its `plain_len` holds the plain bytes verbatim, not an LZ4
block. An encoder MUST store a block verbatim whenever its LZ4 form would not be
strictly smaller, so the equality is unambiguous. Under `deflate` no such rule
exists: every block is a DEFLATE stream, and the only escape is the
whole-region fallback of 5.2.

### 8A.3. Where a Block May End

A block MAY end at any plain offset. A reader MUST produce the same answers
whatever the cuts are, assembling a read that spans a boundary from the blocks
it touches.

An encoder SHOULD end blocks at document boundaries while documents are smaller
than its target, and, under a compressing codec, SHOULD cut a document larger
than the target at intervals of about the target. Within the root offset table
a block SHOULD end only at a multiple of four, so that no entry spans two
blocks. The reference encoder targets 64 KiB of plain structure per block.

### 8A.4. The Root Table Holds Offsets

The table at the head of the plain structure (4.2) holds absolute plain
offsets, not lengths, so opening document *i* is one read and opening an
archive costs nothing that grows with the document count.

### 8A.5. Decoder Requirements

A decoder MUST validate the index before using it: *B* MUST fit in the region
(each block costs eight bytes of index and at least one stored byte), each
`stored_len` MUST lie within the bytes that remain, and the total `plain_len` is
subject to the decoder's inflation limit (14). These checks MUST compare against
the room remaining rather than add, which can overflow. A block that decompresses
to a size other than its `plain_len` MUST be rejected.

A decoder SHOULD decompress a block only when something reads inside it, and
SHOULD keep decompressed blocks for reuse. Neither is required for correctness.

---

## 9. Structure Compression

Each block is stored according to `struct_codec`:

| Codec | Stored bytes |
|-------|--------------|
| `raw` | The plain block, unchanged. |
| `lz4` | An LZ4 block of the plain block; verbatim when that would not be strictly smaller (8A.2). |
| `deflate` | A raw DEFLATE stream of the plain block. |

If the stored blocks together are not smaller than the plain structure, the
encoder stores one raw block and rewrites `struct_codec` to `raw` (5.2).
Decompression MUST yield exactly the plain block. An encoder's compressed bytes
are not part of the contract: two encoders MAY store different bytes for the
same block, provided each decompresses to the same plain block.

`lz4` is the default: a 64 KiB block decompresses in microseconds, so a cold
read costs little more than against `raw`. `deflate` is smaller and roughly an
order of magnitude slower per block. `raw` lets a reader walk the structure
without decompressing anything.

---

## 10. Encoding Procedures

### 10.1. A Single JSON Document

1. Parse the input, preserving number lexemes (G6).
2. Gather keys and string values by role; resolve `index_width` and the value
   pool's membership together (8.8); take the key width from `key_count`
   (5.5); sort both pools (7.2). The same descent can gather the document's key
   paths (7B.2).
3. Train the pool codec on both pools together and compress each pool in groups
   (7.3), storing a group's plaintext with its flag set where compression did
   not shrink it (7.5), and recording where every 64th group begins (7.4).
4. Build the key directory over every key (7A.5).
5. Serialize the root, preceded by a one-entry root offset table (4.2).
6. Cut the plain structure into blocks and apply the structure codec (8A, 9).
   Only now decide whether the key-path filter earns its bytes (7B.6), and set
   `struct_layout` bit 2 accordingly.
7. Write the header, codec state, key pool, value pool, key directory, filter
   (if any), `struct_len` and structure.

### 10.2. Newline-Delimited Input

1. Split the input into lines. Each non-empty line MUST be exactly one complete
   JSON value; a line that is not fails the encode. Empty lines are skipped and
   are not documents.
2. Gather strings across **all** documents, so the pools are shared.
3. Build the pools and codec state once.
4. Serialize each document root against those pools and place them behind a
   D-entry root offset table (4.2).
5. Continue as in steps 6–7 of 10.1.

### 10.3. Streaming Encode

For line-oriented input too large for memory, an encoder MAY make two passes:
one to collect the distinct strings, one to serialize the documents against the
pools built from them. A streaming encode MUST produce the same archive as an
in-memory encode of the same input with the same options.

### 10.4. Determinism and Concurrency

The same input and options MUST produce byte-identical archives from the same
encoder. Pool groups are independent, so an encoder MAY compress them
concurrently; it MUST then assemble the pool sections in group order, so that
the output does not depend on how many workers ran.

### 10.5. The Format Is Declared, Never Guessed

An encoder MUST NOT infer `json`, `jsonl` or `ndjson` from a file path or from
the input bytes: a pretty-printed JSON array is not JSONL, and a one-line NDJSON
file is indistinguishable from JSON. The caller declares the format.

---

## 11. Decoding Procedures

### 11.1. Opening an Archive

1. Parse the header and apply the checks of 4.1.
2. Hand the pool codec the bytes after the header and let it consume its
   state (6).
3. Locate both pool sections by their `pool_len` (7.4), key pool first, and
   check that each one's bitmap and group directory fit inside it. Decompress
   nothing.
4. Read and validate the key directory (7A.6).
5. If `struct_layout` bit 2 is set, read and validate the path filter (7B.7).
   This is not optional: it is what locates `struct_len`.
6. Read `struct_len`, then read and validate the block index (8A.5). Decompress
   nothing.

After this no pool entry has been decompressed and no value parsed.

### 11.2. Reading a Pool Entry

To read entry *i*: locate group *g* = ⌊*i*/*B*⌋ through the group directory,
walking only its segment (7.4); obtain its plaintext — the stored bytes when its
flag is set, the codec's output over them when it is clear (7.5); split the
plaintext into the entries it covers (7.6); return entry *i*. At the default
group size of 1 this is a single entry, and for a raw group a slice of the
archive.

### 11.3. Full Decode

Walk the structure and materialize every value, resolving pool references as
they are met. A single-document archive yields one JSON value; a
multi-document archive yields its roots in order, restored as newline-delimited
text under its content format.

### 11.4. Selective Access

A reader MAY materialize any subtree without its siblings, by skipping subtrees
with length prefixes, reaching the *n*th child of a container through its offset
table, comparing object key indices as integers, and decompressing a pool group
only when a comparison or an output needs a string it holds.

### 11.5. Robustness

A decoder MUST treat every length, count and offset in an archive as hostile
until validated against the bytes actually remaining. In particular, a
container's child count MUST be checked against the container's remaining bytes
before it sizes an allocation (each child costs at least one byte); an offset
table's extent MUST be validated before it is read, and each entry checked to
land inside its container; a length prefix MUST be checked for overflow and
against the enclosing value's end. A decoder MUST fail cleanly on corrupt input:
it MUST NOT crash, loop without bound, or allocate in proportion to a value it
has not validated.

---

## 12. Query Protocol

Selective access is evaluated with JSONPath [RFC9535]; this section defines the
evaluation model, not the path syntax.

### 12.1. Evaluation Model

An implementation walks the binary structure according to the path. It SHOULD
decompress a pool string only when a key comparison or an emitted value needs
it; resolve a field name through the key directory (or a binary search of the
sorted key pool) rather than decoding member keys; abandon a field lookup when
the name resolves to nothing; compare key indices as integers; use offset tables
for `$[n]` and field lookups; and skip subtrees that cannot match. None of this
is required for correctness.

### 12.2. Answer Equivalence

A selective evaluation MUST return the same values, with the same
multiplicities, as the same expression evaluated over the fully materialized
document. Where a path traverses **arrays**, matches MUST come back in the same
order. Where it enumerates **object members** — a wildcard or a descent over an
object — no order is defined, since JSON objects are unordered; two correct
evaluations MAY order such matches differently. A conformance suite MUST compare
positionally first and fall back to a multiset comparison.

### 12.3. Multi-Document Archives

A multi-document archive is queried as a **virtual root array** of `doc_count`
elements: `$[0].type` and `$[*].actor.login` are valid. There is no array tag at
the virtual root.

### 12.4. Early Exit

An implementation MAY offer bounded queries (stop after *n* matches). A bounded
query MUST stop the walk when the limit is reached, not collect everything and
truncate.

---

## 13. Round-Trip Semantics

After an encode and a decode, the restored JSON MUST be semantically equal to
the input:

1. **Numbers** are preserved as lexemes: `1e3` returns as `1e3`.
2. **Strings, booleans and `null`** are preserved exactly.
3. **Array** element order is preserved.
4. **Objects** preserve their member set and values. Member **order may
   change**: it becomes ascending key-index order, which with a sorted pool is
   lexicographic key order.
5. **Duplicate keys** collapse to at most one member.

Whitespace and insignificant formatting are not preserved. A `json` archive
restores one JSON value; a `jsonl` or `ndjson` archive restores
newline-delimited documents under the declared label.

---

## 14. Limits and Rejection

| Quantity | Limit | Past it |
|----------|-------|---------|
| `key_count`, `pool_count` | 2^32 − 1 | Encoder fails; decoder rejects. |
| Number lexeme | 255 bytes | Encoder fails; decoder rejects. |
| `index_width` | 1, 2, 4 | Decoder rejects any other. |
| `pool_group_shift` | 12 | Decoder rejects a larger value. |
| Stored `struct_len` | 2^32 − 1 | Encoder fails. |
| Plain structure offsets | 2^32 − 1 | Encoder fails (root offsets are uint32). |
| `doc_count` | ≥ 1 | Decoder rejects zero. |
| Container child count | fits the container | Decoder rejects a count the remaining bytes cannot hold. |
| Nesting depth | implementation-defined | Decoder SHOULD bound it (15.2). |

An encoder that cannot represent its input MUST fail rather than produce an
archive that decodes to something else: a number lexeme over 255 bytes, more
distinct strings than a pool count can express, a structure larger than its
offsets or `struct_len` can express, nesting deeper than the encoder's guard,
or a value outside the JSON data model.

The format sets no bound on the decompressed size of a structure, on nesting
depth, or on total memory: the right bound depends on where an archive came
from. A decoder SHOULD let its caller set them and SHOULD apply a default depth
guard.

---

## 15. Security Considerations

### 15.1. Decompression Expansion

A small stored structure can decompress to a very much larger plain structure,
and a small pool group likewise. A decoder handling untrusted archives SHOULD
bound the decompressed size of the structure before decompression completes,
and SHOULD bound the memory it commits to a pool group. The group-shift limit
bounds the number of entries a group yields, not their size.

### 15.2. Deep Nesting

Pathological nesting can exhaust a recursive decoder's stack. A decoder SHOULD
enforce a depth limit on every recursive walk — materialization, skipping and
query evaluation alike — and SHOULD apply a default.

### 15.3. Unvalidated Lengths and Counts

Counts and offsets are attacker-controlled integers. Every one MUST be validated
against the bytes actually remaining before it sizes an allocation or advances a
cursor, comparing against the room remaining rather than by addition, which can
overflow (11.5).

### 15.4. No Integrity or Authenticity

JSSC provides neither: no checksum, no signature. A corrupted archive and a
crafted one must both fail safely. An application that needs integrity MUST
wrap archives in something that provides it.

### 15.5. Codec Failures

A pool codec failing to decompress an entry indicates corrupt input and MUST be
reported as such.

### 15.6. Timing

The work a selective decoder does depends on the data and on the path it is
given. Where either is secret, query latency and decompression counts leak
information about the document's structure.

---

## 16. Conformance

**A conformant decoder** MUST:

- accept every `JSS8` archive produced by a conformant encoder, and restore it
  under Section 13;
- honour `struct_layout` bit 2 when locating `struct_len`, whether or not it
  uses the filter (5.3);
- honour the stored-raw flags (7.5);
- verify a segment's framing against the group directory before reading a
  group in it (7.4);
- verify a key-directory collision run against the key pool (7A.4);
- reject the malformed inputs of Section 14, and fail safely on arbitrary
  bytes (15).

**A conformant encoder** MUST:

- write `JSS8`, producing archives a conformant decoder restores under
  Section 13;
- sort both pools and object members (7.2, 8.5), so that its output is
  deterministic;
- size key references by the key width and value references by `index_width`
  (5.5);
- set `struct_codec`, every group's stored-raw flag, and every `lz4` block's
  lengths to describe the bytes actually stored;
- write `pool_len` and every group directory entry exactly (7.4);
- set `struct_layout` bit 2 if and only if it writes the filter;
- fail rather than misrepresent (14).

An implementation MAY support a subset of the pool codecs, and SHOULD report
whether an archive is unreadable because it is invalid or because a codec is
missing from the build.

**Checks for an independent implementation.** Each is a property, not a byte
comparison:

1. *Round trip*, including number lexemes that normalize (`1e3`), an empty
   string, a one-byte string, empty containers and deep nesting.
2. *Determinism*: the same input and options encode to identical bytes.
3. *Inline boundary*: with *w* = 1, a one-byte string value is inline and a
   two-byte one pooled; a one-byte key is pooled regardless.
4. *Index-width narrowing*: a document whose candidate string count is just
   above 256 but whose pooled count falls below it once short values are
   inlined has *w* = 1.
5. *Object order*: members come back in key order.
6. *Offset tables*: a container of eight tiny children carries none; one with a
   large payload does, at a width matching its span.
7. *Group directory*: pools of 64 and 65 groups carry zero and one entries, the
   latter equal to the offset of group 64; moving an entry by a byte makes a
   read in the segment it closes fail.
8. *Stored-raw flags*: a pool of very short strings is stored as plaintext.
9. *Key directory*: complete, agreeing with a pool search, answering a miss
   without touching the pools, settling a collision run by comparison.
10. *Path filter*: never rejects a path the document contains, an object
    wildcard included.
11. *Structure blocks*: the same document decodes identically at several block
    targets; reading one document of a many-block archive decompresses a small,
    constant number of blocks.
12. *Corruption*: truncating an archive at every offset, or flipping bits in a
    `raw`-structure archive, either fails cleanly or decodes correctly; nothing
    crashes or hangs.

Appendix B is a complete archive to check an implementation against byte for
byte.

---

## 17. Extension Points

1. **Pool codec identifiers 4–255.** A new codec needs no new archive version:
   `algo` already distinguishes it, and a decoder that does not know it already
   fails. It MUST define its `codec_state` framing and MUST be a small-string
   codec (5.1). Identifiers 1–3 are retired.
2. **Structure tags 0x0C–0xFF.** Adding one is not additive — an older decoder
   fails on an unknown tag, correctly — so a new tag requires a new archive
   version.
3. **`struct_layout` bits 3–7.** A bit that only adds information a reader may
   skip can ship without a version change. A bit that changes how existing bytes
   are interpreted, or adds bytes, cannot (bit 2 arrived with a new magic for
   this reason).
4. **New magic values.** Required for any change to the container layout, the
   pool framing or the meaning of an existing structure byte. A decoder SHOULD
   accumulate versions rather than replace them.

---

## 18. IANA Considerations

This document has no IANA actions. Archives conventionally use the file name
extension `.jssc`.

---

## 19. References

### 19.1. Normative References

- **[RFC1951]** Deutsch, P., "DEFLATE Compressed Data Format Specification
  version 1.3", RFC 1951, May 1996.
- **[RFC2119]** Bradner, S., "Key words for use in RFCs to Indicate Requirement
  Levels", BCP 14, RFC 2119, March 1997.
- **[RFC8259]** Bray, T., Ed., "The JavaScript Object Notation (JSON) Data
  Interchange Format", STD 90, RFC 8259, December 2017.
- **[LZ4]** Collet, Y., "LZ4 Block Format Description",
  <https://github.com/lz4/lz4/blob/dev/doc/lz4_Block_format.md>.
- **[FSST]** Boncz, P., Neumann, T., Leis, V., "FSST: Fast Random Access String
  Compression", PVLDB 13(11), 2020.

### 19.2. Informative References

- **[RFC5234]** Crocker, D., Ed., Overell, P., "Augmented BNF for Syntax
  Specifications: ABNF", STD 68, RFC 5234, January 2008.
- **[RFC7932]** Alakuijala, J., Szabadka, Z., "Brotli Compressed Data Format",
  RFC 7932, July 2016.
- **[RFC8878]** Collet, Y., Kucherawy, M., Ed., "Zstandard Compression and the
  'application/zstd' Media Type", RFC 8878, February 2021.
- **[RFC8949]** Bormann, C., Hoffman, P., "Concise Binary Object Representation
  (CBOR)", STD 94, RFC 8949, December 2020.
- **[RFC9535]** Gössner, S., Ed., Normington, G., Ed., Bormann, C., Ed.,
  "JSONPath: Query Expressions for JSON", RFC 9535, February 2024.

---

## Appendix A. Grammar

An ABNF-flavoured [RFC5234] summary. It is a reading aid; where it and the body
differ, the body governs.

```
archive        = %s"JSS8" algo struct-codec struct-layout width
                 content-fmt group-shift key-count pool-count doc-count
                 state key-pool-section value-pool-section key-dir
                 [path-filter] struct-len structure

; --- header -------------------------------------------------------
algo           = OCTET            ; 0 fsst (1, 2, 3 retired)
struct-codec   = OCTET            ; 0 raw, 1 deflate, 2 lz4
struct-layout  = OCTET            ; bit 0 lenprefix, bit 1 offsets,
                                  ; bit 2 pathfilter
width          = %x01 / %x02 / %x04
content-fmt    = %x00 / %x01 / %x02
group-shift    = OCTET            ; <= 12
key-count      = uint32
pool-count     = uint32
doc-count      = uint32           ; >= 1
state          = *OCTET           ; codec-defined, self-delimiting
struct-len     = uint32

; --- pools --------------------------------------------------------
key-pool-section   = pool-section ; G = ceil(key-count / 2^group-shift)
value-pool-section = pool-section ; G = ceil(pool-count / 2^group-shift)
pool-section   = pool-len raw-flags group-dir *group
pool-len       = uint32
raw-flags      = *OCTET           ; ceil(G/8) bytes, bit g per group
group-dir      = *uint32          ; max(0, ceil(G/64) - 1) entries
group          = uvarint *OCTET   ; stored length, stored bytes

; a group's stored bytes are its plaintext when its flag bit is set,
; and the pool codec's output when it is clear. Plaintext:
;   one entry      -> the entry bytes
;   several        -> *(uvarint *OCTET)

; --- key directory ------------------------------------------------
key-dir        = uint32 *key-entry
key-entry      = uint32 key-ref   ; crc32, key-pool index
; sorted by crc32; a run of equal hashes is ordered by index

; --- key-path filter (only if struct-layout bit 2) ----------------
path-filter    = uvarint OCTET *OCTET   ; m bits, k probes, m/8 bytes

; --- structure ----------------------------------------------------
structure      = block-count *block-desc *block
block-count    = uint32
block-desc     = uint32 uint32    ; stored length, plain length
block          = *OCTET           ; stored under struct-codec

; the blocks concatenated, after decompression:
plain          = *uint32 1*value  ; doc-count root offsets, then roots

value          = [ uvarint ] tagged   ; uvarint iff LayoutLenPrefix
tagged         = null / false / true / number / string
               / array / object / inline
null           = %x00
false          = %x01
true           = %x02
number         = %x03 OCTET *OCTET    ; length, ASCII lexeme
string         = %x04 value-ref
array          = %x05 uint32 [offset-desc [offset-table]] *value
object         = %x06 uint32 [offset-desc [offset-table]]
                 *(key-ref value)
inline         = (%x07 / %x08 / %x09 / %x0A / %x0B) *OCTET

value-ref      = 1*4OCTET         ; index_width bytes, little-endian
key-ref        = 1*4OCTET         ; key width bytes, little-endian
offset-desc    = %x00 / %x01 / %x02 / %x04   ; iff LayoutOffsets
offset-table   = *OCTET           ; m entries of offset-desc bytes,
                                  ; only when offset-desc is not 0

uint32         = 4OCTET           ; little-endian
uvarint        = *%x80-FF %x00-7F ; LEB128, unsigned
```

---

## Appendix B. A Worked Example

Every byte of a complete archive. The document is

```
{"id":7,"name":"Ada","tags":["x","yy"],"ok":true}
```

encoded with the FSST pool codec, a `raw` structure, the default layout (0x03)
and the default group size of 1. The archive is 161 bytes:

```
0000  4a 53 53 38 00 00 03 01 00 00 04 00 00 00 02 00
0010  00 00 01 00 00 00 01 0d 00 00 0a 14 34 01 0d 00
0020  00 00 00 00 00 00 61 64 79 41 65 67 69 6b 6d 6e
0030  6f 73 74 11 00 00 00 0f 02 69 64 04 6e 61 6d 65
0040  02 6f 6b 04 74 61 67 73 08 00 00 00 03 03 41 64
0050  61 02 79 79 04 00 00 00 06 7e 23 5e 01 26 94 bc
0060  6f 03 47 dd dc 79 02 50 67 39 bf 00 31 00 00 00
0070  01 00 00 00 25 00 00 00 25 00 00 00 04 00 00 00
0080  20 06 04 00 00 00 00 00 03 03 01 37 01 02 04 00
0090  02 01 02 03 0c 05 02 00 00 00 00 02 08 78 02 04
00a0  01
```

The pools are split by role and sorted:

```
keys:    0 "id"   1 "name"   2 "ok"   3 "tags"
values:  0 "Ada"  1 "yy"
```

`"x"` is one byte and the index width is 1, so it is written inline and never
reaches the value pool (8.6); `"yy"` is two bytes and is pooled.

**Header, 22 bytes.**

```
 off  bytes                     meaning
   0  4a 53 53 38               magic "JSS8"
   4  00                        algo = 0 (fsst)
   5  00                        struct_codec = 0 (raw)
   6  03                        struct_layout = 0x03: lenprefix + offsets,
                                  bit 2 clear (no path filter)
   7  01                        index_width = 1
   8  00                        content_fmt = 0 (json)
   9  00                        pool_group_shift = 0 (group size 1)
  10  04 00 00 00               key_count = 4 (key width 1)
  14  02 00 00 00               pool_count = 2
  18  01 00 00 00               doc_count = 1
```

**Codec state, 29 bytes (6.1).**

```
  22  01 0d 00 00 0a 14 34 01   version_word 0x0134140a00000d01:
                                  table_version 20190218, suffix_lim 0,
                                  n_symbols 13, marker 1
  30  0d 00 00 00 00 00 00 00   length_histogram: 13 symbols of length 1
  38  61 64 79 41 65 67 69 6b   symbols "a" "d" "y" "A" "e" "g" "i" "k"
  46  6d 6e 6f 73 74             symbols "m" "n" "o" "s" "t"
```

**Key pool, 4 + 17 bytes (7.4).** Four groups make one segment, so the group
directory is empty here and in the value pool.

```
  51  11 00 00 00               pool_len = 17
  55  0f                        stored_raw flags: all four groups plaintext
  56  02 69 64                  group 0: length 2, "id"
  59  04 6e 61 6d 65            group 1: length 4, "name"
  64  02 6f 6b                  group 2: length 2, "ok"
  67  04 74 61 67 73            group 3: length 4, "tags"
```

**Value pool, 4 + 8 bytes.**

```
  72  08 00 00 00               pool_len = 8
  76  03                        stored_raw flags: both groups plaintext
  77  03 41 64 61               group 0: length 3, "Ada"
  81  02 79 79                  group 1: length 2, "yy"
```

FSST encoded each of these strings to as many bytes as its plaintext, and a tie
is stored raw (7.5), so the pools read as ASCII.

**Key directory, 24 bytes (7A.2).** Sorted by hash; every run is one entry, so
every hit is exact. The last entry is the test vector of 7A.3.

```
  84  04 00 00 00               dir_count = 4
  88  06 7e 23 5e  01           hash 0x5e237e06 -> keys[1] "name"
  93  26 94 bc 6f  03           hash 0x6fbc9426 -> keys[3] "tags"
  98  47 dd dc 79  02           hash 0x79dcdd47 -> keys[2] "ok"
 103  50 67 39 bf  00           hash 0xbf396750 -> keys[0] "id"
```

**Path filter: absent.** Bit 2 is clear, so the next field is `struct_len`.

**Structure, 4 + 49 bytes (8A.2).**

```
 108  31 00 00 00               struct_len = 49
 112  01 00 00 00               block_count = 1
 116  25 00 00 00                 block 0: stored_len = 37
 120  25 00 00 00                 block 0: plain_len  = 37
 124  04 00 00 00               root_offset[0] = 4
 128  20                        payload length 32
 129  06 04 00 00 00            object, 4 children
 134  00                        offset_width = 0 (no table)
 135  00                          key -> keys[0] "id"
 136  03                          payload length 3
 137  03 01 37                    number, 1-byte lexeme "7"
 140  01                          key -> keys[1] "name"
 141  02                          payload length 2
 142  04 00                       string -> values[0] "Ada"
 144  02                          key -> keys[2] "ok"
 145  01                          payload length 1
 146  02                          true
 147  03                          key -> keys[3] "tags"
 148  0c                          payload length 12
 149  05 02 00 00 00              array, 2 children
 154  00                          offset_width = 0
 155  02                            payload length 2
 156  08 78                         inline string, 1 byte: "x"
 158  02                            payload length 2
 159  04 01                         string -> values[1] "yy"
```

Members are stored in key-index order (`id`, `name`, `ok`, `tags`), not source
order. Skipping is arithmetic: at offset 148 a reader learns that the `tags`
value is 12 bytes and steps over it without parsing it. And the archive is
larger than its 48-byte input: the header, symbol table, directory and section
lengths are fixed costs that a document this small cannot amortize.

---

## Appendix E. Earlier Versions

Seven container versions, `JSS1` through `JSS7`, preceded this one during the
format's development. They differ in framing details: pool group framing with
fixed-width lengths, no stored pool lengths, no group directory, a single shared
pool for keys and values, a structure compressed as one stream, and a key
directory that was suppressed on any hash collision. An implementation MAY read
them; the reference decoder does, and its golden fixtures cover several. Their
layouts are not part of this document, and no encoder conforming to it produces
them.
