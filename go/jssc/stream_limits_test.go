package jssc

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// A root offset is a uint32 (specification §4.2), and an encoder that cannot
// represent its input must fail rather than write an archive that opens the
// wrong bytes (§14). The streaming encoder derives the offsets from
// per-document lengths, so the bound has to hold there as it does in the
// in-memory path. A real 4 GiB structure is too large for a test, but the
// table is computed from the lengths alone, and lengths can claim one.
func TestStreamingRootOffsetsStopAtUint32(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch) // where concatOffsetsAndBody creates its file

	body := filepath.Join(t.TempDir(), "body")
	if err := os.WriteFile(body, []byte{0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}

	// Two documents: the table is 8 bytes, so the second root starts at
	// 8 + the first length. Here it lands exactly on the largest offset.
	path, size, err := concatOffsetsAndBody(body, []uint32{math.MaxUint32 - 8, 2})
	if err != nil {
		t.Fatalf("offset at math.MaxUint32: %v", err)
	}
	plain, err := os.ReadFile(path)
	os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	if size != 10 || len(plain) != 10 {
		t.Fatalf("plain is %d bytes (reported %d), want 10", len(plain), size)
	}
	if got := binary.LittleEndian.Uint32(plain[0:4]); got != 8 {
		t.Errorf("root_offset[0] = %d, want 8", got)
	}
	if got := binary.LittleEndian.Uint32(plain[4:8]); got != math.MaxUint32 {
		t.Errorf("root_offset[1] = %d, want %d", got, uint32(math.MaxUint32))
	}

	// One byte more and the second root starts at 2^32, which would have been
	// written as 0.
	over := []uint32{math.MaxUint32 - 7, 2}
	if _, _, err := concatOffsetsAndBody(body, over); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("offset at 2^32: error = %v, want ErrLimitExceeded", err)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("a failed table left %d file(s) behind: %v", len(left), left)
	}
}
