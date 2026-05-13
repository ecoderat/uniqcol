package query

import (
	"errors"
	"fmt"

	"github.com/ecoderat/uniqcol/storage"
)

// Sentinel errors. Distinguishable with errors.Is.
//
// Messages intentionally omit the "query:" prefix; the CLI layer wraps
// them with its own subcommand context, and stdlib convention is to let
// callers compose context rather than baking it into the sentinel.
var (
	// ErrUnknownColumn is returned when a query references a column not
	// in the segment's schema.
	ErrUnknownColumn = errors.New("unknown column")
	// ErrTypeMismatch is returned when a filter literal's type does not
	// match the filter column's type, or SUM is applied to a string.
	ErrTypeMismatch = errors.New("type mismatch")
	// ErrSumOverflow is returned by SUM on int64 when the running sum
	// exceeds the int64 range.
	ErrSumOverflow = errors.New("sum overflow")
)

// Result is the materialized output of Execute.
//
// TODO(future): for very large segments, [][]any wastes ~24 bytes per
// scalar. A columnar result type ([]any of typed slices) would cut that
// by ~10x at the cost of a more complex CLI renderer.
type Result struct {
	// Columns are the result's column headers. For aggregates, this is
	// a single-element slice like ["COUNT(*)"] or ["SUM(amount)"].
	Columns []string
	// Rows are the selected rows for projections, or a single one-cell
	// row for aggregates.
	Rows [][]any

	isAggregate bool
}

// IsAggregate reports whether the Result came from a COUNT(*) or
// SUM(col) query. Aggregates always have exactly one Row with exactly
// one cell; the CLI uses this to skip the "(showing N of M)" footer.
func (r *Result) IsAggregate() bool { return r.isAggregate }

// Execute runs q against seg.
func Execute(seg *storage.Segment, q *Query) (*Result, error) {
	schema := seg.Schema()

	// 1. Resolve projection column names.
	var projCols []string
	switch q.Projection {
	case ProjColumns:
		projCols = q.Columns
	case ProjStar:
		projCols = make([]string, len(schema.Columns))
		for i, c := range schema.Columns {
			projCols[i] = c.Name
		}
	case ProjSumColumn:
		projCols = []string{q.SumColumn}
	case ProjCountStar:
		// no projection columns to read for the count itself
	}
	for _, name := range projCols {
		if _, ok := schema.ColumnIndex(name); !ok {
			return nil, fmt.Errorf("%w: %q (projection)", ErrUnknownColumn, name)
		}
	}

	// 2. If there's a filter, validate column + literal type.
	var (
		selection []bool
		filtered  bool
	)
	if q.Filter != nil {
		f := q.Filter
		idx, ok := schema.ColumnIndex(f.Column)
		if !ok {
			return nil, fmt.Errorf("%w: %q (in WHERE)", ErrUnknownColumn, f.Column)
		}
		colType := schema.Columns[idx].Type
		if err := checkLiteralType(f, colType); err != nil {
			return nil, err
		}
		v, err := seg.ReadColumn(f.Column)
		if err != nil {
			return nil, fmt.Errorf("query: read filter column %q: %w", f.Column, err)
		}
		selection, err = applyFilter(v, colType, f)
		if err != nil {
			return nil, err
		}
		filtered = true
	}

	// 3. Dispatch on projection kind.
	switch q.Projection {
	case ProjCountStar:
		return execCount(seg, selection, filtered), nil
	case ProjSumColumn:
		return execSum(seg, q.SumColumn, schema, selection, filtered)
	case ProjColumns, ProjStar:
		return execProject(seg, projCols, selection, filtered)
	default:
		return nil, fmt.Errorf("query: unknown projection kind %d", q.Projection)
	}
}

// checkLiteralType ensures the parser's literal type matches the column's
// declared type. No silent coercion — see Filter doc.
func checkLiteralType(f *Filter, colType storage.ColumnType) error {
	switch colType {
	case storage.Int64:
		if _, ok := f.Value.(int64); !ok {
			return fmt.Errorf("%w at position %d: column %q is int64; literal must be int64, got %T",
				ErrTypeMismatch, f.Pos, f.Column, f.Value)
		}
	case storage.Float64:
		if _, ok := f.Value.(float64); !ok {
			return fmt.Errorf("%w at position %d: column %q is float64; literal must be float64 (write e.g. 1.0), got %T",
				ErrTypeMismatch, f.Pos, f.Column, f.Value)
		}
	case storage.String:
		if _, ok := f.Value.(string); !ok {
			return fmt.Errorf("%w at position %d: column %q is string; literal must be a single-quoted string, got %T",
				ErrTypeMismatch, f.Pos, f.Column, f.Value)
		}
	default:
		return fmt.Errorf("query: unsupported column type %s", colType)
	}
	return nil
}

func applyFilter(decoded any, colType storage.ColumnType, f *Filter) ([]bool, error) {
	switch colType {
	case storage.Int64:
		vals := decoded.([]int64)
		want := f.Value.(int64)
		return scanInt64(vals, f.Op, want), nil
	case storage.Float64:
		vals := decoded.([]float64)
		want := f.Value.(float64)
		return scanFloat64(vals, f.Op, want), nil
	case storage.String:
		vals := decoded.([]string)
		want := f.Value.(string)
		return scanString(vals, f.Op, want), nil
	default:
		return nil, fmt.Errorf("query: cannot filter on column type %s", colType)
	}
}

