package jssc

import (
	"errors"
	"fmt"

	"github.com/ohler55/ojg/jp"
)

// Match is one JSONPath hit against an Archive: the normalized path and the
// value found there.
type Match struct {
	Path  string
	Value any
}

// errMatchLimit unwinds the walk once QueryLimit has collected enough matches.
var errMatchLimit = errors.New("jssc: match limit reached")

// Query evaluates a JSONPath expression by walking the binary structure,
// decoding pool strings only where a key comparison or a matched value needs
// them.
//
// Archive.DecodedStrings and Archive.SkippedValues are reset at the start of
// each call and updated as the walk proceeds.
//
// A multi-document archive is queried as a virtual root array: "$[0].field",
// "$[*].field", and so on.
func (a *Archive) Query(path string) ([]Match, error) {
	return a.query(path, 0, true)
}

// QueryLimit is Query, stopping as soon as limit matches are found. A limit of
// zero or less means unlimited.
func (a *Archive) QueryLimit(path string, limit int) ([]Match, error) {
	if limit < 0 {
		limit = 0
	}
	return a.query(path, limit, true)
}

// QueryValues returns only the values matched by path.
//
// The walk is told that the paths are not wanted, which is not a formality: a
// path names each object member it descends through, and naming a member means
// decoding its key out of the pool. A value-only query decodes no keys at all.
func (a *Archive) QueryValues(path string) ([]any, error) {
	matches, err := a.query(path, 0, false)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(matches))
	for i, m := range matches {
		out[i] = m.Value
	}
	return out, nil
}

// QueryPaths returns only the normalized paths matched by path.
func (a *Archive) QueryPaths(path string) ([]string, error) {
	matches, err := a.Query(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.Path
	}
	return out, nil
}

