package fsst

// counters tracks symbol and pair frequencies during training.
type counters struct {
	single   [codeMax]uint16
	pair     [codeMax][codeMax]uint16
	pairList [][2]uint32
}

func (c *counters) reset() {
	c.single = [codeMax]uint16{}
	for _, p := range c.pairList {
		c.pair[p[0]][p[1]] = 0
	}
	c.pairList = c.pairList[:0]
}

func (c *counters) incSingle(code uint32) {
	if c.single[code] < 0xFFFF {
		c.single[code]++
	}
}

func (c *counters) setSingle(code uint32, val uint16) {
	c.single[code] = val
}

func (c *counters) incPair(code1, code2 uint32) {
	if c.pair[code1][code2] == 0 {
		c.pairList = append(c.pairList, [2]uint32{code1, code2})
	}
	if c.pair[code1][code2] < 0xFFFF {
		c.pair[code1][code2]++
	}
}

// nextSingle advances *code to the next non-zero single count and returns it.
func (c *counters) nextSingle(code *uint32) uint32 {
	for *code < codeMax {
		if n := c.single[*code]; n != 0 {
			return uint32(n)
		}
		*code++
	}
	return 0
}

func (c *counters) pairCount(code1, code2 uint32) uint32 {
	return uint32(c.pair[code1][code2])
}
