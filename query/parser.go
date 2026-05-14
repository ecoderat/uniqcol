// Package query implements uniqcol's tiny SQL-like query layer.
//
// Supported grammar (one statement, no AND/OR/JOIN/GROUP BY/expressions):
//
//	SELECT <col1, col2, ... | * | COUNT(*) | SUM(col)>
//	[WHERE <col> <op> <literal>]
//
// FROM is implicit; the segment is supplied at the CLI layer (--db).
// <op> is one of = != < > <= >=. Literals are int, float, or
// single-quoted strings. Keywords are case-insensitive; column names
// are not.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ProjectionKind is the shape of the SELECT clause.
type ProjectionKind int

// Projection kinds.
const (
	// ProjColumns selects a comma-separated list of columns; Columns is populated.
	ProjColumns ProjectionKind = iota
	// ProjStar selects every column (SELECT *); Columns is left nil.
	ProjStar
	// ProjCountStar is COUNT(*).
	ProjCountStar
	// ProjSumColumn is SUM(col); SumColumn is populated.
	ProjSumColumn
)

// FilterOp is the comparison operator in a WHERE clause.
type FilterOp int

// Filter operators.
const (
	OpEq  FilterOp = iota // =
	OpNeq                 // !=
	OpLt                  // <
	OpGt                  // >
	OpLte                 // <=
	OpGte                 // >=
)

// String returns the SQL form of the operator, used in error messages.
func (o FilterOp) String() string {
	switch o {
	case OpEq:
		return "="
	case OpNeq:
		return "!="
	case OpLt:
		return "<"
	case OpGt:
		return ">"
	case OpLte:
		return "<="
	case OpGte:
		return ">="
	default:
		return fmt.Sprintf("FilterOp(%d)", int(o))
	}
}

// Comparison is a leaf in the WHERE expression tree: one column,
// one operator, one literal value.
type Comparison struct {
	Column string
	Op     FilterOp
	// Value is whatever literal the parser saw: int64, float64, or
	// string. The executor type-checks against the column's actual type
	// at run time — no silent coercion.
	Value any
	// Pos is the byte offset of the column-name token in the original
	// input. Used to anchor type-mismatch error messages.
	Pos int
}

// FilterExpr is one node in the WHERE expression tree.
//
// Invariants (enforced by the parser; relied on by the executor):
//  1. EXACTLY ONE of Comparison, And, Or is non-nil.
//  2. And and Or, when non-nil, always have len >= 2. A single-child
//     And/Or is normalized away by the parser — a lone Comparison is
//     stored bare so the executor's three-way dispatch never has to
//     handle a degenerate one-element group.
//
// AND binds tighter than OR. The grammar has no parentheses, so the
// tree is at most two levels deep: typically Or[ And[...], Comp,
// And[...] ].
type FilterExpr struct {
	Comparison *Comparison
	And        []*FilterExpr
	Or         []*FilterExpr
}

// Query is a parsed SELECT statement.
type Query struct {
	Projection ProjectionKind
	Columns    []string    // populated for ProjColumns
	SumColumn  string      // populated for ProjSumColumn
	Where      *FilterExpr // nil if no WHERE
}

// Parse tokenizes and parses input into a Query. Errors include the
// byte offset of the offending token where possible.
func Parse(input string) (*Query, error) {
	toks, err := lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	q, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	if !p.atEOF() {
		t := p.peek()
		return nil, fmt.Errorf("parse error at position %d: unexpected trailing token %q", t.pos, t.text)
	}
	return q, nil
}

// ----- tokenizer -----

type tokKind int

const (
	tkEOF tokKind = iota
	tkIdent
	tkKeyword
	tkInt
	tkFloat
	tkString
	tkOp
	tkComma
	tkLParen
	tkRParen
	tkStar
)

type token struct {
	kind tokKind
	text string
	pos  int
}

var keywords = map[string]struct{}{
	"SELECT": {},
	"WHERE":  {},
	"COUNT":  {},
	"SUM":    {},
	"AND":    {},
	"OR":     {},
	"FROM":   {}, // recognized only so we can reject it with a helpful error
}