// QueryOne returns the first value matched by path, stopping the walk as soon
// as it is found.
func (a *Archive) QueryOne(path string) (any, bool, error) {
	matches, err := a.query(path, 1, false)
	if err != nil {
		return nil, false, err
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	return matches[0].Value, true, nil
}

func (a *Archive) query(path string, limit int, wantPaths bool) ([]Match, error) {
	expr, err := jp.ParseString(path)
	if err != nil {
		return nil, fmt.Errorf("%w: jsonpath %q: %v", ErrInvalidInput, path, err)
	}
	a.DecodedStrings = 0
	a.DecodedGroups = 0
	a.SkippedValues = 0
	a.FilteredOut = false
	a.matchLimit = limit
	a.wantPaths = wantPaths
	defer func() { a.matchLimit = 0; a.wantPaths = true }()

	// The cheapest walk is the one that does not happen. When the archive
	// carries a key-path filter (§7B) and the expression names a concrete path,
	// a rejection here is proof there is nothing to find — no structure read, no
	// pool decode, no container entered.
	if a.rejectedByPathFilter(expr) {
		a.FilteredOut = true
		return nil, nil
	}

	var out []Match
	if a.MultiDoc() {
		err = a.evalDocs(expr, &out)
	} else {
		start, serr := a.docStart(0)
		if serr != nil {
			return nil, serr
		}
		_, err = a.eval(start, expr, nil, &out)
	}
	if err != nil && !errors.Is(err, errMatchLimit) {
		return nil, err
	}
	return out, nil
}

// rejectedByPathFilter reports whether the key-path filter proves the
// expression selects nothing.
//
// It applies only to expressions that name a path: child segments, and array
// subscripts, which contribute nothing because the filter elides them. A
// wildcard, a descent or a filter fragment does not name a path — the keys it
// would match are not known here — so those fall through to the walk.
//
// A multi-document archive is a virtual array of its roots, so a leading
// subscript over the documents is elided on the same terms as any other.
func (a *Archive) rejectedByPathFilter(expr jp.Expr) bool {
	if !a.paths.present() {
		return false
	}
	var h uint32
	named := false
	// atRoot tracks whether the next fragment addresses the root value, which
	// is the one place a wildcard's container is known without walking.
	atRoot := true
	for _, frag := range expr {
		switch f := frag.(type) {
		case jp.Root, jp.At, jp.Bracket:
			// Positioning, not naming.
		case jp.Nth, jp.Slice:
			// Subscripts select from arrays and arrays are elided, so these
			// name nothing wherever they appear.
			atRoot = false
		case jp.Wildcard:
			// A wildcard over an array is a subscript; over an object it stands
			// for a key, and that key is part of the path. Eliding it in the
			// second case would compute a hash for a path the document never
			// had and reject a query that should have matched — so it is only
			// safe where the container is known to be an array.
			if !atRoot || !a.rootIsArray() {
				return false
			}
			atRoot = false
		case jp.Child:
			h = pathHash(h, string(f))
			named = true
			atRoot = false
		default:
			// Descent, filters, unions: they do not name a single path.
			return false
		}
	}
	if !named {
		return false
	}
	return !a.paths.mayContain(h)
}

// rootIsArray reports whether the archive's root value is an array, which for a
// multi-document archive is true by construction: its roots are presented as a
// virtual array.
//
// For a single document this reads one tag, which for a deflated structure
// means inflating the first block — the same block the walk would have inflated
// as its first act, so the check never costs more than the step it replaces.
func (a *Archive) rootIsArray() bool {
	if a.MultiDoc() {
		return true
	}
	start, err := a.docStart(0)
	if err != nil {
		return false
	}
	tag, err := a.peekTag(start)
	return err == nil && tag == tagArray
}

// evalDocs walks a multi-document archive as if its roots were an array. There
// is no array tag at the virtual root, so the leading fragment is interpreted
// against docOff directly.
func (a *Archive) evalDocs(expr jp.Expr, out *[]Match) error {
	path := jp.Expr{}
	rest := expr
	for len(rest) > 0 {
		switch rest[0].(type) {
		case jp.Root:
			path = append(clonePath(path), rest[0])
			rest = rest[1:]
			continue
		case jp.At, jp.Bracket:
			rest = rest[1:]
			continue
		}
		break
	}

	if len(rest) == 0 {
		docs, err := a.allDocs()
		if err != nil {
			return err
		}
		return a.addMatch(out, Match{Path: "$", Value: docs})
	}

	frag := rest[0]
	rest = rest[1:]
	evalDoc := func(i int) error {
		start, err := a.docStart(i)
		if err != nil {
			return err
		}
		_, err = a.eval(start, rest, append(clonePath(path), jp.Nth(i)), out)
		return err
	}

	switch f := frag.(type) {
	case jp.Nth:
		i := int(f)
		if i < 0 {
			i += a.docCount
		}
		if i < 0 || i >= a.docCount {
			return nil
		}
		return evalDoc(i)

	case jp.Wildcard:
		for i := range a.docCount {
			if err := evalDoc(i); err != nil {
				return err
			}
		}
		return nil

	case jp.Slice:
		for _, i := range sliceIndices(f, a.docCount) {
			if err := evalDoc(i); err != nil {
				return err
			}
		}
		return nil

	case jp.Descent:
		// Descent through the virtual root visits each document, then descends.
		descent := append(jp.Expr{jp.Descent('.')}, rest...)
		for i := range a.docCount {
			p := append(clonePath(path), jp.Nth(i))
			start, err := a.docStart(i)
			if err != nil {
				return err
			}
			if _, err := a.eval(start, descent, p, out); err != nil {
				return err
			}
		}
		return nil

	case *jp.Filter:
		// The walked fast path applies to the virtual root exactly as it does
		// to a real array. Without this dispatch a filter over a
		// multi-document archive materialized every document to reject it —
		// measured on a million-document corpus as the entire pool decoded
		// for a predicate the walk answers by comparing integers.
		if wp, walkable := parseWalkablePred(f); walkable {
			rp, err := a.resolvePred(wp)
			if err != nil {
				return err
			}
			kids := make([]childRef, a.docCount)
			for i := range a.docCount {
				start, err := a.docStart(i)
				if err != nil {
					return err
				}
				kids[i] = childRef{frag: jp.Nth(i), key: -1, pos: start}
			}
			return a.evalFilterWalk(kids, rp, rest, path, out)
		}
		for i := range a.docCount {
			v, err := a.Doc(i)
			if err != nil {
				return err
			}
			if !f.Match(v) {
				continue
			}
			p := append(clonePath(path), jp.Nth(i))
			if len(rest) == 0 {
				if err := a.addMatch(out, Match{Path: a.pathString(p), Value: v}); err != nil {
					return err
				}
				continue
			}
			start, err := a.docStart(i)
			if err != nil {
				return err
			}
			if _, err := a.eval(start, rest, p, out); err != nil {
				return err
			}
		}
		return nil

	default:
		// Anything else: materialize the virtual array and finish with ojg.
		docs, err := a.allDocs()
		if err != nil {
			return err
		}
		return a.addLocated(out, path, append(jp.Expr{frag}, rest...), docs)
	}
}

func (a *Archive) allDocs() ([]any, error) {
	docs := make([]any, a.docCount)
	for i := range a.docCount {
		v, err := a.Doc(i)
		if err != nil {
			return nil, err
		}
		docs[i] = v
	}
	return docs, nil
}

// eval matches rest against the value at pos and returns the offset just past
// that value.
func (a *Archive) eval(pos int, rest, path jp.Expr, out *[]Match) (int, error) {
	if len(rest) == 0 {
		v, next, err := a.readValue(pos)
		if err != nil {
			return 0, err
		}
		return next, a.addMatch(out, Match{Path: a.pathString(path), Value: v})
	}

	frag := rest[0]
	rest = rest[1:]

	switch f := frag.(type) {
	case jp.Root, jp.At:
		return a.eval(pos, rest, append(clonePath(path), frag), out)
	case jp.Bracket:
		return a.eval(pos, rest, path, out)
	case jp.Child:
		return a.evalChild(pos, string(f), rest, path, out)
	case jp.Nth:
		return a.evalNth(pos, int(f), rest, path, out)
	case jp.Wildcard:
		return a.evalWildcard(pos, rest, path, out)
	case jp.Descent:
		return a.evalDescent(pos, rest, path, out)
	case jp.Slice:
		return a.evalSlice(pos, f, rest, path, out)
	case jp.Union:
		return a.evalUnion(pos, f, rest, path, out)
	case *jp.Filter:
		return a.evalFilter(pos, f, rest, path, out)
	default:
		// A fragment this walker does not special-case: materialize the value
		// and hand the remainder to ojg.
		v, next, err := a.readValue(pos)
		if err != nil {
			return 0, err
		}
		return next, a.addLocated(out, path, append(jp.Expr{frag}, rest...), v)
	}
}

// evalChild resolves one object field. Members are sorted by key index, so once
// the key's pool index is known the lookup is an integer binary search that
// decodes no strings at all.
func (a *Archive) evalChild(pos int, key string, rest, path jp.Expr, out *[]Match) (int, error) {
	ok, meta, err := a.container(pos, tagObject)
	if err != nil || !ok {
		return meta.end, err
	}
	childPath := append(clonePath(path), jp.Child(key))

	// Object keys are always pooled, so a key that is not in the pool names
	// nothing in this archive and the whole subtree can be dismissed without
	// reading a single member. With a key directory that verdict costs one
	// hash and a binary search over a flat array — no pool group is touched.
	target, found, err := a.lookupFieldIndex(key)
	if err != nil {
		return 0, err
	}
	if !found {
		return meta.end, nil
	}

	valPos, found, err := a.memberValuePos(pos, meta, target)
	if err != nil {
		return 0, err
	}
	if !found {
		return meta.end, nil
	}
	_, err = a.eval(valPos, rest, childPath, out)
	return meta.end, err
}

// memberValuePos binary-searches the object at pos for the member whose key
// has pool index target, and returns the member value's position. Members are
// stored in ascending key-index order precisely so this costs log(m) integer
// comparisons; no key and no value is decoded.
//
// A field lookup reads at most log(m) members, so it takes the offsets one at
// a time rather than materializing the container's whole offset table.
func (a *Archive) memberValuePos(pos int, meta containerMeta, target int) (int, bool, error) {
	ci, err := a.childIndexFor(pos, meta)
	if err != nil {
		return 0, false, err
	}
	lo, hi := 0, meta.count-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		keyPos, err := ci.at(mid)
		if err != nil {
			return 0, false, err
		}
		idx, valPos, err := a.readKeyIndexAt(keyPos)
		if err != nil {
			return 0, false, err
		}
		switch {
		case idx == target:
			return valPos, true, nil
		case idx < target:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return 0, false, nil
}

func (a *Archive) evalNth(pos, index int, rest, path jp.Expr, out *[]Match) (int, error) {
	ok, meta, err := a.container(pos, tagArray)
	if err != nil || !ok {
		return meta.end, err
	}
	target := index
	if target < 0 {
		target += meta.count
	}
	if target < 0 || target >= meta.count {
		return meta.end, nil
	}

	elemPos, err := a.nthChildPos(pos, meta, target)
	if err != nil {
		return 0, err
	}
	_, err = a.eval(elemPos, rest, append(clonePath(path), jp.Nth(index)), out)
	return meta.end, err
}

// nthChildPos locates one array element. It prefers a cached or on-disk offset
// table; without one it skips just the prefix, since building a full offset
// table for "$[0]" would scan the entire array.
func (a *Archive) nthChildPos(pos int, meta containerMeta, target int) (int, error) {
	if offs, ok := a.childOff[pos]; ok {
		return offs[target], nil
	}
	if meta.hasTable() {
		// One entry is all this costs; do not decode the other m-1.
		return a.memberOff(meta, target)
	}
	elemPos := meta.elemsStart
	for range target {
		next, err := a.skipValue(elemPos)
		if err != nil {
			return 0, err
		}
		elemPos = next
	}
	return elemPos, nil
}

func (a *Archive) evalWildcard(pos int, rest, path jp.Expr, out *[]Match) (int, error) {
	kids, end, err := a.children(pos)
	if err != nil {
		return 0, err
	}
	for _, k := range kids {
		childPath, err := a.childPath(path, k)
		if err != nil {
			return end, err
		}
		if _, err := a.eval(k.pos, rest, childPath, out); err != nil {
			return end, err
		}
	}
	return end, nil
}

func (a *Archive) evalSlice(pos int, s jp.Slice, rest, path jp.Expr, out *[]Match) (int, error) {
	ok, meta, err := a.container(pos, tagArray)
	if err != nil || !ok {
		return meta.end, err
	}
	offs, err := a.ensureChildOff(pos)
	if err != nil {
		return 0, err
	}
	for _, i := range sliceIndices(s, meta.count) {
		if _, err := a.eval(offs[i], rest, append(clonePath(path), jp.Nth(i)), out); err != nil {
			return meta.end, err
		}
	}
	return meta.end, nil
}

func (a *Archive) evalUnion(pos int, u jp.Union, rest, path jp.Expr, out *[]Match) (int, error) {
	tag, err := a.peekTag(pos)
	if err != nil {
		return 0, err
	}
	switch tag {
	case tagArray:
		meta, err := a.containerMetaAt(pos)
		if err != nil {
			return 0, err
		}
		offs, err := a.ensureChildOff(pos)
		if err != nil {
			return 0, err
		}
		for i := range meta.count {
			if !unionHasIndex(u, i) {
				continue
			}
			if _, err := a.eval(offs[i], rest, append(clonePath(path), jp.Nth(i)), out); err != nil {
				return meta.end, err
			}
		}
		return meta.end, nil

	case tagObject:
		meta, err := a.containerMetaAt(pos)
		if err != nil {
			return 0, err
		}
		keyOffs, err := a.ensureKeyOff(pos)
		if err != nil {
			return 0, err
		}
		// When every named key is already in the pool cache, membership is an
		// integer test and no key has to be decoded.
		want := a.unionKeyIndices(u)
		for i := range meta.count {
			idx, valPos, err := a.readKeyIndexAt(keyOffs[i])
			if err != nil {
				return 0, err
			}
			var childFrag jp.Frag
			if want != nil {
				if _, hit := want[idx]; !hit {
					continue
				}
				k, err := a.keyString(idx)
				if err != nil {
					return 0, err
				}
				childFrag = jp.Child(k)
			} else {
				k, err := a.keyString(idx)
				if err != nil {
					return 0, err
				}
				if !unionHasKey(u, k) {
					continue
				}
				childFrag = jp.Child(k)
			}
			if _, err := a.eval(valPos, rest, append(clonePath(path), childFrag), out); err != nil {
				return meta.end, err
			}
		}
		return meta.end, nil

	default:
		return a.skipValue(pos)
	}
}

func (a *Archive) evalFilter(pos int, f *jp.Filter, rest, path jp.Expr, out *[]Match) (int, error) {
	kids, end, err := a.children(pos)
	if err != nil {
		return 0, err
	}

	// The walked fast path, for the predicate shapes that dominate real
	// filters: equality against string, boolean, and null literals, bare
	// existence, and their && / || compositions. Each leaf's key path
	// resolves to key-pool indices once through the key directory, a string
	// literal resolves to a value-pool index at most once, and each candidate
	// is then tested by binary-searching members and comparing integers — a
	// candidate is never materialized to be rejected, and two pooled strings
	// are equal exactly when their indices are. Every other predicate shape
	// falls back to ojg over a materialized candidate, which is the reference
	// semantics; the fast path must agree with it or it is a bug.
	if wp, walkable := parseWalkablePred(f); walkable {
		rp, err := a.resolvePred(wp)
		if err != nil {
			return 0, err
		}
		err = a.evalFilterWalk(kids, rp, rest, path, out)
		return end, err
	}

	for _, k := range kids {
		v, _, err := a.readValue(k.pos)
		if err != nil {
			return 0, err
		}
		if !f.Match(v) {
			continue
		}
		// Named only now that it matched: a predicate that rejects a member
		// should not have cost its key.
		childPath, err := a.childPath(path, k)
		if err != nil {
			return end, err
		}
		if len(rest) == 0 {
			if err := a.addMatch(out, Match{Path: a.pathString(childPath), Value: v}); err != nil {
				return end, err
			}
			continue
		}
		if err := a.addLocated(out, childPath, jp.Expr(rest), v); err != nil {
			return end, err
		}
	}
	return end, nil
}

// rpred is a walkable predicate with its leaf key paths resolved to key-pool
// indices, and per-leaf literal state the walk fills lazily.
type rpred struct {
	kind predKind
	l, r *rpred

	// absent marks a leaf whose key path names a key the directory does not
	// list anywhere in the archive: its target can never be reached, so the
	// leaf is a constant — false for == and existence, true for !=, exactly
	// as ojg answers a comparison against Nothing.
	absent bool
	want   []int
	lit    litKind
	s      string
	b      bool
	// litIdx is a string literal's value-pool index, resolved on the first
	// pooled comparison: -1 unresolved, -2 proven absent from the pool.
	litIdx int
}

// resolvePred resolves every leaf's key path through the key directory once,
// for the whole filter.
func (a *Archive) resolvePred(p *pred) (*rpred, error) {
	r := &rpred{kind: p.kind, lit: p.lit, s: p.s, b: p.b, litIdx: -1}
	if p.l != nil {
		var err error
		if r.l, err = a.resolvePred(p.l); err != nil {
			return nil, err
		}
		if r.r, err = a.resolvePred(p.r); err != nil {
			return nil, err
		}
		return r, nil
	}
	r.want = make([]int, len(p.segs))
	for i, seg := range p.segs {
		idx, ok, err := a.lookupFieldIndex(seg)
		if err != nil {
			return nil, err
		}
		if !ok {
			r.absent = true
			break
		}
		r.want[i] = idx
	}
	return r, nil
}

// predHolds evaluates a resolved predicate against the candidate at pos,
// reading at most one member per leaf and decoding nothing.
func (a *Archive) predHolds(pos int, p *rpred) (bool, error) {
	switch p.kind {
	case predAnd:
		ok, err := a.predHolds(pos, p.l)
		if err != nil || !ok {
			return false, err
		}
		return a.predHolds(pos, p.r)
	case predOr:
		ok, err := a.predHolds(pos, p.l)
		if err != nil || ok {
			return ok, err
		}
		return a.predHolds(pos, p.r)
	case predExists:
		// Existence is about the key, not the value: a member holding null or
		// false still exists, which is how ojg answers a bare @-path.
		if p.absent {
			return false, nil
		}
		_, found, err := a.predicateTarget(pos, p.want)
		return found, err
	}

	// predEq, predNe. A target that cannot be reached — key absent from the
	// archive, member absent from this candidate, or a non-object on the way
	// — is unequal to every literal, null included: ojg's Nothing.
	eq := false
	if !p.absent {
		valPos, found, err := a.predicateTarget(pos, p.want)
		if err != nil {
			return false, err
		}
		if found {
			if eq, err = a.leafEquals(valPos, p); err != nil {
				return false, err
			}
		}
	}
	if p.kind == predNe {
		return !eq, nil
	}
	return eq, nil
}

// leafEquals compares the value at pos against a leaf's literal, reading a
// tag byte and at most one pool index or four inline bytes.
func (a *Archive) leafEquals(pos int, p *rpred) (bool, error) {
	switch p.lit {
	case litStr:
		return a.stringValueEquals(pos, p.s, &p.litIdx)
	default:
		tagPos, err := valueTagPos(a.structure, pos, a.lenPrefix)
		if err != nil {
			return false, err
		}
		tag, err := a.structure.byteAt(tagPos)
		if err != nil {
			return false, err
		}
		if p.lit == litNull {
			return tag == tagNull, nil
		}
		if p.b {
			return tag == tagTrue, nil
		}
		return tag == tagFalse, nil
	}
}

// evalFilterWalk tests every candidate against a resolved predicate tree,
// materializing only the candidates that match.
func (a *Archive) evalFilterWalk(kids []childRef, rp *rpred, rest, path jp.Expr, out *[]Match) error {
	for _, k := range kids {
		match, err := a.predHolds(k.pos, rp)
		if err != nil {
			return err
		}
		if !match {
			continue
		}

		// Named and decoded only now that it matched.
		childPath, err := a.childPath(path, k)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			v, _, err := a.readValue(k.pos)
			if err != nil {
				return err
			}
			if err := a.addMatch(out, Match{Path: a.pathString(childPath), Value: v}); err != nil {
				return err
			}
			continue
		}
		// The rest of the path continues selectively over the archive instead
		// of over a materialized tree: a filter that selects one member should
		// not have decoded the other forty.
		if _, err := a.eval(k.pos, rest, childPath, out); err != nil {
			return err
		}
	}
	return nil
}

// predicateTarget descends the resolved key path from a candidate, by binary
// search per object, and returns the terminal value's position. Any missing
// member or non-object on the way means the predicate is false for this
// candidate, exactly as ojg evaluates Nothing.
func (a *Archive) predicateTarget(pos int, want []int) (int, bool, error) {
	for _, target := range want {
		ok, meta, err := a.container(pos, tagObject)
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return 0, false, nil
		}
		valPos, found, err := a.memberValuePos(pos, meta, target)
		if err != nil {
			return 0, false, err
		}
		if !found {
			return 0, false, nil
		}
		pos = valPos
	}
	return pos, true, nil
}

