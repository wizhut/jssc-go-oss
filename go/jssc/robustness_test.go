package jssc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/jssc"
)

// A decoder that reads untrusted archives must fail rather than panic, loop, or
// allocate without bound. These tests corrupt otherwise valid archives and
// insist on an error.

func TestOpenRejectsBadMagic(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"empty", nil, jssc.ErrTruncated},
		{"short", []byte("JS"), jssc.ErrTruncated},
		{"wrong magic", []byte("XXXXxxxxxxxxxxxx"), jssc.ErrBadMagic},
		{"future magic", []byte("JSS9xxxxxxxxxxxx"), jssc.ErrBadMagic},
		{"magic only", []byte("JSS8"), jssc.ErrTruncated},
		{"earlier version", []byte("JSS3xxxxxxxxxxxx"), jssc.ErrUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jssc.OpenBytes(tc.raw)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenRejectsBadHeaderFields(t *testing.T) {
	archive, _ := encode(t, []byte(`{"key":"value"}`), jssc.Options{})

	t.Run("index width", func(t *testing.T) {
		bad := bytes.Clone(archive)
		bad[7] = 3
		if _, err := jssc.OpenBytes(bad); !errors.Is(err, jssc.ErrUnsupported) {
			t.Errorf("error = %v, want ErrUnsupported", err)
		}
	})
	t.Run("structure codec", func(t *testing.T) {
		bad := bytes.Clone(archive)
		bad[5] = 9
		if _, err := jssc.OpenBytes(bad); !errors.Is(err, jssc.ErrUnsupported) {
			t.Errorf("error = %v, want ErrUnsupported", err)
		}
	})
	t.Run("pool codec", func(t *testing.T) {
		bad := bytes.Clone(archive)
		bad[4] = 200
		if _, err := jssc.OpenBytes(bad); !errors.Is(err, jssc.ErrUnsupported) {
			t.Errorf("error = %v, want ErrUnsupported", err)
		}
	})
	t.Run("pool count", func(t *testing.T) {
		bad := bytes.Clone(archive)
		binary.LittleEndian.PutUint32(bad[8:], 1<<30)
		if _, err := jssc.OpenBytes(bad); err == nil {
			t.Error("expected an error for an impossible pool count")
		}
	})
	t.Run("reserved layout bits are ignored", func(t *testing.T) {
		relaxed := bytes.Clone(archive)
		// Bit 2 is LayoutPathFilter from JSS8 on, so it is no longer reserved
		// and setting it means something: the reader would go looking for a
		// section that is not there. Bits 3-6 are what remains reserved.
		relaxed[6] |= 0x78
		a, err := jssc.OpenBytes(relaxed)
		if err != nil {
			t.Fatalf("reserved bits should be ignored on read: %v", err)
		}
		if _, err := a.DecodeAll(); err != nil {
			t.Errorf("decode after ignoring reserved bits: %v", err)
		}
	})
}

func TestOpenRejectsTruncation(t *testing.T) {
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := encode(t, raw, jssc.Options{Structure: jssc.StructRaw})

	for cut := 1; cut < len(archive); cut += max(1, len(archive)/37) {
		short := archive[:cut]
		a, err := jssc.OpenBytes(short)
		if err != nil {
			continue // rejected at open, which is the usual case
		}
		// Anything that opens must still fail, not panic, when read.
		if _, err := a.DecodeAll(); err == nil && cut != len(archive) {
			t.Errorf("archive truncated to %d of %d bytes decoded without error", cut, len(archive))
		}
	}
}

func TestCorruptStructureDoesNotPanic(t *testing.T) {
	raw, err := os.ReadFile("testdata/users.json")
	if err != nil {
		t.Fatal(err)
	}
	// Raw structure storage puts the tags where a fuzzer can reach them.
	archive, _ := encode(t, raw, jssc.Options{Structure: jssc.StructRaw})

	rng := rand.New(rand.NewPCG(1, 2))
	for range 400 {
		bad := bytes.Clone(archive)
		// Leave the magic alone so we exercise the body, not the header check.
		i := 4 + rng.IntN(len(bad)-4)
		bad[i] ^= byte(1 << rng.IntN(8))

		a, err := jssc.OpenBytes(bad)
		if err != nil {
			continue
		}
		_, _ = a.DecodeAll()
		_, _ = a.Query("$[*].name")
		_, _ = a.Query("$..role")
		_, _, _ = a.QueryOne("$[3].email")
	}
}

func TestLimitsRejectDeepNesting(t *testing.T) {
	deep := []byte(strings.Repeat(`{"a":`, 200) + "1" + strings.Repeat("}", 200))
	archive, _ := encode(t, deep, jssc.Options{})

	a, err := jssc.OpenBytesWith(archive, jssc.Limits{MaxDepth: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DecodeAll(); !errors.Is(err, jssc.ErrLimitExceeded) {
		t.Errorf("error = %v, want ErrLimitExceeded", err)
	}

	unbounded, err := jssc.OpenBytes(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unbounded.DecodeAll(); err != nil {
		t.Errorf("the default depth limit should accept 200 levels: %v", err)
	}
}

func TestLimitsRejectStructureExpansion(t *testing.T) {
	// Highly repetitive input, so the structure really does deflate.
	var b bytes.Buffer
	b.WriteByte('[')
	for i := range 5000 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"key":"a repeated value that compresses very well indeed"}`)
	}
	b.WriteByte(']')

	archive, st := encode(t, b.Bytes(), jssc.Options{Structure: jssc.StructDeflate})
	if st.StructCodec != jssc.StructDeflate {
		t.Skip("structure did not deflate; nothing to bound")
	}

	_, err := jssc.OpenBytesWith(archive, jssc.Limits{MaxPlainStructure: int64(st.StructBytes)})
	if !errors.Is(err, jssc.ErrLimitExceeded) {
		t.Errorf("error = %v, want ErrLimitExceeded", err)
	}
	if _, err := jssc.OpenBytesWith(archive, jssc.Limits{MaxPlainStructure: int64(st.StructPlain)}); err != nil {
		t.Errorf("a limit at the true plain size should pass: %v", err)
	}
}

func TestLimitsRejectPoolCount(t *testing.T) {
	archive, st := encode(t, []byte(`{"alpha":"one value","beta":"another value"}`), jssc.Options{})
	_, err := jssc.OpenBytesWith(archive, jssc.Limits{MaxPoolCount: st.UniqueStrings - 1})
	if !errors.Is(err, jssc.ErrLimitExceeded) {
		t.Errorf("error = %v, want ErrLimitExceeded", err)
	}
}

func TestStringIndexOutOfRange(t *testing.T) {
	a, _ := openTestArchive(t, "testdata/users.json", jssc.Options{})
	if _, err := a.String(-1); !errors.Is(err, jssc.ErrCorrupt) {
		t.Errorf("String(-1) error = %v, want ErrCorrupt", err)
	}
	if _, err := a.String(a.PoolCount()); !errors.Is(err, jssc.ErrCorrupt) {
		t.Errorf("String(PoolCount) error = %v, want ErrCorrupt", err)
	}
}
