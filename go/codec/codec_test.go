package codec_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wizhut/jssc-go-oss/go/codec"
)

func TestRegistry(t *testing.T) {
	list := codec.List()
	if len(list) != 1 {
		t.Fatalf("registry holds %d codecs, want exactly fsst", len(list))
	}
	for i, info := range list {
		if int(info.ID) != i {
			t.Errorf("codec %q has id %d at position %d; the registry must be ordered by id",
				info.Name, info.ID, i)
		}
		if info.Name == "" || info.Description == "" {
			t.Errorf("codec %d is missing a name or description", info.ID)
		}
	}
	if codec.DefaultName != "fsst" {
		t.Errorf("DefaultName = %q, want fsst", codec.DefaultName)
	}
	if codec.PerEntryName != "fsst" {
		t.Errorf("PerEntryName = %q, want fsst", codec.PerEntryName)
	}
}

// TestRetiredIDsStayUnregistered guards the ids that held removed codecs:
// unishox (1, retired on measurement) and the group-wide deflate and lz4
// (2 and 3). Reissuing any of them would make an archive written by an older
// encoder decode as something else instead of failing.
//
// The specification also requires the failure to be distinguishable from an
// unassigned identifier where a decoder can manage it (§5.1): such an archive
// was conformant when written, and naming the codec it wants is more useful
// than calling the byte unknown.
func TestRetiredIDsStayUnregistered(t *testing.T) {
	for _, id := range []codec.ID{1, 2, 3} {
		_, err := codec.NewByID(id)
		if err == nil {
			t.Errorf("codec id %d is registered again; it held a removed codec", id)
			continue
		}
		if !strings.Contains(err.Error(), "retired") {
			t.Errorf("codec id %d failed with %q; the error must distinguish a retired id "+
				"from an unassigned one", id, err)
		}
	}

	// An id that never held anything must still read as simply unknown.
	if _, err := codec.NewByID(200); err == nil || strings.Contains(err.Error(), "retired") {
		t.Errorf("codec id 200 was never assigned; got %v", err)
	}
}

func TestNewByNameAndID(t *testing.T) {
	byName, err := codec.NewByName("")
	if err != nil {
		t.Fatalf("the empty name must select the default: %v", err)
	}
	if byName.Name() != codec.DefaultName {
		t.Errorf("empty name gave %q, want %q", byName.Name(), codec.DefaultName)
	}

	byID, err := codec.NewByID(codec.IDFSST)
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID() != byName.ID() {
		t.Errorf("NewByID and NewByName disagree: %d vs %d", byID.ID(), byName.ID())
	}

	if _, err := codec.NewByName("  FSST  "); err != nil {
		t.Errorf("names should be case- and space-insensitive: %v", err)
	}
	if _, err := codec.NewByName("nope"); err == nil {
		t.Error("expected an error for an unregistered name")
	}
	if _, err := codec.NewByID(200); err == nil {
		t.Error("expected an error for an unregistered id")
	}
}

func TestUnavailableCodecExplainsItself(t *testing.T) {
	for _, info := range codec.List() {
		if info.Available {
			continue
		}
		_, err := codec.NewByID(info.ID)
		if err == nil {
			t.Errorf("codec %q is marked unavailable but instantiated fine", info.Name)
			continue
		}
		// The message has to tell someone how to get the codec back.
		if !strings.Contains(err.Error(), info.Name) {
			t.Errorf("error for %q does not name the codec: %v", info.Name, err)
		}
	}
}

func TestFSSTCodecRoundTrip(t *testing.T) {
	c, err := codec.NewByName("fsst")
	if err != nil {
		t.Fatal(err)
	}
	entries := [][]byte{
		[]byte("alice@example.com"),
		[]byte("bob@example.com"),
		[]byte(""),
		[]byte(strings.Repeat("repetition ", 40)),
		[]byte("αβγ 漢字 🎉"),
	}
	if err := c.Train(entries); err != nil {
		t.Fatal(err)
	}

	encoded := make([][]byte, len(entries))
	for i, e := range entries {
		enc, err := c.Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		encoded[i] = enc
	}

	// State written by the encoder must be all the decoder needs.
	var state bytes.Buffer
	if err := c.WriteState(&state); err != nil {
		t.Fatal(err)
	}
	reader, err := codec.NewByID(c.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ReadState(bytes.NewReader(state.Bytes())); err != nil {
		t.Fatal(err)
	}
	for i, enc := range encoded {
		got, err := reader.Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, entries[i]) {
			t.Errorf("entry %d: got %q, want %q", i, got, entries[i])
		}
	}
}

func TestFSSTRejectsUseBeforeTraining(t *testing.T) {
	c, err := codec.NewByName("fsst")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Encode([]byte("x")); err == nil {
		t.Error("Encode before Train should fail")
	}
	if err := c.WriteState(&bytes.Buffer{}); err == nil {
		t.Error("WriteState before Train should fail")
	}
}

func TestFSSTTrainsOnEmptyPool(t *testing.T) {
	c, err := codec.NewByName("fsst")
	if err != nil {
		t.Fatal(err)
	}
	// An archive whose documents hold no pooled strings still needs a table.
	if err := c.Train(nil); err != nil {
		t.Fatalf("training on an empty pool must succeed: %v", err)
	}
	var state bytes.Buffer
	if err := c.WriteState(&state); err != nil {
		t.Fatal(err)
	}
	if state.Len() == 0 {
		t.Error("fsst wrote no state; the decoder needs a table")
	}
}
