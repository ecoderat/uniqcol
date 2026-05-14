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

	// 2. Evaluate the WHERE expression tree (if any) into a selection
	// vector. The Segment caches decoded columns internally, so a column
	// referenced in multiple comparisons is read exactly once.
	var (
		selection []bool
		filtered  bool
	)
	if q.Where != nil {
		var err error
		selection, err = evaluate(seg, schema, q.Where)
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
	case ProjGrouped:
		return execGrouped(seg, schema, q, selection, filtered)
	default:
		return nil, fmt.Errorf("query: unknown projection kind %d", q.Projection)
	}
}

// evaluate recursively walks the WHERE expression tree, returning a
// selection vector of length seg.RowCount(). Leaf nodes read and scan
// their column; And/Or nodes combine child vectors with bitwise AND/OR.
//
// We do NOT short-circuit (e.g. when an OR child evaluates to all-true).
// The simpler recurse-and-combine approach is correct and adequate for
// this prototype; short-circuit is a future optimization.
func evaluate(seg *storage.Segment, schema storage.Schema, e *FilterExpr) ([]bool, error) {
	switch {
	case e.Comparison != nil:
		return evalComparison(seg, schema, e.Comparison)
	case e.And != nil:
		acc, err := evaluate(seg, schema, e.And[0])
		if err != nil {
			return nil, err
		}
		for _, child := range e.And[1:] {
			next, err := evaluate(seg, schema, child)
			if err != nil {
				return nil, err
			}
			for i := range acc {
				acc[i] = acc[i] && next[i]
			}
		}
		return acc, nil
	case e.Or != nil:
		acc, err := evaluate(seg, schema, e.Or[0])
		if err != nil {
			return nil, err
		}
		for _, child := range e.Or[1:] {
			next, err := evaluate(seg, schema, child)
			if err != nil {
				return nil, err
			}
			for i := range acc {
				acc[i] = acc[i] || next[i]
			}
		}
		return acc, nil
	default:
		return nil, fmt.Errorf("query: malformed FilterExpr (all branches nil)")
	}
}

func evalComparison(seg *storage.Segment, schema storage.Schema, c *Comparison) ([]bool, error) {
	idx, ok := schema.ColumnIndex(c.Column)
	if !ok {
		return nil, fmt.Errorf("%w: %q (in WHERE)", ErrUnknownColumn, c.Column)
	}
	colType := schema.Columns[idx].Type
	if err := checkLiteralType(c, colType); err != nil {
		return nil, err
	}
	v, err := seg.ReadColumn(c.Column)
	if err != nil {
		return nil, fmt.Errorf("query: read filter column %q: %w", c.Column, err)
	}
	return applyComparison(v, colType, c)
}

// checkLiteralType ensures the parser's literal type matches the column's
// declared type. No silent coercion — see Comparison doc.
func checkLiteralType(c *Comparison, colType storage.ColumnType) error {
	switch colType {
	case storage.Int64:
		if _, ok := c.Value.(int64); !ok {
			return fmt.Errorf("%w at position %d: column %q is int64; literal must be int64, got %T",
				ErrTypeMismatch, c.Pos, c.Column, c.Value)
		}
	case storage.Float64:
		if _, ok := c.Value.(float64); !ok {
			return fmt.Errorf("%w at position %d: column %q is float64; literal must be float64 (write e.g. 1.0), got %T",
				ErrTypeMismatch, c.Pos, c.Column, c.Value)
		}
	case storage.String:
		if _, ok := c.Value.(string); !ok {
			return fmt.Errorf("%w at position %d: column %q is string; literal must be a single-quoted string, got %T",
				ErrTypeMismatch, c.Pos, c.Column, c.Value)
		}
	default:
		return fmt.Errorf("query: unsupported column type %s", colType)
	}
	return nil
}

