package bench

import (
	"reflect"
	"testing"
)

func TestGenRows_Count(t *testing.T) {
	rows := GenRows(1000, 0.1, 42)
	if len(rows) != 1000 {
		t.Fatalf("len = %d; want 1000", len(rows))
	}
}

func TestGenRows_ExactDuplicateCount(t *testing.T) {
	rows := GenRows(1000, 0.1, 42)
	distinct := make(map[int64]struct{}, len(rows))
	for _, r := range rows {
		distinct[r[0].(int64)] = struct{}{}
	}
	if len(distinct) != 900 {
		t.Fatalf("distinct event_ids = %d; want 900 (1000 - floor(0.1*1000))", len(distinct))
	}
}

func TestGenRows_DupFractionZero_AllUnique(t *testing.T) {
	rows := GenRows(500, 0, 7)
	distinct := make(map[int64]struct{}, len(rows))
	for _, r := range rows {
		distinct[r[0].(int64)] = struct{}{}
	}
	if len(distinct) != len(rows) {
		t.Errorf("dupFraction=0 produced collisions: %d distinct of %d rows",
			len(distinct), len(rows))
	}
}

func TestGenRows_SameSeed_IdenticalOutput(t *testing.T) {
	a := GenRows(200, 0.2, 9001)
	b := GenRows(200, 0.2, 9001)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same seed produced different output")
	}
}

func TestGenRows_DifferentSeed_DifferentOutput(t *testing.T) {
	a := GenRows(200, 0, 1)
	b := GenRows(200, 0, 2)
	if reflect.DeepEqual(a, b) {
		t.Fatalf("different seeds produced identical output")
	}
}

func TestGenRows_RowsMatchSchema(t *testing.T) {
	rows := GenRows(10, 0, 1)
	schema := BenchSchema()
	for i, r := range rows {
		if len(r) != len(schema.Columns) {
			t.Fatalf("row %d has %d fields; want %d", i, len(r), len(schema.Columns))
		}
		if _, ok := r[0].(int64); !ok {
			t.Errorf("row %d[0] type = %T; want int64", i, r[0])
		}
		if _, ok := r[1].(int64); !ok {
			t.Errorf("row %d[1] type = %T; want int64", i, r[1])
		}
		if _, ok := r[2].(float64); !ok {
			t.Errorf("row %d[2] type = %T; want float64", i, r[2])
		}
		if _, ok := r[3].(string); !ok {
			t.Errorf("row %d[3] type = %T; want string", i, r[3])
		}
	}
}

func TestGenRows_EdgeCases(t *testing.T) {
	if got := GenRows(0, 0.5, 1); got != nil {
		t.Errorf("n=0 should return nil, got %v", got)
	}
	if got := GenRows(-5, 0.5, 1); got != nil {
		t.Errorf("negative n should return nil, got %v", got)
	}
	// Out-of-range dupFraction clamps without crashing.
	if got := GenRows(10, -0.5, 1); len(got) != 10 {
		t.Errorf("dupFraction<0 should clamp to 0; len=%d", len(got))
	}
	if got := GenRows(10, 1.5, 1); len(got) != 10 {
		t.Errorf("dupFraction>1 should clamp to 1; len=%d", len(got))
	}
}
