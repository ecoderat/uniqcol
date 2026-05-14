package query

import (
	"fmt"
	"strings"
	"testing"
)

func TestParse_ProjectionKinds(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantKind  ProjectionKind
		wantCols  []string
		wantSum   string
		wantWhere bool
	}{
		{"single column", "SELECT id", ProjColumns, []string{"id"}, "", false},
		{"two columns", "SELECT a, b", ProjColumns, []string{"a", "b"}, "", false},
		{"three columns spaced", "SELECT  a , b ,  c  ", ProjColumns, []string{"a", "b", "c"}, "", false},
		{"star", "SELECT *", ProjStar, nil, "", false},
		{"count star", "SELECT COUNT(*)", ProjCountStar, nil, "", false},
		{"count lowercase", "select count(*)", ProjCountStar, nil, "", false},
		{"sum col", "SELECT SUM(amount)", ProjSumColumn, nil, "amount", false},
		{"sum lowercase", "select sum(amount)", ProjSumColumn, nil, "amount", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if q.Projection != tc.wantKind {
				t.Errorf("Projection = %v; want %v", q.Projection, tc.wantKind)
			}
			if len(tc.wantCols) > 0 {
				if got := strings.Join(q.Columns, ","); got != strings.Join(tc.wantCols, ",") {
					t.Errorf("Columns = %v; want %v", q.Columns, tc.wantCols)
				}
			} else if q.Columns != nil {
				t.Errorf("Columns = %v; want nil", q.Columns)
			}
			if q.SumColumn != tc.wantSum {
				t.Errorf("SumColumn = %q; want %q", q.SumColumn, tc.wantSum)
			}
		})
	}
}

func TestParse_FilterOpsAndLiterals(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOp    FilterOp
		wantValue any
	}{
		{"eq int", "SELECT * WHERE x = 42", OpEq, int64(42)},
		{"eq negative int", "SELECT * WHERE x = -5", OpEq, int64(-5)},
		{"neq int", "SELECT a WHERE x != 1", OpNeq, int64(1)},
		{"lt", "SELECT a WHERE x < 2", OpLt, int64(2)},
		{"gt float", "SELECT a WHERE x > 3.14", OpGt, 3.14},
		{"lte string", "SELECT a WHERE country <= 'US'", OpLte, "US"},
		{"gte zero", "SELECT a WHERE x >= 0", OpGte, int64(0)},
		{"float zero", "SELECT a WHERE x = 0.0", OpEq, 0.0},
		{"empty string literal", "SELECT a WHERE tag = ''", OpEq, ""},
		{"utf8 string literal", "SELECT a WHERE country = '世界'", OpEq, "世界"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if q.Where == nil {
				t.Fatalf("Where is nil")
			}
			c := q.Where.Comparison
			if c == nil {
				t.Fatalf("Where is not a bare Comparison: %+v", q.Where)
			}
			if c.Op != tc.wantOp {
				t.Errorf("Op = %v; want %v", c.Op, tc.wantOp)
			}
			if c.Value != tc.wantValue {
				t.Errorf("Value = %v (%T); want %v (%T)",
					c.Value, c.Value, tc.wantValue, tc.wantValue)
			}
			if c.Pos == 0 && !strings.HasPrefix(tc.input, "SELECT * WHERE x") {
				// at least a sanity check that Pos is being recorded
				t.Logf("Pos = %d for %q", c.Pos, tc.input)
			}
		})
	}
}

func TestParse_WhitespaceAndCaseInsensitive(t *testing.T) {
	cases := []string{
		"SELECT id WHERE x = 1",
		"  SELECT  id  WHERE  x  =  1  ",
		"select id where x = 1",
		"sElEcT id WhErE x = 1",
		"SELECT\tid\tWHERE\tx\t=\t1",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			q, err := Parse(c)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c, err)
			}
			if len(q.Columns) != 1 || q.Columns[0] != "id" {
				t.Errorf("Columns = %v; want [id]", q.Columns)
			}
			if q.Where == nil || q.Where.Comparison == nil || q.Where.Comparison.Column != "x" {
				t.Errorf("Where = %+v; want bare Comparison on 'x'", q.Where)
			}
		})
	}
}

