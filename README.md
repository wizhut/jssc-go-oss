# JSSC — JSON String-pool Compressed

JSSC is a binary archive format for JSON that you can **query without
decompressing**. Every distinct string goes into a pool, compressed one entry
at a time; the value tree becomes a compact structure of tags and pool
references, with length prefixes and offset tables so a reader can skip what
it does not need. A JSONPath query walks the archive and decompresses only the
strings it actually reaches.

```
$ jssc compress users.json
$ jssc query --stats '$[*].name' users.json.jssc
$[0].name	"Alice"
…
8 match(es); decoded 8 of 18 pool strings, skipped 40 values
```

This repository holds the format's specification and its Go implementation.

| Path | What it is |
|------|------------|
| [`spec/draft-jssc-00.md`](spec/draft-jssc-00.md) | The specification of format version **JSS8**, written as an RFC-style draft |
| [`go/`](go/) | The Go reference implementation: format package, SDK, `jssc` command line |

## Quick start

Command line:

```bash
go install github.com/wizhut/jssc-go-oss/go/cmd/jssc@latest

jssc compress data.json                      # -> data.json.jssc
jssc query '$.users[*].name' data.json.jssc  # walk the archive, decode only what matches
jssc decompress data.json.jssc               # -> data.json
```

Library:

```go
import "github.com/wizhut/jssc-go-oss/go/sdk"

archive, stats, err := sdk.Compress(jsonBytes)
names, err := sdk.QueryArchiveValues(archive, "$.users[*].name")
text, _, err := sdk.Decompress(archive)
```

See [`go/README.md`](go/README.md) for the full API, the command line and the
package layout, and [`go/examples/basic`](go/examples/basic) for a runnable
program.

## When it is a good fit — and when it is not

JSSC trades compression ratio for random access: each string is compressed on
its own so it can be read on its own.

- **Good fit:** large JSON documents, held locally or in a database, that are
  read a few fields at a time. Reading one value out of an archive costs
  microseconds and touches a small, constant part of it, where compressed JSON
  has to be decompressed and parsed in full first.
- **Not a fit:** shrinking JSON for transfer or archival. On most documents
  gzip, zstd or brotli over the JSON text is smaller than a JSSC archive, often
  by 1.5–3x, and faster to produce. Small documents (a few KB) carry fixed
  overheads they cannot amortize.

Round trips keep every value, number lexemes included (`1e3` stays `1e3`);
object members come back in key order, and insignificant whitespace is not
kept.

## Status

The format and this implementation are a **draft** (format `JSS8`,
specification revision 5.6.0-draft). Until a 1.0 release the wire format may
still change, including within `JSS8`, so do not use it for archives that must
outlive the version that wrote them. The Go implementation also reads the
earlier development versions `JSS1`–`JSS7`, and writes only `JSS8`.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
