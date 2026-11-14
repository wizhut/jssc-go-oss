// Package sdk is the supported API for JSSC — JSON String-pool Compressed.
//
// It wraps the format package with the operations most callers need:
// compress, decompress, and query. The underlying package
// github.com/wizhut/jssc-go-oss/go/jssc exposes the format internals — tags,
// layout flags, archive walking — for tools that need them.
//
// # Compressing
//
//	archive, stats, err := sdk.Compress(jsonBytes)
//	text, _, err := sdk.Decompress(archive)
//
// Compress uses FSST for the string pool and lz4 for the structure blocks
// (DefaultOptions). CompressWith takes explicit Options; CompressFile handles large
// line-oriented corpora by streaming.
//
// The input's textual form is never inferred. A single JSON value is the
// default; set Options.Format to FormatJSONL or FormatNDJSON for
// newline-delimited input, which is encoded as one multi-document archive over
// a shared string pool.
//
// # Querying
//
// The Query* functions evaluate Goessner-style JSONPath. The plain forms parse
// JSON; the QueryArchive* forms walk an archive directly, decoding only the
// pool strings a path actually needs:
//
//	names, err := sdk.QueryArchiveValues(archive, "$.users[*].name")
//
// A multi-document archive is queried as a virtual array of its documents:
// "$[0].type", "$[*].actor.login".
package sdk
