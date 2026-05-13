package query

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ecoderat/uniqcol/storage"
)

func buildTestSegment(t *testing.T) *storage.Segment {
	t.Helper()
	schema := storage.Schema{
		PK: "id",
		Columns: []storage.Column{
			{Name: "id", Type: storage.Int64},
			{Name: "amount", Type: storage.Float64},
			{Name: "country", Type: storage.String},
		},
	}
	tbl, err := storage.CreateTable(schema, storage.TableOptions{
		BloomExpectedItems: 100, BloomTargetFPR: 0.01,
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	rows := []storage.Row{
		{int64(1), 10.0, "TR"},
		{int64(2), 20.0, "US"},
		{int64(3), 30.0, "TR"},
		{int64(4), 40.0, "DE"},
		{int64(5), 50.0, "TR"},
	}
	for _, r := range rows {
		if got := tbl.Insert(r); !got.Accepted {
			t.Fatalf("setup: %s", got.Reason)
		}
	}
	path := filepath.Join(t.TempDir(), "x.uniq")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tbl.Flush(f); err != nil {
		_ = f.Close()
		t.Fatalf("flush: %v", err)
	}
	_ = f.Close()
	seg, err := storage.OpenSegment(path)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	t.Cleanup(func() { seg.Close() })
	return seg
}

func mustParse(t *testing.T, s string) *Query {
	t.Helper()
	q, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return q
}

func TestExecute_SelectStar(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT *"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Columns) != 3 {
		t.Errorf("Columns = %v; want 3", r.Columns)
	}
	if len(r.Rows) != 5 {
		t.Errorf("Rows = %d; want 5", len(r.Rows))
	}
	if r.IsAggregate() {
		t.Errorf("IsAggregate() = true; want false")
	}
}

func TestExecute_SelectColumns(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT amount, country"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Columns) != 2 || r.Columns[0] != "amount" || r.Columns[1] != "country" {
		t.Errorf("Columns = %v; want [amount country]", r.Columns)
	}
	if len(r.Rows) != 5 {
		t.Errorf("Rows = %d; want 5", len(r.Rows))
	}
	for i, row := range r.Rows {
		if _, ok := row[0].(float64); !ok {
			t.Errorf("row %d[0] type = %T; want float64", i, row[0])
		}
		if _, ok := row[1].(string); !ok {
			t.Errorf("row %d[1] type = %T; want string", i, row[1])
		}
	}
}

func TestExecute_FilteredProjection(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT id, amount WHERE country = 'TR'"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 3 {
		t.Errorf("Rows = %d; want 3 (TR appears 3 times)", len(r.Rows))
	}
	wantIDs := map[int64]bool{1: true, 3: true, 5: true}
	for _, row := range r.Rows {
		id := row[0].(int64)
		if !wantIDs[id] {
			t.Errorf("unexpected id in result: %d", id)
		}
	}
}

func TestExecute_AllFilterOps(t *testing.T) {
	seg := buildTestSegment(t)
	cases := []struct {
		sql    string
		wantN  int
		wantOp string
	}{
		{"SELECT id WHERE id = 3", 1, "eq"},
		{"SELECT id WHERE id != 3", 4, "neq"},
		{"SELECT id WHERE id < 3", 2, "lt"},
		{"SELECT id WHERE id > 3", 2, "gt"},
		{"SELECT id WHERE id <= 3", 3, "lte"},
		{"SELECT id WHERE id >= 3", 3, "gte"},
		{"SELECT id WHERE amount > 25.0", 3, "float gt"},
		{"SELECT id WHERE country < 'TR'", 1, "string lt"}, // only "DE" < "TR" lexicographically
	}
	for _, tc := range cases {
		t.Run(tc.wantOp, func(t *testing.T) {
			r, err := Execute(seg, mustParse(t, tc.sql))
			if err != nil {
				t.Fatalf("Execute(%q): %v", tc.sql, err)
			}
			if len(r.Rows) != tc.wantN {
				t.Errorf("rows = %d; want %d", len(r.Rows), tc.wantN)
			}
		})
	}
}