// stringValueEquals reports whether the value at pos is exactly the string
// lit, reading at most a tag byte, one pool index, or four inline bytes. The
// representation of a string value is decided by its length alone — at most
// index_width bytes inline, pooled otherwise — so only one arm can ever match
// a given literal.
func (a *Archive) stringValueEquals(pos int, lit string, litIdx *int) (bool, error) {
	tagPos, err := valueTagPos(a.structure, pos, a.lenPrefix)
	if err != nil {
		return false, err
	}
	tag, err := a.structure.byteAt(tagPos)
	if err != nil {
		return false, err
	}
	switch {
	case tag == tagString:
		if len(lit) <= a.indexWidth {
			// The literal would have been written inline; a pooled string is
			// longer than index_width by construction and cannot equal it.
			return false, nil
		}
		if *litIdx == -1 {
			idx, found, err := a.LookupIndex(lit)
			if err != nil {
				return false, err
			}
			if !found {
				*litIdx = -2
			} else {
				*litIdx = idx
			}
		}
		if *litIdx < 0 {
			return false, nil
		}
		idx, _, err := a.readIndexAt(tagPos + 1)
		if err != nil {
			return false, err
		}
		return idx == *litIdx, nil
	case tag >= tagInline0 && tag <= tagInlineMax:
		n := int(tag - tagInline0)
		if n != len(lit) {
			return false, nil
		}
		for i := range n {
			b, err := a.structure.byteAt(tagPos + 1 + i)
			if err != nil {
				return false, err
			}
			if b != lit[i] {
				return false, nil
			}
		}
		return true, nil
	default:
		// Numbers, booleans, null, containers: never equal to a string
		// literal, which is also how ojg answers it.
		return false, nil
	}
}

