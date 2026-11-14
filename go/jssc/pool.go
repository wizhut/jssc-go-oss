package jssc

import "slices"

// stringPool is the ordered set of strings an archive stores, together with the
// reverse map an encoder needs to turn a string into its index.
type stringPool struct {
	order []string
	index map[string]int
}

func newStringPool(capacity int) *stringPool {
	return &stringPool{
		order: make([]string, 0, capacity),
		index: make(map[string]int, capacity),
	}
}

func (p *stringPool) add(s string) int {
	if i, ok := p.index[s]; ok {
		return i
	}
	i := len(p.order)
	p.index[s] = i
	p.order = append(p.order, s)
	return i
}

func (p *stringPool) lookup(s string) (int, bool) {
	i, ok := p.index[s]
	return i, ok
}

func (p *stringPool) len() int          { return len(p.order) }
func (p *stringPool) strings() []string { return p.order }

// samples returns the pool entries as the training set for the pool codec.
func (p *stringPool) samples() [][]byte {
	out := make([][]byte, len(p.order))
	for i, s := range p.order {
		out[i] = []byte(s)
	}
	return out
}

// stringSets holds every distinct string reachable from the documents being
// encoded, split by role. A string that appears as both a key and a value is
// only in keys: keys are always pooled, which subsumes the value case.
type stringSets struct {
	keys   map[string]struct{}
	values map[string]struct{}
	// paths holds the distinct root-to-leaf key path hashes the documents
	// contain, for the JSS8 key-path filter (§7B).
	paths map[uint32]struct{}
}

