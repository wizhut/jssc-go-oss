// Package fsst implements Fast Static Symbol Table compression, the default
// JSSC string-pool codec (algo 0).
//
// FSST learns up to 255 symbols of 1–8 bytes from training data and encodes a
// string as a mix of one-byte symbol codes and escaped literals. Crucially for
// JSSC, each string decodes on its own: there is no block framing and no
// dependency between pool entries.
//
// The serialized table is the archive's codec_state, and its wire form is
// normative (specification §6.1):
//
//	uint64 LE  version_word     // tableVersion<<32 | suffixLim<<16 | nSymbols<<8 | 1
//	uint8[8]   length_histogram // symbol counts for lengths 1..8
//	bytes      symbols          // bodies, grouped by length 2,3,…,8, then 1
//
// Based on Boncz, Neumann, Leis, "FSST: Fast Random Access String Compression"
// (VLDB 2020), https://github.com/cwida/fsst.
//
// Usage:
//
//	tbl := fsst.Train([][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}`)})
//	out := tbl.Encode([]byte(`{"id":3}`))
//	raw := tbl.DecodeAll(out)
package fsst