func (a *Archive) evalDescent(pos int, rest, path jp.Expr, out *[]Match) (int, error) {
	kids, end, err := a.children(pos)
	if err != nil {
		return 0, err
	}

	if len(rest) == 0 {
		v, _, err := a.readValue(pos)
		if err != nil {
			return 0, err
		}
		if err := a.addMatch(out, Match{Path: a.pathString(path), Value: v}); err != nil {
			return end, err
		}
	} else {
		switch f := rest[0].(type) {
		case jp.Child:
			// The key is resolved to a pool index once and every member of
			// every object on the way down is then an integer comparison. The
			// alternative — the one this replaces — decoded each member's key
			// and compared strings, which is a pool decode per member of a
			// document the descent is only passing through.
			want, ok, err := a.lookupFieldIndex(string(f))
			if err != nil {
				return end, err
			}
			if ok {
				for _, k := range kids {
					if k.key != want {
						continue
					}
					var childPath jp.Expr
					if a.wantPaths {
						childPath = append(clonePath(path), f)
					}
					if _, err := a.eval(k.pos, rest[1:], childPath, out); err != nil {
						return end, err
					}
				}
			}
		case jp.Nth:
			i := int(f)
			if i < 0 {
				i += len(kids)
			}
			if i >= 0 && i < len(kids) {
				if _, err := a.eval(kids[i].pos, rest[1:], append(clonePath(path), f), out); err != nil {
					return end, err
				}
			}
		case jp.Wildcard:
			for _, k := range kids {
				childPath, err := a.childPath(path, k)
				if err != nil {
					return end, err
				}
				if _, err := a.eval(k.pos, rest[1:], childPath, out); err != nil {
					return end, err
				}
			}
		default:
			v, _, err := a.readValue(pos)
			if err != nil {
				return 0, err
			}
			if err := a.addLocated(out, path, jp.Expr(rest), v); err != nil {
				return end, err
			}
		}
	}

	for _, k := range kids {
		// This is the step that made a descent cost a key per node: it names
		// every child on the way past, whether or not anything below it
		// matches. With paths unwanted there is nothing to name.
		childPath, err := a.childPath(path, k)
		if err != nil {
			return end, err
		}
		if _, err := a.evalDescent(k.pos, rest, childPath, out); err != nil {
			return end, err
		}
	}
	return end, nil
}

