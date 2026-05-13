package query

import (
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
			if q.Filter == nil {
				t.Fatalf("Filter is nil")
			}
			if q.Filter.Op != tc.wantOp {
				t.Errorf("Op = %v; want %v", q.Filter.Op, tc.wantOp)
			}
			if q.Filter.Value != tc.wantValue {
				t.Errorf("Value = %v (%T); want %v (%T)",
					q.Filter.Value, q.Filter.Value, tc.wantValue, tc.wantValue)
			}
			if q.Filter.Pos == 0 && !strings.HasPrefix(tc.input, "SELECT * WHERE x") {
				// at least a sanity check that Pos is being recorded
				t.Logf("Pos = %d for %q", q.Filter.Pos, tc.input)
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
			if q.Filter == nil || q.Filter.Column != "x" {
				t.Errorf("Filter = %+v; want column 'x'", q.Filter)
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
		{"projection has SUM after comma", "SELECT a, SUM(b)", "aggregation"},
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

func TestParse_FilterPosAnchored(t *testing.T) {
	q, err := Parse("SELECT * WHERE country = 'TR'")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if q.Filter == nil {
		t.Fatalf("Filter is nil")
	}
	// "country" starts at byte offset 15 in "SELECT * WHERE country..."
	if q.Filter.Pos != 15 {
		t.Errorf("Filter.Pos = %d; want 15", q.Filter.Pos)
	}
}
