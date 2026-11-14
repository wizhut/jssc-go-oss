package fsst

import (
	"container/heap"
	"sync"
)

const (
	sampleTarget  = 1 << 14 // ~16KB
	sampleMaxSize = 2 * sampleTarget
	sampleLine    = 512

	singleByteBoost     = 8
	minCountNumerator   = 5
	minCountDenominator = 128
	rngSeed             = 4637947
	sampleMask          = 127
)

// Train builds a finalized symbol table from representative inputs.
func Train(inputs [][]byte) *Table {
	sample := makeSample(inputs)
	table := newTable()
	ws := trainingWorkspacePool.Get().(*trainingWorkspace)
	defer func() {
		ws.reset()
		trainingWorkspacePool.Put(ws)
	}()

	for frac := 8; ; frac += 30 {
		ws.counter.reset()
		compressCount(table, &ws.counter, sample, frac)
		buildCandidates(table, &ws.counter, frac)
		if frac >= 128 {
			break
		}
	}
	table.finalize()
	return table
}

// TrainStrings is a convenience wrapper around Train.
func TrainStrings(inputs []string) *Table {
	b := make([][]byte, len(inputs))
	for i := range inputs {
		b[i] = []byte(inputs[i])
	}
	return Train(b)
}

type trainingWorkspace struct {
	counter counters
}

func (w *trainingWorkspace) reset() { w.counter.reset() }

var trainingWorkspacePool = sync.Pool{
	New: func() any { return &trainingWorkspace{} },
}

func findNextSymbolFast(t *Table, data []byte, position int) (code uint16, advance int) {
	word := unalignedLoad(data[position:])
	prefix24 := word & mask24
	hashIndex := hashWord(prefix24) & (hashTabSize - 1)
	hashSymbol := t.enc.hashTab[hashIndex]
	shortCode := t.enc.shortCodes[uint16(word&mask16)] & codeMask
	symbolMask := ^uint64(0) >> hashSymbol.ignoredBits()
	maskedWord := word & symbolMask

	if hashSymbol.icl < iclFree && hashSymbol.val == maskedWord {
		return hashSymbol.code(), int(hashSymbol.length())
	}
	if shortCode >= codeBase {
		return shortCode, 2
	}
	return t.enc.byteCodes[byte(word&mask8)] & codeMask, 1
}

func compressCount(t *Table, c *counters, sample [][]byte, frac int) {
	for i := range sample {
		if frac < 128 && int(hashWord(uint64(i))&sampleMask) > frac {
			continue
		}
		end := len(sample[i])
		if end == 0 {
			continue
		}
		pos := 0
		cur := t.findLongestSymbol(newSymbolFromBytes(sample[i][pos:min(pos+8, end)]))
		pos += int(t.symbols[cur].length())
		start := 0
		for {
			c.incSingle(uint32(cur))
			if pos-start != 1 {
				c.incSingle(uint32(sample[i][start]))
			}
			if pos == end {
				break
			}
			start = pos
			var next uint16
			if pos < end-7 {
				var adv int
				next, adv = findNextSymbolFast(t, sample[i], pos)
				pos += adv
			} else {
				next = t.findLongestSymbol(newSymbolFromBytes(sample[i][pos:min(pos+8, end)]))
				pos += int(t.symbols[next].length())
			}
			if frac < 128 {
				n := pos - start
				c.incPair(uint32(cur), uint32(next))
				if n > 1 {
					c.incPair(uint32(cur), uint32(sample[i][start]))
				}
			}
			cur = next
		}
	}
}

type qsym struct {
	symbol symbol
	gain   uint32
}

type qsymHeap []qsym

func (h qsymHeap) Len() int { return len(h) }
func (h qsymHeap) Less(i, j int) bool {
	if h[i].gain != h[j].gain {
		return h[i].gain > h[j].gain // max-heap
	}
	if h[i].symbol.val != h[j].symbol.val {
		return h[i].symbol.val < h[j].symbol.val
	}
	return h[i].symbol.length() < h[j].symbol.length()
}
func (h qsymHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *qsymHeap) Push(x any)   { *h = append(*h, x.(qsym)) }
func (h *qsymHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func buildCandidates(t *Table, c *counters, frac int) {
	minCount := max((minCountNumerator*frac)/minCountDenominator, 1)
	cands := make(map[[2]uint64]uint32, 512)

	add := func(sym symbol, gain uint32) {
		if gain == 0 {
			return
		}
		key := [2]uint64{sym.val, uint64(sym.length())}
		cands[key] += gain
	}

	for code := uint32(0); code < codeBase+uint32(t.nSymbols); code++ {
		count := c.nextSingle(&code)
		if count == 0 {
			continue
		}
		sym := t.symbols[code]
		weight := uint64(count)
		if sym.length() == 1 {
			weight *= singleByteBoost
		}
		if int(weight) < minCount {
			continue
		}
		add(sym, uint32(weight)*uint32(sym.length()))
	}

	if frac < 128 {
		for _, p := range c.pairList {
			count2 := c.pairCount(p[0], p[1])
			if count2 == 0 || int(count2) < minCount {
				continue
			}
			sym := t.symbols[p[0]]
			if sym.length() == 8 {
				continue
			}
			merged := concatSymbols(sym, t.symbols[p[1]])
			add(merged, count2*uint32(merged.length()))
		}
	}

	h := make(qsymHeap, 0, len(cands))
	for key, gain := range cands {
		sym := symbol{val: key[0]}
		sym.setCodeLen(codeMask, uint32(key[1]))
		h = append(h, qsym{symbol: sym, gain: gain})
	}
	heap.Init(&h)

	t.clearSymbols()
	for h.Len() > 0 && int(t.nSymbols) < maxSymbols {
		q := heap.Pop(&h).(qsym)
		t.addSymbol(q.symbol)
	}
}

func makeSample(inputs [][]byte) [][]byte {
	var total int
	for i := range inputs {
		total += len(inputs[i])
	}
	if total < sampleTarget {
		return inputs
	}

	buf := make([]byte, sampleMaxSize)
	sample := make([][]byte, 0, len(inputs))
	pos := 0
	rng := hashWord(rngSeed)

	for pos < sampleMaxSize {
		rng = hashWord(rng)
		idx := int(rng % uint64(len(inputs)))
		for len(inputs[idx]) == 0 {
			idx = (idx + 1) % len(inputs)
		}

		numChunks := (len(inputs[idx]) + sampleLine - 1) / sampleLine
		rng = hashWord(rng)
		off := sampleLine * int(rng%uint64(numChunks))

		n := len(inputs[idx]) - off
		if n > sampleLine {
			n = sampleLine
		}
		if pos+n > sampleMaxSize {
			break
		}
		copy(buf[pos:pos+n], inputs[idx][off:off+n])
		sample = append(sample, buf[pos:pos+n:pos+n])
		pos += n
		if pos >= sampleTarget {
			break
		}
	}
	return sample
}