func TestParse_ColumnNamesAreCaseSensitive(t *testing.T) {
	q, err := Parse("SELECT Event_ID, USER_id")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if q.Columns[0] != "Event_ID" || q.Columns[1] != "USER_id" {
		t.Errorf("Columns = %v; want preserved case", q.Columns)
	}
}

func TestParse_ErrorMessages(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{"not SELECT", "INSERT id", "expected SELECT"},
		{"FROM rejected", "SELECT id FROM events", "unexpected FROM clause"},
		{"projection after comma is star", "SELECT a, *", "cannot mix"},
		{"grouped projection without GROUP BY", "SELECT a, SUM(b)", "requires a GROUP BY clause"},
		{"empty input", "", "expected SELECT"},
		{"select then nothing", "SELECT", "expected column name"},
		{"missing rparen on count", "SELECT COUNT(*", "expected ')'"},
		{"count without star", "SELECT COUNT(1)", "expected '*'"},
		{"sum without col", "SELECT SUM(*)", "expected column name"},
		{"where without column", "SELECT * WHERE = 1", "expected column name after WHERE"},
		{"where missing op", "SELECT * WHERE x 1", "comparison operator"},
		{"unknown op stray bang", "SELECT * WHERE x !1", "did you mean !=?"},
		{"unterminated string", "SELECT * WHERE x = 'unclosed", "unterminated string"},
		{"trailing junk", "SELECT id BANANA", "unexpected trailing token"},
		{"stray character", "SELECT id WHERE x = @1", "unexpected character"},
		{"bad literal in where", "SELECT a WHERE x = ,", "expected literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q; want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestFilterOp_String(t *testing.T) {
	cases := map[FilterOp]string{
		OpEq:         "=",
		OpNeq:        "!=",
		OpLt:         "<",
		OpGt:         ">",
		OpLte:        "<=",
		OpGte:        ">=",
		FilterOp(99): "FilterOp(99)",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("FilterOp(%d).String() = %q; want %q", op, got, want)
		}
	}
}

// shape returns a stable text representation of a FilterExpr tree so
// tests can assert the exact structure (not just "no parse error").
//
// Examples:
//
//	Cmp(a=1)
//	And[ Cmp(a=1), Cmp(b=2) ]
//	Or[ And[ Cmp(a=1), Cmp(b=2) ], Cmp(c=3) ]
func shape(e *FilterExpr) string {
	if e == nil {
		return "<nil>"
	}
	if e.Comparison != nil {
		c := e.Comparison
		return fmt.Sprintf("Cmp(%s%s%v)", c.Column, c.Op, c.Value)
	}
	if e.And != nil {
		parts := make([]string, len(e.And))
		for i, ch := range e.And {
			parts[i] = shape(ch)
		}
		return "And[ " + strings.Join(parts, ", ") + " ]"
	}
	if e.Or != nil {
		parts := make([]string, len(e.Or))
		for i, ch := range e.Or {
			parts[i] = shape(ch)
		}
		return "Or[ " + strings.Join(parts, ", ") + " ]"
	}
	return "<empty>"
}

