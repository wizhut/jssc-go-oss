package jssc

import (
	"strings"

	"github.com/ohler55/ojg/jp"
)

// The walked filter's predicate tree. parseWalkablePred builds it from the
// filter's normalized string form; the archive walks it per candidate without
// materializing anything (query.go, predHolds).
//
// The walkable shapes are equality against string, boolean, and null
// literals, bare @-path existence, and their conjunctions and disjunctions:
//
//	[?(@.a.b == 'x' && (@.c != 'y' || @.d == true))]
//
// Everything else — ordering operators, numeric literals, regex matches,
// negation, quoted or bracketed keys — reports false and takes the
// materializing fallback, whose semantics are ojg's and therefore the
// reference.
//
// The boundary is deliberate, twice over. Equality over the archive is exact:
// pooled strings are equal iff their indices are, inline strings compare by
// byte, booleans and null are single tags. Ordering operators would have to
// reproduce ojg's coercion rules bug for bug, and numeric equality is worse
// than that: the project's reference trees carry json.Number, which ojg's
// comparison against a bare numeric literal never matches — a walk that
// "fixed" this would disagree with the fallback it must be equivalent to.
type predKind uint8

const (
	predAnd predKind = iota
	predOr
	predEq     // @.path == literal
	predNe     // @.path != literal
	predExists // bare @.path
)

type litKind uint8

const (
	litStr litKind = iota
	litBool
	litNull
)

type pred struct {
	kind predKind
	l, r *pred // predAnd, predOr

	segs []string // leaves: the @-relative key path
	lit  litKind
	s    string // litStr
	b    bool   // litBool
}

// parseWalkablePred recognizes the predicate shapes the walked filter
// handles, from the filter's normalized String() form — ojg does not export
// its equation tree, but it does re-serialize it canonically.
func parseWalkablePred(f *jp.Filter) (*pred, bool) {
	src := f.String()
	var inner string
	switch {
	case strings.HasPrefix(src, "[?(") && strings.HasSuffix(src, ")]"):
		inner = src[3 : len(src)-2]
	case strings.HasPrefix(src, "[?") && strings.HasSuffix(src, "]"):
		inner = src[2 : len(src)-1]
	default:
		return nil, false
	}

	p := predParser{s: inner}
	node, ok := p.parseOr()
	if !ok {
		return nil, false
	}
	p.skipSpace()
	if p.i != len(p.s) {
		return nil, false
	}
	return node, true
}

// predParser is a recursive-descent parser over the filter's inner text.
// Precedence is JSONPath's: || binds loosest, && tighter, parentheses group.
type predParser struct {
	s string
	i int
}

func (p *predParser) skipSpace() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *predParser) lit(tok string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.s[p.i:], tok) {
		p.i += len(tok)
		return true
	}
	return false
}

func (p *predParser) parseOr() (*pred, bool) {
	node, ok := p.parseAnd()
	if !ok {
		return nil, false
	}
	for p.lit("||") {
		r, ok := p.parseAnd()
		if !ok {
			return nil, false
		}
		node = &pred{kind: predOr, l: node, r: r}
	}
	return node, true
}

func (p *predParser) parseAnd() (*pred, bool) {
	node, ok := p.parsePrimary()
	if !ok {
		return nil, false
	}
	for p.lit("&&") {
		r, ok := p.parsePrimary()
		if !ok {
			return nil, false
		}
		node = &pred{kind: predAnd, l: node, r: r}
	}
	return node, true
}

func (p *predParser) parsePrimary() (*pred, bool) {
	if p.lit("(") {
		node, ok := p.parseOr()
		if !ok || !p.lit(")") {
			return nil, false
		}
		return node, true
	}
	return p.parseComparison()
}

// parseComparison reads one leaf: `@.path op literal`, `literal op @.path`,
// or a bare `@.path` existence test.
func (p *predParser) parseComparison() (*pred, bool) {
	segs, litOK, leaf := p.parseOperand()
	p.skipSpace()

	var op predKind
	switch {
	case p.lit("=="):
		op = predEq
	case p.lit("!="):
		op = predNe
	default:
		// No operator: a bare path is an existence test; a bare literal is
		// nothing this walker claims.
		if segs == nil {
			return nil, false
		}
		return &pred{kind: predExists, segs: segs}, true
	}

	rsegs, rlitOK, rleaf := p.parseOperand()
	switch {
	case segs != nil && rlitOK:
		rleaf.kind, rleaf.segs = op, segs
		return rleaf, true
	case litOK && rsegs != nil:
		leaf.kind, leaf.segs = op, rsegs
		return leaf, true
	default:
		// Path against path, literal against literal, or an operand the
		// walker does not speak: not walkable.
		return nil, false
	}
}

// parseOperand reads either a dotted @-relative path of plain identifiers
// (segs non-nil) or a walkable literal (ok true, leaf carrying it).
func (p *predParser) parseOperand() (segs []string, ok bool, leaf *pred) {
	p.skipSpace()
	if strings.HasPrefix(p.s[p.i:], "@.") {
		p.i += 2
		start := p.i
		for p.i < len(p.s) {
			c := p.s[p.i]
			if c == ' ' || c == ')' || c == '=' || c == '!' || c == '&' || c == '|' {
				break
			}
			p.i++
		}
		for seg := range strings.SplitSeq(p.s[start:p.i], ".") {
			if !plainIdentifier(seg) {
				return nil, false, nil
			}
			segs = append(segs, seg)
		}
		if segs == nil {
			return nil, false, nil
		}
		return segs, false, nil
	}

	if p.i < len(p.s) && (p.s[p.i] == '\'' || p.s[p.i] == '"') {
		quote := p.s[p.i]
		end := strings.IndexByte(p.s[p.i+1:], quote)
		if end < 0 {
			return nil, false, nil
		}
		s := p.s[p.i+1 : p.i+1+end]
		if strings.ContainsAny(s, `'"\`) {
			return nil, false, nil
		}
		p.i += end + 2
		return nil, true, &pred{lit: litStr, s: s}
	}

	switch {
	case p.litWord("true"):
		return nil, true, &pred{lit: litBool, b: true}
	case p.litWord("false"):
		return nil, true, &pred{lit: litBool, b: false}
	case p.litWord("null"):
		return nil, true, &pred{lit: litNull}
	}
	return nil, false, nil
}

// litWord matches a keyword only when it ends at a token boundary, so the
// path text "nullify" is not read as null.
func (p *predParser) litWord(w string) bool {
	if !strings.HasPrefix(p.s[p.i:], w) {
		return false
	}
	if end := p.i + len(w); end < len(p.s) {
		c := p.s[end]
		if c != ' ' && c != ')' && c != '&' && c != '|' {
			return false
		}
	}
	p.i += len(w)
	return true
}

// plainIdentifier keeps the fast path to keys whose JSONPath spelling is
// unambiguous: anything needing quoting or escaping falls back.
func plainIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