// container resolves the value at pos when it has the wanted container tag. A
// mismatch is not an error — a path segment simply does not apply here — so it
// reports ok=false with meta.end set past the value.
func (a *Archive) container(pos int, want byte) (ok bool, meta containerMeta, err error) {
	tag, err := a.peekTag(pos)
	if err != nil {
		return false, containerMeta{}, err
	}
	if tag != want {
		end, err := a.skipValue(pos)
		return false, containerMeta{end: end}, err
	}
	meta, err = a.containerMetaAt(pos)
	if err != nil {
		return false, containerMeta{}, err
	}
	return true, meta, nil
}

// childRef is one immediate child: where its value starts, and enough to name
// it if anyone asks.
//
// An object member carries its key's *pool index*, not the key. That is the
// difference between a walk that decodes every key it passes and one that
// decodes only the keys its answer mentions: a member is matched by comparing
// integers, and the string is fetched — through childFrag — only to build a
// path a caller actually wanted.
type childRef struct {
	frag jp.Frag // set for array elements; nil for object members
	key  int     // pool index of the member key, or -1 for an array element
	pos  int
}

// childFrag names a child, decoding its key if it has not been decoded before.
func (a *Archive) childFrag(c childRef) (jp.Frag, error) {
	if c.frag != nil {
		return c.frag, nil
	}
	k, err := a.keyString(c.key)
	if err != nil {
		return nil, err
	}
	return jp.Child(k), nil
}

