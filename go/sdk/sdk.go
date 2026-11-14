package sdk

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wizhut/jssc-go-oss/go/codec"
	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// Version is the SDK release version.
const Version = "1.0.0"

// SpecVersion is the JSSC specification revision this SDK implements.
const SpecVersion = jssc.SpecVersion

// LatestVersion is the archive version Compress writes.
const LatestVersion = jssc.LatestVersion

// ArchiveSuffix is the conventional file extension for a JSSC archive.
const ArchiveSuffix = ".jssc"

// Pool algorithm names accepted by Options.Algorithm. Both are small-string
// codecs: they compress one pool entry at a time, so a query decompresses only
// the string it reached.
const (
	// AlgoFSST is the default and the sole registered codec. It compresses
	// each entry against a symbol table learned from the pool and stored once
	// in the archive. Pure Go.
	AlgoFSST = "fsst"
)

// Structure selects how the binary structure block is stored inside an
// archive, independently of the pool algorithm.
type Structure = jssc.StructCodec

const (
	// StructureRaw stores the structure verbatim, so a reader traverses it
	// without inflating anything. Prefer it when an outer compressor will be
	// applied anyway, or when selective access latency matters above all.
	StructureRaw = jssc.StructRaw

	// StructureDeflate stores the structure as a raw DEFLATE stream — the
	// smallest of the three and the slowest to enter. Prefer it for cold
	// storage and transmission.
	StructureDeflate = jssc.StructDeflate

	// StructureLZ4 stores each structure block as an LZ4 block, or verbatim
	// where compression does not pay. This is the default: most of deflate's
	// ratio, at an order of magnitude less time to decompress a block on a
	// cold read.
	StructureLZ4 = jssc.StructLZ4
)

// Format is the textual document form for Compress and Decompress.
type Format = jssc.ContentFormat

const (
	FormatJSON   = jssc.FormatJSON
	FormatJSONL  = jssc.FormatJSONL
	FormatNDJSON = jssc.FormatNDJSON
)

// Stats reports what an encode or decode produced.
type Stats = jssc.Stats

// Options configures compression. The zero value is valid but stores the
// structure uncompressed; DefaultOptions is the recommended starting point.
type Options struct {
	// Algorithm is the string-pool codec name. Empty selects AlgoFSST.
	Algorithm string

	// Structure is the structure-block codec. The zero value is StructureRaw.
	Structure Structure

	// Layout selects the structure walk aids written into the archive. Zero
	// uses jssc.LayoutDefault: length prefixes plus child offset tables.
	Layout jssc.Layout

	// Format declares the input's textual form. Zero means a single JSON
	// document. Set FormatJSONL or FormatNDJSON for newline-delimited input;
	// this is never inferred from a filename or from the bytes.
	Format Format

	// MaxDirectBytes is the largest line-oriented file CompressFile will load
	// entirely into memory. Zero selects jssc.DefaultMaxDirectBytes (64 MiB);
	// negative forces the two-pass streaming encoder.
	MaxDirectBytes int64

	// MaxDocs, when positive, encodes only the first MaxDocs lines of
	// line-oriented input.
	MaxDocs int

	// PoolGroupShift sets the pool group size to 1<<PoolGroupShift entries.
	// Zero selects the format default, which is one entry per group. A group is
	// the unit a reader must decompress to reach any string inside it, and the
	// pool algorithms compress an entry at a time, so raising this makes cold
	// reads touch neighbours without buying compression in return.
	PoolGroupShift uint8

	// PerEntryPool forces a group size of 1, so decoding one pool entry touches
	// no other. That is the default; set it to say so explicitly.
	PerEntryPool bool

	// Legacy4 writes the older JSS3/JSS4 encodings instead of JSS5.
	Legacy4 bool

	// Version pins the archive version. Zero selects LatestVersion. Use it to
	// write an archive an older decoder can read.
	Version int

	// StructBlockBytes is the target plain size of one JSS7 structure block, the
	// unit a reader inflates to reach any document inside it. Zero selects the
	// format default; negative stores the structure as a single block, which is
	// what JSS6 and earlier did and what makes a cold read cost the archive.
	StructBlockBytes int

	// OmitKeyDirectory suppresses the JSS6 key hash directory, the table that
	// lets a field lookup resolve its key without decompressing any pool group.
	// The section is still written, empty, so the archive stays valid.
	OmitKeyDirectory bool

	// Concurrency bounds the goroutines the encoder uses to compress pool
	// groups. Zero selects GOMAXPROCS; 1 encodes sequentially, which is what a
	// server already running many concurrent compressions should set. The
	// archive is byte-identical either way.
	Concurrency int
}

// DefaultOptions returns the recommended settings: an FSST string pool and
// lz4 structure blocks, which keep the archive small while a cold read
// decompresses one block rather than the structure.
func DefaultOptions() Options {
	return Options{
		Algorithm: AlgoFSST,
		Structure: StructureLZ4,
	}
}

func (o Options) toJSSC() jssc.Options {
	return jssc.Options{
		Algorithm:      o.Algorithm,
		Structure:      o.Structure,
		Layout:         o.Layout,
		Format:         o.Format,
		MaxDirectBytes: o.MaxDirectBytes,
		MaxDocs:        o.MaxDocs,
		PoolGroupShift: o.PoolGroupShift,
		PerEntryPool:   o.PerEntryPool,
		Legacy4:        o.Legacy4,
		Version:        o.Version,

		OmitKeyDirectory: o.OmitKeyDirectory,
		StructBlockBytes: o.StructBlockBytes,
		Concurrency:      o.Concurrency,
	}
}

