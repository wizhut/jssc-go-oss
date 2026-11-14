package codec

import (
	"fmt"
	"io"

	"github.com/wizhut/jssc-go-oss/go/fsst"
)

// fsstCodec is pool codec 0: a symbol table learned from the pool, persisted
// as codec state so every entry decodes independently.
type fsstCodec struct {
	table *fsst.Table
}

func newFSST() Codec { return &fsstCodec{} }

func (c *fsstCodec) ID() ID       { return IDFSST }
func (c *fsstCodec) Name() string { return "fsst" }

func (c *fsstCodec) Description() string {
	return "Fast Static Symbol Table (learned symbols, good for repetitive JSON)"
}

// Train learns a symbol table. An empty sample set yields an escape-only table,
// which is still a usable codec for an empty pool.
func (c *fsstCodec) Train(samples [][]byte) error {
	if len(samples) == 0 {
		samples = [][]byte{{}}
	}
	c.table = fsst.Train(samples)
	return nil
}

func (c *fsstCodec) Encode(src []byte) ([]byte, error) {
	if c.table == nil {
		return nil, fmt.Errorf("fsst: encode before train")
	}
	return c.table.Encode(src), nil
}

func (c *fsstCodec) Decode(src []byte) ([]byte, error) {
	if c.table == nil {
		return nil, fmt.Errorf("fsst: decode without a symbol table")
	}
	return c.table.DecodeAll(src), nil
}

func (c *fsstCodec) WriteState(w io.Writer) error {
	if c.table == nil {
		return fmt.Errorf("fsst: write state before train")
	}
	_, err := c.table.WriteTo(w)
	return err
}

func (c *fsstCodec) ReadState(r io.Reader) error {
	var t fsst.Table
	if _, err := t.ReadFrom(r); err != nil {
		return err
	}
	c.table = &t
	return nil
}

// NumSymbols returns how many symbols the table learned, or 0 if untrained.
func (c *fsstCodec) NumSymbols() int {
	if c.table == nil {
		return 0
	}
	return c.table.NumSymbols()
}
