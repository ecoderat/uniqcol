package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ecoderat/uniqcol/bloom"
)

func tableTestSchema() Schema {
	return Schema{
		PK: "id",
		Columns: []Column{
			{Name: "id", Type: Int64},
			{Name: "amount", Type: Float64},
			{Name: "country", Type: String},
		},
	}
}

func TestCreateTableValidatesInputs(t *testing.T) {
	t.Run("invalid schema", func(t *testing.T) {
		_, err := CreateTable(Schema{PK: "missing"}, TableOptions{
			BloomExpectedItems: 100, BloomTargetFPR: 0.01,
		})
		if err == nil {
			t.Fatalf("expected error for invalid schema")
		}
	})
	t.Run("invalid bloom params", func(t *testing.T) {
		_, err := CreateTable(tableTestSchema(), TableOptions{
			BloomExpectedItems: 0, BloomTargetFPR: 0.01,
		})
		if err == nil {
			t.Fatalf("expected error for zero BloomExpectedItems")
		}
	})
}

func TestInsertUniqueAccepted(t *testing.T) {
	tb, err := CreateTable(tableTestSchema(), TableOptions{
		BloomExpectedItems: 2000, BloomTargetFPR: 0.001,
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const n = 1000
	rejected := 0
	for i := range n {
		r := tb.Insert(Row{int64(i), float64(i), "TR"})
		if !r.Accepted {
			rejected++
			t.Logf("row %d rejected: %s", i, r.Reason)
		}
	}
	// At 0.1% FPR on 1000 uniques, expected false positives are well under
	// 1% in practice. Allow up to 1% slack to keep this from flaking.
	if rejected > n/100 {
		t.Errorf("rejected %d/%d unique inserts; want <=1%%", rejected, n)
	}
	s := tb.Stats()
	if int(s.Accepted)+int(s.Rejected) != n {
		t.Errorf("accepted+rejected = %d; want %d", s.Accepted+s.Rejected, n)
	}
	if s.BufferLen != int(s.Accepted) {
		t.Errorf("BufferLen %d != Accepted %d", s.BufferLen, s.Accepted)
	}
}

func TestInsertDuplicatesRejected(t *testing.T) {
	tb, _ := CreateTable(tableTestSchema(), TableOptions{
		BloomExpectedItems: 2000, BloomTargetFPR: 0.001,
	})
	const n = 500
	for i := range n {
		tb.Insert(Row{int64(i), float64(i), "x"})
	}
	acceptedAfterUnique := tb.Stats().Accepted

	for i := range n {
		r := tb.Insert(Row{int64(i), float64(i), "x"})
		if r.Accepted {
			t.Errorf("duplicate insert for id %d was accepted", i)
		}
		if r.Reason == "" {
			t.Errorf("duplicate rejection had empty Reason")
		}
	}
	s := tb.Stats()
	if s.Accepted != acceptedAfterUnique {
		t.Errorf("accepted count grew during duplicate phase: %d -> %d",
			acceptedAfterUnique, s.Accepted)
	}
	if s.Rejected < n {
		t.Errorf("rejected = %d; want >= %d", s.Rejected, n)
	}
}

func TestInsertTypeErrors(t *testing.T) {
	tb, _ := CreateTable(tableTestSchema(), TableOptions{
		BloomExpectedItems: 100, BloomTargetFPR: 0.01,
	})

	cases := []struct {
		name    string
		row     Row
		wantSub string
	}{
		{"wrong row length", Row{int64(1), 1.0}, "row has 2 values"},
		{"wrong PK type", Row{"not-int", 1.0, "x"}, `PK column "id"`},
		{"wrong non-PK type", Row{int64(1), "nope", "x"}, "type error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tb.Insert(tc.row)
			if r.Accepted {
				t.Fatalf("insert succeeded; expected rejection with %q", tc.wantSub)
			}
			if !strings.Contains(r.Reason, tc.wantSub) {
				t.Fatalf("Reason = %q; want substring %q", r.Reason, tc.wantSub)
			}
		})
	}
}

func TestFlushLoadRoundTrip(t *testing.T) {
	tb, _ := CreateTable(tableTestSchema(), TableOptions{
		BloomExpectedItems: 1000, BloomTargetFPR: 0.001,
	})
	insertedRows := make([]Row, 0, 100)
	for i := range 100 {
		row := Row{int64(i), float64(i), "TR"}
		r := tb.Insert(row)
		if !r.Accepted {
			t.Fatalf("setup: unexpected rejection at i=%d: %s", i, r.Reason)
		}
		insertedRows = append(insertedRows, row)
	}

	path := filepath.Join(t.TempDir(), "tbl.uniq")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tb.Flush(f); err != nil {
		_ = f.Close()
		t.Fatalf("Flush: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	tb2, err := LoadTable(path)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	if tb2.Schema().PK != "id" {
		t.Fatalf("loaded PK = %q; want \"id\"", tb2.Schema().PK)
	}
	for _, row := range insertedRows {
		r := tb2.Insert(row)
		if r.Accepted {
			t.Fatalf("PK %v accepted after load; expected rejection", row[0])
		}
	}
	newAccepted := 0
	for i := 200; i < 250; i++ {
		r := tb2.Insert(Row{int64(i), float64(i), "US"})
		if r.Accepted {
			newAccepted++
		}
	}
	// At 0.1% FPR with ~150 keys in the filter, expected FPs on 50 unique
	// queries is essentially zero. Allow up to 5 to be safe.
	if newAccepted < 45 {
		t.Errorf("new unique inserts accepted = %d; want >=45", newAccepted)
	}
}

func TestLoadTableRefusesV1(t *testing.T) {
	buf, _, _, _, _ := buildFixture(t, 5)
	path := filepath.Join(t.TempDir(), "v1.uniq")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	writeV1Segment(t, f, fixtureSchema(), buf)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := LoadTable(path); !errors.Is(err, ErrIncompatibleSegment) {
		t.Fatalf("err = %v; want ErrIncompatibleSegment", err)
	}
}

func TestLoadTableRefusesV2WithoutPK(t *testing.T) {
	buf, _, _, _, _ := buildFixture(t, 5)
	bf, err := bloom.New(100, 0.01)
	if err != nil {
		t.Fatalf("bloom.New: %v", err)
	}
	path := filepath.Join(t.TempDir(), "nopk.uniq")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := WriteSegment(f, fixtureSchema(), buf, WriteSegmentOpts{Bloom: bf}); err != nil {
		_ = f.Close()
		t.Fatalf("WriteSegment: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := LoadTable(path); !errors.Is(err, ErrIncompatibleSegment) {
		t.Fatalf("err = %v; want ErrIncompatibleSegment", err)
	}
}

func TestLoadTableRefusesV2WithoutBloom(t *testing.T) {
	buf, _, _, _, _ := buildFixture(t, 5)
	path := filepath.Join(t.TempDir(), "nobf.uniq")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := WriteSegment(f, fixtureSchema(), buf, WriteSegmentOpts{PKName: "id"}); err != nil {
		_ = f.Close()
		t.Fatalf("WriteSegment: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := LoadTable(path); !errors.Is(err, ErrIncompatibleSegment) {
		t.Fatalf("err = %v; want ErrIncompatibleSegment", err)
	}
}

func TestLoadTableOpenError(t *testing.T) {
	if _, err := LoadTable(filepath.Join(t.TempDir(), "missing.uniq")); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestPKBytesAllTypes(t *testing.T) {
	cases := []struct {
		name string
		v    any
		typ  ColumnType
		want int
	}{
		{"int64", int64(42), Int64, 8},
		{"float64", 3.14, Float64, 8},
		{"string short", "abc", String, 3},
		{"string utf8", "世界", String, 6},
		{"string empty", "", String, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := pkBytes(tc.v, tc.typ)
			if err != nil {
				t.Fatalf("pkBytes: %v", err)
			}
			if len(b) != tc.want {
				t.Errorf("len = %d; want %d", len(b), tc.want)
			}
		})
	}
	a, _ := pkBytes(int64(1), Int64)
	b, _ := pkBytes(1.0, Float64)
	if bytes.Equal(a, b) {
		t.Errorf("int64(1) and float64(1.0) produced identical bytes")
	}

	if _, err := pkBytes("nope", Int64); err == nil {
		t.Errorf("expected type-mismatch error for string into Int64")
	}
	if _, err := pkBytes(int64(1), Float64); err == nil {
		t.Errorf("expected type-mismatch error for int64 into Float64")
	}
	if _, err := pkBytes(1.0, String); err == nil {
		t.Errorf("expected type-mismatch error for float64 into String")
	}
	if _, err := pkBytes("x", ColumnType(99)); err == nil {
		t.Errorf("expected error for unsupported PK type")
	}
}

func TestTableBloomAndStatsAccessors(t *testing.T) {
	tb, _ := CreateTable(tableTestSchema(), TableOptions{
		BloomExpectedItems: 100, BloomTargetFPR: 0.01,
	})
	if tb.Bloom() == nil {
		t.Fatalf("Bloom() returned nil")
	}
	if tb.Schema().PK != "id" {
		t.Fatalf("Schema().PK = %q", tb.Schema().PK)
	}
	tb.Insert(Row{int64(1), 1.0, "TR"})
	tb.Insert(Row{int64(1), 1.0, "TR"}) // duplicate
	s := tb.Stats()
	if s.Accepted != 1 || s.Rejected != 1 || s.BufferLen != 1 {
		t.Errorf("Stats() = %+v; want {1,1,1}", s)
	}
}
