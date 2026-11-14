# JSSC for Go

The reference implementation of **JSSC** — JSON String-pool Compressed — in
pure Go (no cgo). It implements the format defined in
[`../spec/draft-jssc-00.md`](../spec/draft-jssc-00.md); where this code and that
document disagree, one of them has a bug.

Module: `github.com/wizhut/jssc-go-oss/go` · Go 1.26 or later.

## Install

```bash
go install github.com/wizhut/jssc-go-oss/go/cmd/jssc@latest
```

Or from a checkout of this directory:

```bash
make build      # bin/jssc
make demo       # a walkthrough of the command line over the test data
make example    # go run ./examples/basic
```

## Command line

```bash
jssc compress data.json                      # -> data.json.jssc
jssc compress --format ndjson events.ndjson  # one archive, one shared pool
jssc compress --structure raw data.json      # traversable without inflating anything
jssc decompress data.json.jssc               # -> data.json
jssc info data.json.jssc                     # header and section sizes; nothing decoded
jssc query '$.users[*].name' data.json.jssc
jssc query --first '$[0].actor.login' events.ndjson.jssc
jssc query --stats '$..email' data.json.jssc # what the walk decoded and skipped
jssc algorithms
```

`--format` is required for line-oriented input and is never guessed from the
file name or the bytes. `query` accepts JSON text or an archive; archives are
recognized by their magic bytes.

## Library

The `sdk` package is the supported surface:

```go
import "github.com/wizhut/jssc-go-oss/go/sdk"

archive, stats, err := sdk.Compress(jsonBytes)
text, _, err := sdk.Decompress(archive)

// Walk the archive; decode only the strings this path needs.
names, err := sdk.QueryArchiveValues(archive, "$.users[*].name")

// Reuse an open archive across queries to keep its caches warm.
a, err := sdk.OpenArchive(archive)
v, found, err := a.QueryOne("$.users[0].email")
```

Newline-delimited input becomes one archive over a shared pool; set
`Options.Format` to `sdk.FormatNDJSON` or `sdk.FormatJSONL`, and query the
documents as a virtual array: `$[0].type`, `$[*].actor.login`.

The package `jssc` exposes the format itself — options, layout flags, archive
walking, streaming encode, statistics — for tools that need to go below the
SDK.

### Untrusted input

Opening applies a nesting guard but no size ceiling. A compressed structure
block can inflate to far more than the archive, so bound it explicitly when an
archive did not come from you:

```go
a, err := jssc.OpenBytesWith(archive, jssc.Limits{
	MaxDepth:          64,
	MaxPlainStructure: 64 << 20,
	MaxPoolCount:      1 << 20,
})
```

Every error wraps one of `ErrBadMagic`, `ErrTruncated`, `ErrCorrupt`,
`ErrUnsupported`, `ErrInvalidInput` or `ErrLimitExceeded`, so callers can
classify failures with `errors.Is`. The decoder is fuzzed (`make fuzz-open`):
arbitrary bytes fail cleanly rather than panic, hang or over-allocate.

## Layout

| Path | Role |
|------|------|
| `jssc/` | the format: header, pools, key directory, path filter, structure blocks, encode, decode, selective query |
| `codec/` | the pool codec registry (`fsst`) |
| `fsst/` | Fast Static Symbol Table, the short-string codec |
| `sdk/` | the supported API |
| `cmd/jssc/` | the command line |
| `examples/basic/` | a runnable walkthrough of the SDK |

## Format in brief

An archive is a header, the pool codec's state, the key pool, the value pool,
the key hash directory, an optional key-path filter, and the structure:

```
"JSS8" algo struct_codec struct_layout index_width content_fmt
       pool_group_shift key_count pool_count doc_count
codec_state  key_pool  value_pool  key_directory  [path_filter]
struct_len  block_index  structure_blocks
```

- **Pools.** Keys and string values are pooled by role, each pool sorted and
  stored once per distinct string, every entry compressed on its own with
  FSST. Each pool section carries its length, a bit per group saying whether it
  was stored plain, and a directory every 64 groups, so opening reads nothing
  from the pools and the first string read walks one 64-group segment.
- **Structure.** One-byte tags and pool references; a string value no longer
  than the reference width is written inline. Every value carries a length
  prefix, and large containers an offset table, so a walk skips what it does
  not want. Object members are sorted by key index, so a field lookup is an
  integer binary search. Numbers are stored as their original text.
- **Blocks.** The structure is cut into independently compressed blocks of
  about 64 KiB (lz4 by default, or DEFLATE, or raw), so a cold read
  decompresses the block it lands in.
- **Directories.** A CRC-32 key directory resolves a field name to its index
  without touching the pool, and proves a missing field absent; an optional
  Bloom filter over key paths rejects absent paths without walking at all.

## Round-trip guarantees

Encode then decode returns a semantically equal value: number lexemes exactly,
strings, booleans and `null` exactly, array order preserved. Object member order
becomes key order, insignificant whitespace is not kept, and duplicate keys
collapse to one member.

## Testing

```bash
make test        # the suite
make race
make cover
make fuzz        # round trip: valid JSON in, the same value out
make fuzz-open   # decoder: arbitrary bytes must fail cleanly
```

`jssc/spec_example_test.go` holds the encoder to the worked example of the
specification (Appendix B) byte for byte. Past fuzzer findings are kept as
regression inputs (in `jssc/testdata/fuzz/`).

## Codecs

| Pool codec | ID | |
|---|---|---|
| `fsst` | 0 | the default and only codec |

It compresses one pool entry at a time, which is what lets a query decode the
single string it reached. IDs 1, 2 and 3 are retired and never reissued: a
decoder names the retired codec an archive wants rather than misreading it.

The structure is stored under `lz4` (LZ4 block format, the default), `deflate`
(raw DEFLATE, RFC 1951) or `raw`, independently of the pool codec. Under lz4 an
incompressible block is stored verbatim; the encoder falls back to `raw` for
the whole structure whenever compression does not shrink it, so the header
always describes the bytes that follow.
