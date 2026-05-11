package storage

import (
	"strings"
	"testing"
)

func newTestSchema() Schema {
	return Schema{
		PK: "id",
		Columns: []Column{
			{Name: "id", Type: Int64},
			{Name: "amount", Type: Float64},
			{Name: "country", Type: String},
		},
	}
}

func TestWriteBufferAppendAllTypes(t *testing.T) {
	b := NewWriteBuffer(newTestSchema())
	rows := []Row{
		{int64(1), 10.5, "TR"},
		{int64(2), 20.0, "US"},
		{int64(3), -1.25, ""},
	}
	for i, r := range rows {
		if err := b.Append(r); err != nil {
			t.Fatalf("Append row %d: %v", i, err)
		}
	}
	if b.Len() != len(rows) {
		t.Fatalf("Len() = %d; want %d", b.Len(), len(rows))
	}
	if got := b.int64Cols[0]; len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("int64Cols[0] = %v; want [1 2 3]", got)
	}
	if got := b.float64Cols[1]; len(got) != 3 || got[1] != 20.0 {
		t.Fatalf("float64Cols[1] = %v; want second value 20.0", got)
	}
	if got := b.stringCols[2]; len(got) != 3 || got[0] != "TR" || got[2] != "" {
		t.Fatalf("stringCols[2] = %v; want [TR US \"\"]", got)
	}
}

func TestWriteBufferAppendErrors(t *testing.T) {
	tests := []struct {
		name    string
		row     Row
		wantSub string
	}{
		{
			name:    "wrong column count short",
			row:     Row{int64(1), 1.0},
			wantSub: "2 values",
		},
		{
			name:    "wrong column count long",
			row:     Row{int64(1), 1.0, "x", "extra"},
			wantSub: "4 values",
		},
		{
			name:    "wrong type for int64",
			row:     Row{"not-an-int", 1.0, "x"},
			wantSub: `expected int64, got string`,
		},
		{
			name:    "wrong type for float64",
			row:     Row{int64(1), "nope", "x"},
			wantSub: `expected float64, got string`,
		},
		{
			name:    "wrong type for string",
			row:     Row{int64(1), 1.0, 42},
			wantSub: `expected string, got int`,
		},
		{
			name:    "int instead of int64",
			row:     Row{1, 1.0, "x"},
			wantSub: `expected int64, got int`,
		},
		{
			name:    "float32 instead of float64",
			row:     Row{int64(1), float32(1.0), "x"},
			wantSub: `expected float64, got float32`,
		},
		{
			name:    "nil value rejected",
			row:     Row{int64(1), nil, "x"},
			wantSub: "nil value not allowed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewWriteBuffer(newTestSchema())
			err := b.Append(tc.row)
			if err == nil {
				t.Fatalf("Append() = nil; want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Append() error = %q; want substring %q", err.Error(), tc.wantSub)
			}
			if b.Len() != 0 {
				t.Fatalf("Len() = %d; want 0 (buffer must not change on error)", b.Len())
			}
		})
	}
}

func TestWriteBufferUnknownColumnType(t *testing.T) {
	// Construct a deliberately broken schema (not Validated). NewWriteBuffer
	// skips unknown types; Append should surface them with a clear error.
	s := Schema{
		PK:      "id",
		Columns: []Column{{Name: "id", Type: ColumnType(99)}},
	}
	b := NewWriteBuffer(s)
	err := b.Append(Row{int64(1)})
	if err == nil {
		t.Fatalf("Append() = nil; want error for unknown column type")
	}
	if !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("Append() error = %q; want substring %q", err.Error(), "unknown type")
	}
}

func TestWriteBufferLen(t *testing.T) {
	b := NewWriteBuffer(newTestSchema())
	if b.Len() != 0 {
		t.Fatalf("Len() initial = %d; want 0", b.Len())
	}
	for i := range 5 {
		if err := b.Append(Row{int64(i), float64(i), "x"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if b.Len() != 5 {
		t.Fatalf("Len() = %d; want 5", b.Len())
	}
}

func TestWriteBufferResetPreservesCapacity(t *testing.T) {
	b := NewWriteBuffer(newTestSchema())
	const n = 32
	for i := range n {
		if err := b.Append(Row{int64(i), float64(i), "x"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	intCap := cap(b.int64Cols[0])
	floatCap := cap(b.float64Cols[1])
	strCap := cap(b.stringCols[2])
	if intCap < n || floatCap < n || strCap < n {
		t.Fatalf("expected capacities >= %d; got int=%d float=%d str=%d",
			n, intCap, floatCap, strCap)
	}

	b.Reset()

	if b.Len() != 0 {
		t.Fatalf("Len() after Reset() = %d; want 0", b.Len())
	}
	if len(b.int64Cols[0]) != 0 || len(b.float64Cols[1]) != 0 || len(b.stringCols[2]) != 0 {
		t.Fatalf("Reset() did not zero slice lengths: int=%d float=%d str=%d",
			len(b.int64Cols[0]), len(b.float64Cols[1]), len(b.stringCols[2]))
	}
	if cap(b.int64Cols[0]) != intCap {
		t.Fatalf("Reset() shrank int64 capacity: %d -> %d", intCap, cap(b.int64Cols[0]))
	}
	if cap(b.float64Cols[1]) != floatCap {
		t.Fatalf("Reset() shrank float64 capacity: %d -> %d", floatCap, cap(b.float64Cols[1]))
	}
	if cap(b.stringCols[2]) != strCap {
		t.Fatalf("Reset() shrank string capacity: %d -> %d", strCap, cap(b.stringCols[2]))
	}

	// Subsequent Append should reuse the existing backing arrays.
	if err := b.Append(Row{int64(99), 99.0, "y"}); err != nil {
		t.Fatalf("Append after Reset: %v", err)
	}
	if b.Len() != 1 {
		t.Fatalf("Len() after Append post-Reset = %d; want 1", b.Len())
	}
	if cap(b.int64Cols[0]) != intCap {
		t.Fatalf("capacity changed after re-append: %d -> %d",
			intCap, cap(b.int64Cols[0]))
	}
}