func TestParse_WhereTreeShape(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantShape string
	}{
		{
			name:      "single condition is a bare Comparison",
			input:     "SELECT * WHERE a = 1",
			wantShape: "Cmp(a=1)",
		},
		{
			name:      "two AND",
			input:     "SELECT * WHERE a = 1 AND b = 2",
			wantShape: "And[ Cmp(a=1), Cmp(b=2) ]",
		},
		{
			name:      "two OR",
			input:     "SELECT * WHERE a = 1 OR b = 2",
			wantShape: "Or[ Cmp(a=1), Cmp(b=2) ]",
		},
		{
			name:      "three AND",
			input:     "SELECT * WHERE a = 1 AND b = 2 AND c = 3",
			wantShape: "And[ Cmp(a=1), Cmp(b=2), Cmp(c=3) ]",
		},
		{
			name:      "three OR",
			input:     "SELECT * WHERE a = 1 OR b = 2 OR c = 3",
			wantShape: "Or[ Cmp(a=1), Cmp(b=2), Cmp(c=3) ]",
		},
		{
			// AND binds tighter than OR.
			name:      "mixed AND-then-OR",
			input:     "SELECT * WHERE a = 1 AND b = 2 OR c = 3",
			wantShape: "Or[ And[ Cmp(a=1), Cmp(b=2) ], Cmp(c=3) ]",
		},
		{
			name:      "mixed OR-then-AND",
			input:     "SELECT * WHERE a = 1 OR b = 2 AND c = 3",
			wantShape: "Or[ Cmp(a=1), And[ Cmp(b=2), Cmp(c=3) ] ]",
		},
		{
			// The case that proves AND-grouping collects MULTIPLE runs,
			// not just a prefix.
			name:      "four conditions, AND-OR-AND",
			input:     "SELECT * WHERE a = 1 AND b = 2 OR c = 3 AND d = 4",
			wantShape: "Or[ And[ Cmp(a=1), Cmp(b=2) ], And[ Cmp(c=3), Cmp(d=4) ] ]",
		},
		{
			name:      "case insensitive: lowercase and/or",
			input:     "SELECT * WHERE a = 1 and b = 2 or c = 3",
			wantShape: "Or[ And[ Cmp(a=1), Cmp(b=2) ], Cmp(c=3) ]",
		},
		{
			name:      "case insensitive: mixed-case And/Or",
			input:     "SELECT * WHERE a = 1 And b = 2 oR c = 3",
			wantShape: "Or[ And[ Cmp(a=1), Cmp(b=2) ], Cmp(c=3) ]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}
			if q.Where == nil {
				t.Fatalf("Where is nil")
			}
			got := shape(q.Where)
			if got != tc.wantShape {
				t.Fatalf("tree shape mismatch for %q\n got: %s\nwant: %s",
					tc.input, got, tc.wantShape)
			}
			// Defensive: a top-level And/Or must never have <2 children.
			assertNoDegenerate(t, q.Where)
		})
	}
}

func assertNoDegenerate(t *testing.T, e *FilterExpr) {
	t.Helper()
	if e == nil {
		return
	}
	if e.And != nil && len(e.And) < 2 {
		t.Fatalf("invariant violated: And with %d children (must be >=2)", len(e.And))
	}
	if e.Or != nil && len(e.Or) < 2 {
		t.Fatalf("invariant violated: Or with %d children (must be >=2)", len(e.Or))
	}
	for _, ch := range e.And {
		assertNoDegenerate(t, ch)
	}
	for _, ch := range e.Or {
		assertNoDegenerate(t, ch)
	}
}

func TestParse_MultiConditionErrors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{"trailing AND", "SELECT a WHERE a = 1 AND", "expected column name"},
		{"trailing OR", "SELECT a WHERE a = 1 OR", "expected column name"},
		{"parens rejected", "SELECT * WHERE (a = 1)", "parentheses are not supported"},
		{"parens after AND", "SELECT * WHERE a = 1 AND (b = 2)", "parentheses are not supported"},
		{"missing operator between conditions", "SELECT * WHERE a = 1 b = 2", "expected comparison operator (=, !=, <, >, <=, >=) or AND/OR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q; want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestParse_FilterPosAnchored(t *testing.T) {
	q, err := Parse("SELECT * WHERE country = 'TR'")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if q.Where == nil || q.Where.Comparison == nil {
		t.Fatalf("Where = %+v; want bare Comparison", q.Where)
	}
	// "country" starts at byte offset 15 in "SELECT * WHERE country..."
	if q.Where.Comparison.Pos != 15 {
		t.Errorf("Comparison.Pos = %d; want 15", q.Where.Comparison.Pos)
	}
}