func applyComparison(decoded any, colType storage.ColumnType, c *Comparison) ([]bool, error) {
	switch colType {
	case storage.Int64:
		vals := decoded.([]int64)
		want := c.Value.(int64)
		return scanInt64(vals, c.Op, want), nil
	case storage.Float64:
		vals := decoded.([]float64)
		want := c.Value.(float64)
		return scanFloat64(vals, c.Op, want), nil
	case storage.String:
		vals := decoded.([]string)
		want := c.Value.(string)
		return scanString(vals, c.Op, want), nil
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
			newSum, ok := addInt64Checked(sum, x)
			if !ok {
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

// addInt64Checked returns sum+x and a "no overflow" flag. Overflow
// occurs when sum and x share a sign but the result has the opposite
// sign — the standard two's-complement test.
func addInt64Checked(sum, x int64) (int64, bool) {
	newSum := sum + x
	if (sum > 0 && x > 0 && newSum < 0) || (sum < 0 && x < 0 && newSum >= 0) {
		return 0, false
	}
	return newSum, true
}

// groupAccumulator holds the running aggregate state for one group key.
// The executor knows globally which aggregate is active for a given
// query, so the struct doesn't carry its own discriminator — only the
// relevant field is updated, the others stay at their zero value.
//
// TODO(future): for very large group counts, the in-memory map could
// spill to disk. Out of scope for this prototype — note kept here so
// the location is obvious when revisiting.
type groupAccumulator struct {
	count  uint64  // AggCountStar
	sumI64 int64   // AggSumColumn over an Int64 column
	sumF64 float64 // AggSumColumn over a Float64 column
}

// execGrouped implements ProjGrouped: scan, partition by group key,
// emit one row per discovered group in insertion order.
//
// NaN as a group key is technically pathological because NaN != NaN in
// Go map keys — distinct NaN bit patterns become distinct groups. Not
// special-cased; documented here.
func execGrouped(seg *storage.Segment, schema storage.Schema, q *Query, selection []bool, filtered bool) (*Result, error) {
	gIdx, ok := schema.ColumnIndex(q.GroupBy)
	if !ok {
		return nil, fmt.Errorf("%w: %q (in GROUP BY)", ErrUnknownColumn, q.GroupBy)
	}
	gType := schema.Columns[gIdx].Type
	gCol, err := seg.ReadColumn(q.GroupBy)
	if err != nil {
		return nil, fmt.Errorf("query: read group column %q: %w", q.GroupBy, err)
	}

	var (
		sumIsInt   bool
		sumIsFloat bool
		sumI64s    []int64
		sumF64s    []float64
	)
	if q.GroupAgg == AggSumColumn {
		sIdx, ok := schema.ColumnIndex(q.SumColumn)
		if !ok {
			return nil, fmt.Errorf("%w: %q (in SUM)", ErrUnknownColumn, q.SumColumn)
		}
		sType := schema.Columns[sIdx].Type
		if sType != storage.Int64 && sType != storage.Float64 {
			return nil, fmt.Errorf("%w: cannot SUM %s column %q", ErrTypeMismatch, sType, q.SumColumn)
		}
		sv, err := seg.ReadColumn(q.SumColumn)
		if err != nil {
			return nil, fmt.Errorf("query: read sum column %q: %w", q.SumColumn, err)
		}
		switch sType {
		case storage.Int64:
			sumIsInt = true
			sumI64s = sv.([]int64)
		case storage.Float64:
			sumIsFloat = true
			sumF64s = sv.([]float64)
		}
	}

	keys := make([]any, 0, 16)
	groups := make(map[any]*groupAccumulator)
	total := int(seg.RowCount())
	for i := range total {
		if filtered && !selection[i] {
			continue
		}
		key := rowValueAt(gCol, i)
		// Normalize the key under map semantics. int64/float64/string
		// are all comparable; nothing further is needed. NaN floats
		// behave as distinct keys — see doc.
		_ = gType
		acc, ok := groups[key]
		if !ok {
			acc = &groupAccumulator{}
			groups[key] = acc
			keys = append(keys, key)
		}
		switch q.GroupAgg {
		case AggCountStar:
			acc.count++
		case AggSumColumn:
			switch {
			case sumIsInt:
				newSum, ok := addInt64Checked(acc.sumI64, sumI64s[i])
				if !ok {
					return nil, fmt.Errorf("%w: column %q exceeds int64 range in group %v",
						ErrSumOverflow, q.SumColumn, key)
				}
				acc.sumI64 = newSum
			case sumIsFloat:
				acc.sumF64 += sumF64s[i]
			}
		}
	}

	// Emit results in discovery order.
	rows := make([][]any, 0, len(keys))
	for _, k := range keys {
		acc := groups[k]
		var aggVal any
		switch q.GroupAgg {
		case AggCountStar:
			aggVal = acc.count
		case AggSumColumn:
			switch {
			case sumIsInt:
				aggVal = acc.sumI64
			case sumIsFloat:
				aggVal = acc.sumF64
			}
		}
		rows = append(rows, []any{k, aggVal})
	}

	aggHeader := "COUNT(*)"
	if q.GroupAgg == AggSumColumn {
		aggHeader = fmt.Sprintf("SUM(%s)", q.SumColumn)
	}
	return &Result{
		Columns: []string{q.GroupBy, aggHeader},
		Rows:    rows,
		// Grouped results are NOT marked aggregate — they're a row set
		// (one row per group), which is what IsAggregate distinguishes.
	}, nil
}