// childPath extends path so that it names c, or returns nil when the caller did
// not ask for paths — in which case c's key is never decoded.
func (a *Archive) childPath(path jp.Expr, c childRef) (jp.Expr, error) {
	if !a.wantPaths {
		return nil, nil
	}
	frag, err := a.childFrag(c)
	if err != nil {
		return nil, err
	}
	return append(clonePath(path), frag), nil
}

// pathString renders a path for a Match, or the empty string when paths were
// not asked for.
func (a *Archive) pathString(path jp.Expr) string {
	if !a.wantPaths {
		return ""
	}
	return path.String()
}

// children lists the immediate children of the value at pos, and returns the
// offset just past it. A leaf has no children. Neither keys nor values are
// decoded: a member is returned as its key's pool index (§7A), and callers that
// need the name ask childFrag for it.
func (a *Archive) children(pos int) ([]childRef, int, error) {
	tag, err := a.peekTag(pos)
	if err != nil {
		return nil, 0, err
	}
	if tag != tagArray && tag != tagObject {
		next, err := a.skipValue(pos)
		return nil, next, err
	}

	meta, err := a.containerMetaAt(pos)
	if err != nil {
		return nil, 0, err
	}
	valOffs, err := a.ensureChildOff(pos)
	if err != nil {
		return nil, 0, err
	}

	kids := make([]childRef, meta.count)
	if !meta.isObject {
		for i := range meta.count {
			kids[i] = childRef{frag: jp.Nth(i), key: -1, pos: valOffs[i]}
		}
		return kids, meta.end, nil
	}

	keyOffs, err := a.ensureKeyOff(pos)
	if err != nil {
		return nil, 0, err
	}
	for i := range meta.count {
		idx, _, err := a.readKeyIndexAt(keyOffs[i])
		if err != nil {
			return nil, 0, err
		}
		kids[i] = childRef{key: idx, pos: valOffs[i]}
	}
	return kids, meta.end, nil
}