func scanInt64(vals []int64, op FilterOp, w int64) []bool {
	out := make([]bool, len(vals))
	for i, v := range vals {
		out[i] = compareInt64(v, op, w)
	}
	return out
}

func scanFloat64(vals []float64, op FilterOp, w float64) []bool {
	out := make([]bool, len(vals))
	for i, v := range vals {
		out[i] = compareFloat64(v, op, w)
	}
	return out
}

func scanString(vals []string, op FilterOp, w string) []bool {
	out := make([]bool, len(vals))
	for i, v := range vals {
		out[i] = compareString(v, op, w)
	}
	return out
}

func compareInt64(v int64, op FilterOp, w int64) bool {
	switch op {
	case OpEq:
		return v == w
	case OpNeq:
		return v != w
	case OpLt:
		return v < w
	case OpGt:
		return v > w
	case OpLte:
		return v <= w
	case OpGte:
		return v >= w
	}
	return false
}

func compareFloat64(v float64, op FilterOp, w float64) bool {
	switch op {
	case OpEq:
		return v == w
	case OpNeq:
		return v != w
	case OpLt:
		return v < w
	case OpGt:
		return v > w
	case OpLte:
		return v <= w
	case OpGte:
		return v >= w
	}
	return false
}

func compareString(v string, op FilterOp, w string) bool {
	switch op {
	case OpEq:
		return v == w
	case OpNeq:
		return v != w
	case OpLt:
		return v < w
	case OpGt:
		return v > w
	case OpLte:
		return v <= w
	case OpGte:
		return v >= w
	}
	return false
}

func execCount(seg *storage.Segment, selection []bool, filtered bool) *Result {
	var n uint64
	if !filtered {
		n = seg.RowCount()
	} else {
		for _, b := range selection {
			if b {
				n++
			}
		}
	}
	return &Result{
		Columns:     []string{"COUNT(*)"},
		Rows:        [][]any{{n}},
		isAggregate: true,
	}
}

func execSum(seg *storage.Segment, col string, schema storage.Schema, selection []bool, filtered bool) (*Result, error) {
	idx, ok := schema.ColumnIndex(col)
	if !ok {
		return nil, fmt.Errorf("%w: %q (in SUM)", ErrUnknownColumn, col)
	}
	colType := schema.Columns[idx].Type
	if colType != storage.Int64 && colType != storage.Float64 {
		return nil, fmt.Errorf("%w: cannot SUM %s column %q", ErrTypeMismatch, colType, col)
	}
	v, err := seg.ReadColumn(col)
	if err != nil {
		return nil, fmt.Errorf("query: read sum column %q: %w", col, err)
	}

	switch colType {
	case storage.Int64:
		vals := v.([]int64)
		sum := int64(0)
		for i, x := range vals {
			if filtered && !selection[i] {
				continue
			}
			newSum := sum + x
			// Overflow detection: if sum and x share a sign but newSum has
			// the opposite sign, we wrapped.
			if (sum > 0 && x > 0 && newSum < 0) || (sum < 0 && x < 0 && newSum >= 0) {
				return nil, fmt.Errorf("%w: column %q exceeds int64 range; consider casting to float64 in source",
					ErrSumOverflow, col)
			}
			sum = newSum
		}
		return &Result{
			Columns:     []string{fmt.Sprintf("SUM(%s)", col)},
			Rows:        [][]any{{sum}},
			isAggregate: true,
		}, nil
	case storage.Float64:
		vals := v.([]float64)
		sum := 0.0
		for i, x := range vals {
			if filtered && !selection[i] {
				continue
			}
			sum += x
		}
		return &Result{
			Columns:     []string{fmt.Sprintf("SUM(%s)", col)},
			Rows:        [][]any{{sum}},
			isAggregate: true,
		}, nil
	}
	// unreachable — guarded above
	return nil, fmt.Errorf("query: unreachable in execSum")
}

func execProject(seg *storage.Segment, cols []string, selection []bool, filtered bool) (*Result, error) {
	// Decode each projected column once.
	decoded := make([]any, len(cols))
	for i, name := range cols {
		v, err := seg.ReadColumn(name)
		if err != nil {
			return nil, fmt.Errorf("query: read column %q: %w", name, err)
		}
		decoded[i] = v
	}
	total := int(seg.RowCount())
	rows := make([][]any, 0, estimateRows(total, selection, filtered))
	for i := range total {
		if filtered && !selection[i] {
			continue
		}
		row := make([]any, len(cols))
		for j, dec := range decoded {
			row[j] = rowValueAt(dec, i)
		}
		rows = append(rows, row)
	}
	return &Result{
		Columns: cols,
		Rows:    rows,
	}, nil
}

// rowValueAt extracts the i'th element from a decoded column slice as
// an interface{}. Mirrors the three supported types.
func rowValueAt(decoded any, i int) any {
	switch s := decoded.(type) {
	case []int64:
		return s[i]
	case []float64:
		return s[i]
	case []string:
		return s[i]
	default:
		return nil
	}
}

func estimateRows(total int, selection []bool, filtered bool) int {
	if !filtered {
		return total
	}
	// Pre-count to size the slice; cheaper than growing it.
	n := 0
	for _, b := range selection {
		if b {
			n++
		}
	}
	return n
}
