package codec_test

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

// The pool codecs are compared here on the one operation a selective read
// actually performs: decompressing a single group. An end-to-end query mixes
// that with the walk and the open, and the walk is identical whichever codec
// wrote the archive — so the isolated number is the one that says which codec
// is faster at its job.
//
// A group now holds one entry, which is the shape oneShort measures. The
// 64-entry shapes are kept because a caller may still set PoolGroupShift above
// zero, and because they are what shows that a small-string codec neither gains
// nor loses much from the neighbours it is handed.

// groupEntries is a deliberately non-default group size: DefaultPoolGroupShift
// is 0 now, and these benchmarks want a multi-entry group to exist at all.
const groupEntries = 64

// oneShort is a single pool entry as a group of one — the default shape, and
// the only one a lazy per-entry read ever decompresses. A group of one carries
// no length prefix: its plaintext is the entry bytes and nothing else.
func oneShort() []byte {
	return []byte("info/gateway/0000002a")
}

func shortGroup() []byte {
	rng := rand.New(rand.NewPCG(42, 7))
	levels := []string{"debug", "info", "warn", "error"}
	svcs := []string{"auth", "billing", "search", "gateway", "worker"}
	out := make([]string, 0, groupEntries)
	for i := range groupEntries {
		out = append(out, fmt.Sprintf("%s/%s/%08x", levels[rng.IntN(4)], svcs[rng.IntN(5)], i))
	}
	return frame(out)
}

func proseGroup() []byte {
	rng := rand.New(rand.NewPCG(7, 42))
	words := strings.Fields("the quick brown fox jumps over a lazy dog while the " +
		"gateway service records every request it forwards to the billing worker")
	out := make([]string, 0, groupEntries)
	for range groupEntries {
		var b strings.Builder
		for range 40 {
			b.WriteString(words[rng.IntN(len(words))])
			b.WriteByte(' ')
		}
		out = append(out, b.String())
	}
	return frame(out)
}

// frame lays entries out as a group's plaintext: uvarint length, then bytes.
func frame(entries []string) []byte {
	var out []byte
	for _, s := range entries {
		out = binary.AppendUvarint(out, uint64(len(s)))
		out = append(out, s...)
	}
	return out
}

func compressed(tb testing.TB, name string, plain []byte) ([]byte, codec.Codec) {
	tb.Helper()
	c, err := codec.NewByName(name)
	if err != nil {
		tb.Fatal(err)
	}
	if err := c.Train([][]byte{plain}); err != nil {
		tb.Fatal(err)
	}
	enc, err := c.Encode(plain)
	if err != nil {
		tb.Fatal(err)
	}
	return enc, c
}

func benchDecode(b *testing.B, name string, plain []byte) {
	enc, c := compressed(b, name, plain)
	b.ReportMetric(float64(len(enc)), "stored_B")
	b.ReportMetric(float64(len(plain))/float64(len(enc)), "ratio")
	b.SetBytes(int64(len(plain))) // throughput in plaintext produced
	b.ResetTimer()
	for b.Loop() {
		if _, err := c.Decode(enc); err != nil {
			b.Fatal(err)
		}
	}
}

func benchEncode(b *testing.B, name string, plain []byte) {
	_, c := compressed(b, name, plain)
	b.SetBytes(int64(len(plain)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := c.Encode(plain); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeShortFSST(b *testing.B) { benchDecode(b, "fsst", shortGroup()) }
func BenchmarkDecodeProseFSST(b *testing.B) { benchDecode(b, "fsst", proseGroup()) }

func BenchmarkEncodeShortFSST(b *testing.B) { benchEncode(b, "fsst", shortGroup()) }
func BenchmarkEncodeProseFSST(b *testing.B) { benchEncode(b, "fsst", proseGroup()) }

// The single-entry benchmarks are what the pool actually asks a codec to do now
// that a group holds one entry: compress a key or a short string value on its
// own, and decode it without touching a neighbour.
func BenchmarkDecodeOneShortFSST(b *testing.B) { benchDecode(b, "fsst", oneShort()) }
func BenchmarkEncodeOneShortFSST(b *testing.B) { benchEncode(b, "fsst", oneShort()) }