func lex(input string) ([]token, error) {
	var out []token
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == ',':
			out = append(out, token{tkComma, ",", i})
			i++
		case c == '(':
			out = append(out, token{tkLParen, "(", i})
			i++
		case c == ')':
			out = append(out, token{tkRParen, ")", i})
			i++
		case c == '*':
			out = append(out, token{tkStar, "*", i})
			i++
		case c == '=':
			out = append(out, token{tkOp, "=", i})
			i++
		case c == '!':
			if i+1 < len(input) && input[i+1] == '=' {
				out = append(out, token{tkOp, "!=", i})
				i += 2
				continue
			}
			return nil, fmt.Errorf("parse error at position %d: stray '!' (did you mean !=?)", i)
		case c == '<', c == '>':
			if i+1 < len(input) && input[i+1] == '=' {
				out = append(out, token{tkOp, string([]byte{c, '='}), i})
				i += 2
				continue
			}
			out = append(out, token{tkOp, string(c), i})
			i++
		case c == '\'':
			start := i
			i++ // skip opening quote
			j := i
			for j < len(input) && input[j] != '\'' {
				j++
			}
			if j >= len(input) {
				return nil, fmt.Errorf("parse error at position %d: unterminated string literal", start)
			}
			out = append(out, token{tkString, input[i:j], start})
			i = j + 1 // skip closing quote
		case isDigit(c) || (c == '-' && i+1 < len(input) && isDigit(input[i+1])):
			start := i
			if c == '-' {
				i++
			}
			for i < len(input) && isDigit(input[i]) {
				i++
			}
			isFloat := false
			if i < len(input) && input[i] == '.' {
				isFloat = true
				i++
				for i < len(input) && isDigit(input[i]) {
					i++
				}
			}
			text := input[start:i]
			if isFloat {
				out = append(out, token{tkFloat, text, start})
			} else {
				out = append(out, token{tkInt, text, start})
			}
		case isIdentStart(c):
			start := i
			for i < len(input) && isIdentPart(input[i]) {
				i++
			}
			text := input[start:i]
			upper := strings.ToUpper(text)
			if _, ok := keywords[upper]; ok {
				out = append(out, token{tkKeyword, upper, start})
			} else {
				out = append(out, token{tkIdent, text, start})
			}
		default:
			return nil, fmt.Errorf("parse error at position %d: unexpected character %q", i, c)
		}
	}
	out = append(out, token{tkEOF, "", len(input)})
	return out, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// ----- parser -----

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) advance() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) atEOF() bool { return p.toks[p.i].kind == tkEOF }

func (p *parser) parseQuery() (*Query, error) {
	t := p.advance()
	if t.kind != tkKeyword || t.text != "SELECT" {
		return nil, fmt.Errorf("parse error at position %d: expected SELECT, got %q", t.pos, displayToken(t))
	}
	q := &Query{}
	if err := p.parseProjection(q); err != nil {
		return nil, err
	}
	// Optional WHERE / reject FROM.
	t = p.peek()
	if t.kind == tkKeyword && t.text == "FROM" {
		return nil, fmt.Errorf("parse error at position %d: unexpected FROM clause (segment is specified via --db)", t.pos)
	}
	if t.kind == tkKeyword && t.text == "WHERE" {
		p.advance()
		expr, err := p.parseWhere()
		if err != nil {
			return nil, err
		}
		q.Where = expr
	}
	return q, nil
}

// parseWhere parses one or more comparisons joined by AND / OR and
// applies SQL precedence (AND binds tighter than OR). Returns a
// FilterExpr whose shape is:
//   - a bare Comparison when there is exactly one condition,
//   - an And when all operators are AND (length >= 2),
//   - an Or otherwise, with each child either a single Comparison or
//     an And group.
//
// Parentheses, NOT, and column-to-column comparisons are not supported;
// each yields a clear parse error rather than a silent misinterpretation.
func (p *parser) parseWhere() (*FilterExpr, error) {
	// Pass 1: lex the condition list. comps[i] is joined to comps[i+1]
	// by ops[i] ("AND" or "OR").
	var comps []*Comparison
	var ops []string

	first, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	comps = append(comps, first)
	for {
		t := p.peek()
		if t.kind == tkEOF {
			break
		}
		if t.kind == tkKeyword && (t.text == "AND" || t.text == "OR") {
			p.advance()
			next, err := p.parseComparison()
			if err != nil {
				return nil, err
			}
			ops = append(ops, t.text)
			comps = append(comps, next)
			continue
		}
		// Anything else here is either a normal end-of-WHERE (trailing
		// junk handled by parseQuery's tail check) or two adjacent
		// conditions with no operator between them. Catch the latter
		// explicitly because it's a common mistake.
		if t.kind == tkIdent {
			return nil, fmt.Errorf("parse error at position %d: expected comparison operator (=, !=, <, >, <=, >=) or AND/OR before %q",
				t.pos, t.text)
		}
		break
	}

	// Pass 2: group consecutive AND-joined comparisons.
	type group struct {
		members []*Comparison
	}
	var groups []group
	cur := group{members: []*Comparison{comps[0]}}
	for i, op := range ops {
		if op == "AND" {
			cur.members = append(cur.members, comps[i+1])
		} else {
			groups = append(groups, cur)
			cur = group{members: []*Comparison{comps[i+1]}}
		}
	}
	groups = append(groups, cur)

	// Pass 3: lower groups to FilterExpr nodes, then OR-join.
	groupNodes := make([]*FilterExpr, 0, len(groups))
	for _, g := range groups {
		if len(g.members) == 1 {
			groupNodes = append(groupNodes, &FilterExpr{Comparison: g.members[0]})
			continue
		}
		children := make([]*FilterExpr, len(g.members))
		for i, c := range g.members {
			children[i] = &FilterExpr{Comparison: c}
		}
		groupNodes = append(groupNodes, &FilterExpr{And: children})
	}
	if len(groupNodes) == 1 {
		return groupNodes[0], nil
	}
	return &FilterExpr{Or: groupNodes}, nil
}