func TestExecute_CountStar(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT COUNT(*)"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !r.IsAggregate() {
		t.Errorf("IsAggregate() = false")
	}
	if r.Rows[0][0].(uint64) != 5 {
		t.Errorf("count = %v; want 5", r.Rows[0][0])
	}
	if r.Columns[0] != "COUNT(*)" {
		t.Errorf("header = %q; want COUNT(*)", r.Columns[0])
	}
}

func TestExecute_CountStarFiltered(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT COUNT(*) WHERE country = 'TR'"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Rows[0][0].(uint64) != 3 {
		t.Errorf("count = %v; want 3", r.Rows[0][0])
	}
}

func TestExecute_SumFloat64(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT SUM(amount)"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !r.IsAggregate() {
		t.Errorf("IsAggregate() = false")
	}
	got := r.Rows[0][0].(float64)
	want := 10.0 + 20.0 + 30.0 + 40.0 + 50.0
	if got != want {
		t.Errorf("sum = %v; want %v", got, want)
	}
	if r.Columns[0] != "SUM(amount)" {
		t.Errorf("header = %q", r.Columns[0])
	}
}

func TestExecute_SumInt64(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT SUM(id)"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := r.Rows[0][0].(int64)
	if got != 1+2+3+4+5 {
		t.Errorf("sum = %d; want 15", got)
	}
}

func TestExecute_SumFiltered(t *testing.T) {
	seg := buildTestSegment(t)
	r, err := Execute(seg, mustParse(t, "SELECT SUM(amount) WHERE country = 'TR'"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := r.Rows[0][0].(float64)
	if got != 10.0+30.0+50.0 {
		t.Errorf("sum = %v; want 90.0", got)
	}
}

func TestExecute_SumOverflow(t *testing.T) {
	// Build a segment with int64 values that sum past int64 range.
	schema := storage.Schema{
		PK:      "id",
		Columns: []storage.Column{{Name: "id", Type: storage.Int64}, {Name: "big", Type: storage.Int64}},
	}
	tbl, err := storage.CreateTable(schema, storage.TableOptions{
		BloomExpectedItems: 10, BloomTargetFPR: 0.01,
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	// Two values each near MaxInt64; second one overflows.
	if got := tbl.Insert(storage.Row{int64(1), int64(math.MaxInt64 - 1)}); !got.Accepted {
		t.Fatalf("setup: %s", got.Reason)
	}
	if got := tbl.Insert(storage.Row{int64(2), int64(math.MaxInt64 - 1)}); !got.Accepted {
		t.Fatalf("setup: %s", got.Reason)
	}
	path := filepath.Join(t.TempDir(), "x.uniq")
	f, _ := os.Create(path)
	if err := tbl.Flush(f); err != nil {
		_ = f.Close()
		t.Fatalf("flush: %v", err)
	}
	_ = f.Close()
	seg, err := storage.OpenSegment(path)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()

	_, err = Execute(seg, mustParse(t, "SELECT SUM(big)"))
	if !errors.Is(err, ErrSumOverflow) {
		t.Fatalf("err = %v; want ErrSumOverflow", err)
	}
}

func TestExecute_SumOnString(t *testing.T) {
	seg := buildTestSegment(t)
	_, err := Execute(seg, mustParse(t, "SELECT SUM(country)"))
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("err = %v; want ErrTypeMismatch", err)
	}
}

func TestExecute_UnknownColumns(t *testing.T) {
	seg := buildTestSegment(t)
	for _, sql := range []string{
		"SELECT nope",
		"SELECT id WHERE nope = 1",
		"SELECT SUM(nope)",
	} {
		_, err := Execute(seg, mustParse(t, sql))
		if !errors.Is(err, ErrUnknownColumn) {
			t.Errorf("Execute(%q): err = %v; want ErrUnknownColumn", sql, err)
		}
	}
}

func TestExecute_FilterTypeMismatches(t *testing.T) {
	seg := buildTestSegment(t)
	cases := []struct {
		sql     string
		wantSub string
	}{
		{"SELECT id WHERE id = 'TR'", "is int64; literal must be int64"},
		{"SELECT id WHERE amount = 50", "is float64; literal must be float64"},
		{"SELECT id WHERE country = 1", "is string; literal must be a single-quoted"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			_, err := Execute(seg, mustParse(t, tc.sql))
			if !errors.Is(err, ErrTypeMismatch) {
				t.Fatalf("err = %v; want ErrTypeMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}