// sortedPaths returns the gathered path hashes in ascending order.
func (s *stringSets) sortedPaths() []uint32 {
	out := make([]uint32, 0, len(s.paths))
	for h := range s.paths {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

func newStringSets() *stringSets {
	return &stringSets{
		keys:   make(map[string]struct{}),
		values: make(map[string]struct{}),
		paths:  make(map[uint32]struct{}),
	}
}

// gather walks a parsed document, recording every object key, every string
// value, and every root-to-leaf key path (§7B).
//
// The paths ride along in the same descent the strings need: a key's path hash
// is its parent's extended by one key, so the walk that already visits every
// member computes them for nothing. Both the in-memory and the streaming
// encoder gather this way, which is what keeps their archives identical.
func (s *stringSets) gather(v any) { s.walk(v, 0) }

// walk is gather, carrying the path hash of the container it is inside. An
// array passes its parent through unchanged: subscripts are not part of a path.
func (s *stringSets) walk(v any, parent uint32) {
	switch x := v.(type) {
	case string:
		s.values[x] = struct{}{}
	case []any:
		for _, el := range x {
			s.walk(el, parent)
		}
	case map[string]any:
		for k, el := range x {
			s.keys[k] = struct{}{}
			h := pathHash(parent, k)
			s.paths[h] = struct{}{}
			s.walk(el, h)
		}
	}
}

// distinct counts every string, whether it ends up pooled or inline.
func (s *stringSets) distinct() int {
	n := len(s.keys)
	for str := range s.values {
		if _, isKey := s.keys[str]; !isKey {
			n++
		}
	}
	return n
}

// poolPlan is the outcome of resolving the pool and its index width together.
type poolPlan struct {
	// keys is the key pool: every distinct object key, sorted. values is the
	// value pool: every distinct pooled string value, sorted. The roles are
	// disjoint namespaces (specification §7): a string used both ways appears
	// in both pools.
	keys   *stringPool
	values *stringPool
	// width sizes value-pool references; keyWidth sizes key-pool references,
	// derived from the key count alone (specification §5.5).
	width    int
	keyWidth int
	// inlineDistinct counts string values kept out of the pool because they fit
	// inline. Reported in Stats.
	inlineDistinct int
	// paths holds the distinct root-to-leaf key path hashes, ascending. Sorted
	// so that nothing downstream depends on map iteration order.
	paths []uint32
	// keyIdx is set only by the legacy (pre-JSS8) plan: the merged-pool index
	// of every key, ascending, for the old key-directory encoding.
	keyIdx []int
}

// buildPool resolves the string pool and the index width it is encoded with.
//
// The two depend on each other. Object keys are always pooled, but a string
// that only ever appears as a value is pooled only when it is longer than the
// index width — below that, writing it inline costs no more and saves a pool
// entry. Since dropping strings from the pool can shrink the pool enough to
// narrow the index, and a narrower index inlines fewer strings, this starts
// from the width implied by every distinct string and narrows while the
// resulting pool still fits (specification §8.8).
// legacyBuildPool resolves the single merged pool of the pre-JSS8 layouts:
// keys and values share one index space and one width. The golden fixtures
// pin these bytes, so this is the old algorithm verbatim.
func legacyBuildPool(sets *stringSets) poolPlan {
	width := indexWidthFor(sets.distinct())
	pool := legacyPoolAtWidth(sets, width)
	for {
		narrower := indexWidthFor(pool.len())
		if narrower >= width {
			break
		}
		candidate := legacyPoolAtWidth(sets, narrower)
		if indexWidthFor(candidate.len()) > narrower {
			break
		}
		pool, width = candidate, narrower
	}

	inline := 0
	for s := range sets.values {
		if _, pooled := pool.lookup(s); !pooled {
			inline++
		}
	}

	keyIdx := make([]int, 0, len(sets.keys))
	for k := range sets.keys {
		if i, ok := pool.lookup(k); ok {
			keyIdx = append(keyIdx, i)
		}
	}
	slices.Sort(keyIdx)

	return poolPlan{keys: pool, values: pool, width: width, keyWidth: width,
		inlineDistinct: inline, keyIdx: keyIdx, paths: sets.sortedPaths()}
}

// legacyPoolAtWidth is the merged-pool membership of the pre-JSS8 layouts.
func legacyPoolAtWidth(sets *stringSets, width int) *stringPool {
	members := make([]string, 0, len(sets.keys)+len(sets.values))
	for s := range sets.keys {
		members = append(members, s)
	}
	for s := range sets.values {
		if _, isKey := sets.keys[s]; isKey {
			continue
		}
		if inlinable(s, width) {
			continue
		}
		members = append(members, s)
	}
	slices.Sort(members)
	pool := newStringPool(len(members))
	for _, m := range members {
		pool.add(m)
	}
	return pool
}

func buildPool(sets *stringSets) poolPlan {
	// The value pool and its width resolve together through the inline rule;
	// the key pool needs no fixpoint — keys pool unconditionally, and the key
	// width follows their count (specification §8.6, §5.5).
	width := indexWidthFor(len(sets.values))
	values := poolAtWidth(sets, width)
	for {
		narrower := indexWidthFor(values.len())
		if narrower >= width {
			break
		}
		candidate := poolAtWidth(sets, narrower)
		if indexWidthFor(candidate.len()) > narrower {
			// Inlining fewer strings pushed the pool back over the boundary.
			break
		}
		values, width = candidate, narrower
	}

	inline := 0
	for s := range sets.values {
		if _, pooled := values.lookup(s); !pooled {
			inline++
		}
	}

	keyStrings := make([]string, 0, len(sets.keys))
	for k := range sets.keys {
		keyStrings = append(keyStrings, k)
	}
	slices.Sort(keyStrings)
	keys := newStringPool(len(keyStrings))
	for _, k := range keyStrings {
		keys.add(k)
	}

	return poolPlan{keys: keys, values: values, width: width,
		keyWidth: keyWidthFor(keys.len()), inlineDistinct: inline,
		paths: sets.sortedPaths()}
}

// poolAtWidth builds the pool membership implied by a given index width, sorted
// lexicographically by unsigned byte order so archives are deterministic and
// FSST trains on prefix-adjacent strings (specification §8.3).
func poolAtWidth(sets *stringSets, width int) *stringPool {
	members := make([]string, 0, len(sets.values))
	for s := range sets.values {
		if inlinable(s, width) {
			continue
		}
		members = append(members, s)
	}
	slices.Sort(members)

	pool := newStringPool(len(members))
	for _, s := range members {
		pool.add(s)
	}
	return pool
}

// inlinable reports whether a string value costs no more written inline than as
// a pool reference of the given width (specification §8.6).
func inlinable(s string, width int) bool {
	return len(s) <= width
}
