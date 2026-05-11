package storage

import "fmt"

// WriteBuffer is a column-major, in-memory append buffer for a single
// Schema. Values are stored in typed per-column slices, not as rows, so
// the in-memory layout already matches the on-disk segment layout that
// later iterations will write.
//
// WriteBuffer is NOT safe for concurrent use. uniqcol assumes a single
// writer; see README, "Kapsam ve Sınırlamalar".
type WriteBuffer struct {
	schema      Schema
	int64Cols   map[int][]int64
	float64Cols map[int][]float64
	stringCols  map[int][]string
	rows        int
}

// NewWriteBuffer constructs an empty WriteBuffer for the given schema.
// The schema is assumed to have been validated by Schema.Validate; this
// constructor does not re-check it.
func NewWriteBuffer(schema Schema) *WriteBuffer {
	b := &WriteBuffer{
		schema:      schema,
		int64Cols:   make(map[int][]int64),
		float64Cols: make(map[int][]float64),
		stringCols:  make(map[int][]string),
	}
	// Pre-register a slice key for every schema column so Reset can
	// iterate without missing freshly-untouched columns.
	for i, c := range schema.Columns {
		switch c.Type {
		case Int64:
			b.int64Cols[i] = nil
		case Float64:
			b.float64Cols[i] = nil
		case String:
			b.stringCols[i] = nil
		}
	}
	return b
}

// Append type-checks each value in row against the schema and appends it
// to the corresponding column slice. If the row length or any value's
// concrete type does not match the schema, Append returns an error
// naming the offending column, and the buffer is left unchanged.
func (b *WriteBuffer) Append(row Row) error {
	if len(row) != len(b.schema.Columns) {
		return fmt.Errorf("row has %d values, schema has %d columns",
			len(row), len(b.schema.Columns))
	}
	// Validate every value before mutating any slice, so a bad row never
	// produces a partial write.
	for i, c := range b.schema.Columns {
		v := row[i]
		if v == nil {
			return fmt.Errorf("column %q (%s): nil value not allowed", c.Name, c.Type)
		}
		switch c.Type {
		case Int64:
			if _, ok := v.(int64); !ok {
				return fmt.Errorf("column %q (%s): expected int64, got %T", c.Name, c.Type, v)
			}
		case Float64:
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("column %q (%s): expected float64, got %T", c.Name, c.Type, v)
			}
		case String:
			if _, ok := v.(string); !ok {
				return fmt.Errorf("column %q (%s): expected string, got %T", c.Name, c.Type, v)
			}
		default:
			return fmt.Errorf("column %q: unknown type %s", c.Name, c.Type)
		}
	}
	for i, c := range b.schema.Columns {
		switch c.Type {
		case Int64:
			b.int64Cols[i] = append(b.int64Cols[i], row[i].(int64))
		case Float64:
			b.float64Cols[i] = append(b.float64Cols[i], row[i].(float64))
		case String:
			b.stringCols[i] = append(b.stringCols[i], row[i].(string))
		}
	}
	b.rows++
	return nil
}

// Len returns the number of rows currently buffered.
func (b *WriteBuffer) Len() int { return b.rows }

// Reset empties the buffer without releasing the underlying slice
// capacity, so subsequent Appends reuse memory. The schema is preserved.
func (b *WriteBuffer) Reset() {
	for i, s := range b.int64Cols {
		b.int64Cols[i] = s[:0]
	}
	for i, s := range b.float64Cols {
		b.float64Cols[i] = s[:0]
	}
	for i, s := range b.stringCols {
		b.stringCols[i] = s[:0]
	}
	b.rows = 0
}