// ParseFormat resolves "json", "jsonl", or "ndjson". Empty selects FormatJSON.
func ParseFormat(name string) (Format, error) {
	return jssc.ParseContentFormat(name)
}

// ParseStructure resolves "deflate" or "raw". Empty selects deflate.
func ParseStructure(name string) (Structure, error) {
	return jssc.ParseStructCodec(name)
}

// AlgorithmInfo describes a registered pool codec.
type AlgorithmInfo struct {
	ID          uint8
	Name        string
	Description string
	// Available reports whether this build can use the codec. Every
	// registered codec is currently pure Go and always available; the field
	// stays because listings render it.
	Available bool
	Default   bool
}

// Algorithms returns the registered string-pool codecs.
func Algorithms() []AlgorithmInfo {
	list := codec.List()
	out := make([]AlgorithmInfo, len(list))
	for i, e := range list {
		out[i] = AlgorithmInfo{
			ID:          uint8(e.ID),
			Name:        e.Name,
			Description: e.Description,
			Available:   e.Available,
			Default:     e.Name == codec.DefaultName,
		}
	}
	return out
}

// StructureInfo describes a structure-block codec.
type StructureInfo struct {
	ID          uint8
	Name        string
	Default     bool
	Description string
}

// Structures returns the structure-block codecs an archive header can name.
func Structures() []StructureInfo {
	return []StructureInfo{
		{
			ID:          uint8(StructureRaw),
			Name:        StructureRaw.String(),
			Description: "store the structure verbatim (in-place traversal, no inflate)",
		},
		{
			ID:          uint8(StructureDeflate),
			Name:        StructureDeflate.String(),
			Default:     true,
			Description: "store the structure as a raw DEFLATE stream (default)",
		},
	}
}

// Compress encodes JSON bytes into an archive using DefaultOptions.
func Compress(data []byte) ([]byte, *Stats, error) {
	return CompressWith(data, DefaultOptions())
}

// CompressWith encodes bytes into an archive with explicit options.
func CompressWith(data []byte, opt Options) ([]byte, *Stats, error) {
	var buf bytes.Buffer
	st, err := jssc.EncodeWith(data, &buf, opt.toJSSC())
	if err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), st, nil
}

// CompressTo encodes data and writes the archive to w.
func CompressTo(data []byte, w io.Writer, opt Options) (*Stats, error) {
	return jssc.EncodeWith(data, w, opt.toJSSC())
}

// CompressStream encodes everything readable from r and writes the archive to
// w. Line-oriented input above Options.MaxDirectBytes is encoded in two
// streaming passes.
func CompressStream(r io.Reader, w io.Writer, opt Options) (*Stats, error) {
	return jssc.EncodeFrom(r, w, opt.toJSSC())
}

// Decompress restores the text of an archive: JSON, JSONL, or NDJSON according
// to the archive's content format.
func Decompress(archive []byte) ([]byte, *Stats, error) {
	return jssc.DecodeBytes(archive)
}

// DecompressFrom restores the text of an archive read from r.
func DecompressFrom(r io.Reader) ([]byte, *Stats, error) {
	return jssc.Decode(r)
}

// Inspect reports an archive's header and framing without decoding its pool or
// walking its structure.
func Inspect(archive []byte) (*Stats, error) {
	return jssc.Inspect(archive)
}

// CompressFile compresses path and writes path+".jssc", returning the output
// path and encode stats. Set opt.Format for line-oriented input: the extension
// is not inspected.
//
// Line-oriented files larger than opt.MaxDirectBytes are streamed in two
// passes, so peak memory tracks the unique-string set rather than the corpus.
func CompressFile(path string, opt Options) (outPath string, st *Stats, err error) {
	outPath = path + ArchiveSuffix
	out, err := os.Create(outPath)
	if err != nil {
		return "", nil, fmt.Errorf("create %s: %w", outPath, err)
	}
	st, err = jssc.EncodeFile(path, out, opt.toJSSC())
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(outPath)
		return "", nil, err
	}
	return outPath, st, nil
}

// DecompressFile restores a .jssc archive to the same path with the suffix
// removed. When the stripped path carries no matching extension, the archive's
// own content format supplies one.
func DecompressFile(path string) (outPath string, st *Stats, err error) {
	if !strings.HasSuffix(path, ArchiveSuffix) {
		return "", nil, fmt.Errorf("input must end with %s: %s", ArchiveSuffix, path)
	}
	in, err := os.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer in.Close()

	text, st, err := DecompressFrom(in)
	if err != nil {
		return "", nil, err
	}

	outPath = strings.TrimSuffix(path, ArchiveSuffix)
	if st.ContentFormat.LineOriented() && !hasLineExtension(outPath) {
		outPath += st.ContentFormat.Extension()
	}
	if err := os.WriteFile(outPath, text, 0o644); err != nil {
		return "", nil, fmt.Errorf("write %s: %w", outPath, err)
	}
	return outPath, st, nil
}

func hasLineExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl", ".ndjson":
		return true
	default:
		return false
	}
}

// Ratio returns original size over archive size, or 0 when unknown.
func Ratio(st *Stats) float64 { return st.Ratio() }