func TestParse_GroupBy_Valid(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantGroup string
		wantAgg   AggregateKind
		wantSum   string
		wantWhere bool
	}{
		{
			name:      "grouped COUNT(*)",
			input:     "SELECT country, COUNT(*) GROUP BY country",
			wantGroup: "country",
			wantAgg:   AggCountStar,
		},
		{
			name:      "grouped SUM(col)",
			input:     "SELECT country, SUM(amount) GROUP BY country",
			wantGroup: "country",
			wantAgg:   AggSumColumn,
			wantSum:   "amount",
		},
		{
			name:      "grouped with WHERE before GROUP BY",
			input:     "SELECT country, COUNT(*) WHERE amount > 10.0 GROUP BY country",
			wantGroup: "country",
			wantAgg:   AggCountStar,
			wantWhere: true,
		},
		{
			name:      "case-insensitive group by",
			input:     "select country, count(*) group by country",
			wantGroup: "country",
			wantAgg:   AggCountStar,
		},
		{
			name:      "mixed-case Group By",
			input:     "SELECT country, COUNT(*) Group By country",
			wantGroup: "country",
			wantAgg:   AggCountStar,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if q.Projection != ProjGrouped {
				t.Errorf("Projection = %v; want ProjGrouped", q.Projection)
			}
			if q.GroupBy != tc.wantGroup {
				t.Errorf("GroupBy = %q; want %q", q.GroupBy, tc.wantGroup)
			}
			if q.GroupAgg != tc.wantAgg {
				t.Errorf("GroupAgg = %v; want %v", q.GroupAgg, tc.wantAgg)
			}
			if q.SumColumn != tc.wantSum {
				t.Errorf("SumColumn = %q; want %q", q.SumColumn, tc.wantSum)
			}
			if (q.Where != nil) != tc.wantWhere {
				t.Errorf("Where presence = %v; want %v", q.Where != nil, tc.wantWhere)
			}
			// Columns must still hold the literal SELECT projection for
			// diagnostics, but the executor reads GroupBy.
			if len(q.Columns) != 1 || q.Columns[0] != tc.wantGroup {
				t.Errorf("Columns = %v; want %q in slot 0", q.Columns, tc.wantGroup)
			}
		})
	}
}

func TestParse_GroupBy_MalformedCases(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{
			"no aggregate",
			"SELECT country GROUP BY country",
			"GROUP BY requires exactly one aggregate in the projection",
		},
		{
			"no group column in projection",
			"SELECT COUNT(*) GROUP BY country",
			"GROUP BY column must appear in the projection",
		},
		{
			"SELECT * with GROUP BY",
			"SELECT * GROUP BY country",
			"GROUP BY column must appear in the projection",
		},
		{
			"SELECT SUM(col) with GROUP BY",
			"SELECT SUM(amount) GROUP BY country",
			"GROUP BY column must appear in the projection",
		},
		{
			"extra column before aggregate",
			"SELECT country, user_id, COUNT(*) GROUP BY country",
			"GROUP BY projection must be exactly: group column, then one aggregate",
		},
		{
			"two aggregates",
			"SELECT country, COUNT(*), SUM(amount) GROUP BY country",
			"GROUP BY projection must be exactly: group column, then one aggregate",
		},
		{
			"GROUP BY column mismatches projection",
			"SELECT country, COUNT(*) GROUP BY user_id",
			`GROUP BY column "user_id" does not match projected column "country"`,
		},
		{
			"multi-column GROUP BY rejected",
			"SELECT country, COUNT(*) GROUP BY country, user_id",
			"only single-column GROUP BY is supported",
		},
		{
			"HAVING rejected",
			"SELECT country, COUNT(*) GROUP BY country HAVING COUNT(*) > 1",
			"HAVING is not supported",
		},
		{
			"GROUP missing BY",
			"SELECT country, COUNT(*) GROUP country",
			"expected BY after GROUP",
		},
		{
			"GROUP BY missing column",
			"SELECT country, COUNT(*) GROUP BY",
			"expected column name after GROUP BY",
		},
		{
			"grouped projection without GROUP BY",
			"SELECT country, COUNT(*)",
			"requires a GROUP BY clause",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q; want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}