func (p *parser) parseProjection(q *Query) error {
	t := p.peek()
	switch {
	case t.kind == tkStar:
		p.advance()
		q.Projection = ProjStar
		return nil
	case t.kind == tkKeyword && t.text == "COUNT":
		p.advance()
		if err := p.expectKind(tkLParen, "'('"); err != nil {
			return err
		}
		next := p.advance()
		if next.kind != tkStar {
			return fmt.Errorf("parse error at position %d: expected '*' in COUNT(*), got %q", next.pos, displayToken(next))
		}
		if err := p.expectKind(tkRParen, "')'"); err != nil {
			return err
		}
		q.Projection = ProjCountStar
		return nil
	case t.kind == tkKeyword && t.text == "SUM":
		p.advance()
		if err := p.expectKind(tkLParen, "'('"); err != nil {
			return err
		}
		next := p.advance()
		if next.kind != tkIdent {
			return fmt.Errorf("parse error at position %d: expected column name inside SUM(...), got %q", next.pos, displayToken(next))
		}
		q.Projection = ProjSumColumn
		q.SumColumn = next.text
		if err := p.expectKind(tkRParen, "')'"); err != nil {
			return err
		}
		return nil
	case t.kind == tkIdent:
		// Column list.
		first := p.advance()
		q.Projection = ProjColumns
		q.Columns = []string{first.text}
		for p.peek().kind == tkComma {
			p.advance()
			n := p.advance()
			if n.kind != tkIdent {
				// Helpful messages for common slips.
				if n.kind == tkStar {
					return fmt.Errorf("parse error at position %d: expected column name, got '*' (cannot mix column projection with '*')", n.pos)
				}
				if n.kind == tkKeyword && (n.text == "COUNT" || n.text == "SUM") {
					return fmt.Errorf("parse error at position %d: cannot mix column projection with aggregation", n.pos)
				}
				return fmt.Errorf("parse error at position %d: expected column name, got %q", n.pos, displayToken(n))
			}
			q.Columns = append(q.Columns, n.text)
		}
		return nil
	default:
		return fmt.Errorf("parse error at position %d: expected column name, '*', COUNT(*) or SUM(col), got %q",
			t.pos, displayToken(t))
	}
}

// parseComparison parses one leaf `col op literal` predicate. Parens
// are rejected up front with a specific message — they're not part of
// the supported grammar.
func (p *parser) parseComparison() (*Comparison, error) {
	col := p.advance()
	if col.kind == tkLParen {
		return nil, fmt.Errorf("parse error at position %d: parentheses are not supported in WHERE clauses", col.pos)
	}
	if col.kind != tkIdent {
		return nil, fmt.Errorf("parse error at position %d: expected column name after WHERE, got %q", col.pos, displayToken(col))
	}
	opTok := p.advance()
	if opTok.kind != tkOp {
		// Two adjacent comparisons with no AND/OR between them lands here.
		if opTok.kind == tkIdent {
			return nil, fmt.Errorf("parse error at position %d: expected comparison operator (=, !=, <, >, <=, >=) or AND/OR before %q", opTok.pos, opTok.text)
		}
		return nil, fmt.Errorf("parse error at position %d: expected comparison operator, got %q", opTok.pos, displayToken(opTok))
	}
	op, err := parseOp(opTok.text)
	if err != nil {
		return nil, fmt.Errorf("parse error at position %d: %s", opTok.pos, err.Error())
	}
	lit := p.advance()
	val, err := literalValue(lit)
	if err != nil {
		return nil, err
	}
	return &Comparison{Column: col.text, Op: op, Value: val, Pos: col.pos}, nil
}

func (p *parser) expectKind(k tokKind, descr string) error {
	t := p.advance()
	if t.kind != k {
		return fmt.Errorf("parse error at position %d: expected %s, got %q", t.pos, descr, displayToken(t))
	}
	return nil
}

func parseOp(text string) (FilterOp, error) {
	switch text {
	case "=":
		return OpEq, nil
	case "!=":
		return OpNeq, nil
	case "<":
		return OpLt, nil
	case ">":
		return OpGt, nil
	case "<=":
		return OpLte, nil
	case ">=":
		return OpGte, nil
	default:
		return 0, fmt.Errorf("unknown operator %q", text)
	}
}

func literalValue(t token) (any, error) {
	switch t.kind {
	case tkInt:
		v, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse error at position %d: bad integer literal %q: %v", t.pos, t.text, err)
		}
		return v, nil
	case tkFloat:
		v, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("parse error at position %d: bad float literal %q: %v", t.pos, t.text, err)
		}
		return v, nil
	case tkString:
		return t.text, nil
	default:
		return nil, fmt.Errorf("parse error at position %d: expected literal (int, float, or 'string'), got %q",
			t.pos, displayToken(t))
	}
}

func displayToken(t token) string {
	if t.kind == tkEOF {
		return "<end of input>"
	}
	return t.text
}