func (a *Archive) addMatch(out *[]Match, m Match) error {
	*out = append(*out, m)
	if a.matchLimit > 0 && len(*out) >= a.matchLimit {
		return errMatchLimit
	}
	return nil
}

// addLocated finishes a path with ojg against an already-materialized value.
func (a *Archive) addLocated(out *[]Match, base, expr jp.Expr, data any) error {
	max := 0
	if a.matchLimit > 0 {
		max = a.matchLimit - len(*out)
		if max <= 0 {
			return errMatchLimit
		}
	}
	for _, loc := range expr.Locate(data, max) {
		full := append(clonePath(base), loc...)
		if err := a.addMatch(out, Match{Path: a.pathString(full), Value: loc.First(data)}); err != nil {
			return err
		}
	}
	return nil
}

// clonePath copies a path prefix so appending to it cannot disturb a sibling
// branch of the walk.
func clonePath(path jp.Expr) jp.Expr {
	if len(path) == 0 {
		return nil
	}
	out := make(jp.Expr, len(path))
	copy(out, path)
	return out
}

// unionKeyIndices maps the union's named keys to pool indices, but only when
// every one of them has already been decoded. Returning nil tells the caller to
// fall back to string comparison.
func (a *Archive) unionKeyIndices(u jp.Union) map[int]struct{} {
	out := make(map[int]struct{})
	for _, x := range u {
		s, ok := x.(string)
		if !ok {
			continue
		}
		idx, known, err := a.lookupFieldIndex(s)
		if err != nil || !known {
			// A union naming a string the pool does not hold cannot be resolved
			// by index; fall back to comparing decoded keys.
			return nil
		}
		out[idx] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func unionHasIndex(u jp.Union, i int) bool {
	for _, x := range u {
		switch v := x.(type) {
		case int:
			if v == i {
				return true
			}
		case int64:
			if int(v) == i {
				return true
			}
		}
	}
	return false
}

func unionHasKey(u jp.Union, key string) bool {
	for _, x := range u {
		if s, ok := x.(string); ok && s == key {
			return true
		}
	}
	return false
}

// sliceBounds resolves a JSONPath slice against a container size. The returned
// start and end are both inclusive, and a step of 0 means the slice selects
// nothing.
//
// This mirrors jp.Slice.startEndStep, which is unexported: the selective walk
// has to agree with ojg on every open, negative, and out-of-range bound, since
// the two are meant to give the same answer for the same path. The written
// JSONPath end is exclusive; the conversion happens here.
func sliceBounds(s jp.Slice, size int) (start, end, step int) {
	start, end, step = s[0], s[1], s[2]
	if step == jp.SliceNotSet {
		step = 1
	}

	switch {
	case start == jp.SliceNotSet:
		if step >= 0 {
			start = 0
		} else {
			start = size - 1
		}
	case start < 0:
		start = max(size+start, 0)
	case start >= size:
		if step >= 0 {
			step = 0 // start is past the end and we are going forwards
		} else {
			start = size - 1
		}
	}

	switch {
	case end == jp.SliceNotSet:
		if step >= 0 {
			end = size - 1
		} else {
			end = 0
		}
	case end < 0:
		end += size
		switch {
		case end < 0:
			if step > 0 {
				step = 0
			} else {
				end = 0
			}
		case step > 0:
			end-- // exclusive to inclusive
		default:
			end++
			if end >= size {
				step = 0
			}
		}
	case end >= size:
		end = size - 1
	default:
		if step > 0 {
			end-- // exclusive to inclusive
		} else {
			end++
			if end >= size {
				step = 0
			}
		}
	}

	if step != 0 {
		// Snap end onto the last index the stride actually reaches, so a plain
		// range test is enough to decide membership.
		end = start + (end-start)/step*step
	}
	return start, end, step
}

// sliceIndices lists the indices a slice selects, in the order ojg yields them
// — which for a negative step counts down.
func sliceIndices(s jp.Slice, size int) []int {
	start, end, step := sliceBounds(s, size)
	if step == 0 || size == 0 {
		return nil
	}
	var out []int
	if step > 0 {
		for i := start; i <= end; i += step {
			if i >= 0 && i < size {
				out = append(out, i)
			}
		}
		return out
	}
	for i := start; i >= end; i += step {
		if i >= 0 && i < size {
			out = append(out, i)
		}
	}
	return out
}
