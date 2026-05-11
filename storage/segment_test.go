package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ecoderat/uniqcol/bloom"
)

// fixtureSchema returns the schema used by the end-to-end segment tests.
// It mixes all three supported types and is the contract every assertion
// below depends on.
func fixtureSchema() Schema {
	return Schema{
		PK: "id",
		Columns: []Column{
			{Name: "id", Type: Int64},       // unique
			{Name: "amount", Type: Float64}, // mostly unique
			{Name: "country", Type: String}, // low-cardinality (RLE shines)
			{Name: "tag", Type: String},     // unique strings
		},
	}
}

// buildFixture appends n rows that exercise both low-cardinality and
// unique-value paths, and returns the expected typed slices alongside
// the loaded buffer.
func buildFixture(t *testing.T, n int) (*WriteBuffer, []int64, []float64, []string, []string) {
	t.Helper()
	buf := NewWriteBuffer(fixtureSchema())
	countries := []string{"TR", "US", "DE", "GB", "FR"}
	ids := make([]int64, n)
	amounts := make([]float64, n)
	wantCountries := make([]string, n)
	tags := make([]string, n)
	for i := range n {
		id := int64(i)
		amt := float64(i) * 1.5
		country := countries[i%len(countries)]
		tag := fmt.Sprintf("t%d", i)
		ids[i] = id
		amounts[i] = amt
		wantCountries[i] = country
		tags[i] = tag
		if err := buf.Append(Row{id, amt, country, tag}); err != nil {
			t.Fatalf("Append row %d: %v", i, err)
		}
	}
	return buf, ids, amounts, wantCountries, tags
}

func TestSegmentRoundTripInMemory(t *testing.T) {
	buf, ids, amounts, countries, tags := buildFixture(t, 100)
	var bb bytes.Buffer
	if err := WriteSegment(&bb, fixtureSchema(), buf, WriteSegmentOpts{}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}

	seg, err := parseSegment(bb.Bytes())
	if err != nil {
		t.Fatalf("parseSegment: %v", err)
	}
	defer seg.Close()

	if got := seg.RowCount(); got != 100 {
		t.Fatalf("RowCount() = %d; want 100", got)
	}
	if got := seg.Schema(); len(got.Columns) != 4 {
		t.Fatalf("Schema().Columns = %d; want 4", len(got.Columns))
	}

	col, err := seg.ReadColumn("id")
	if err != nil {
		t.Fatalf("read id: %v", err)
	}
	if !reflect.DeepEqual(col.([]int64), ids) {
		t.Fatalf("id column mismatch")
	}
	col, err = seg.ReadColumn("amount")
	if err != nil {
		t.Fatalf("read amount: %v", err)
	}
	if !reflect.DeepEqual(col.([]float64), amounts) {
		t.Fatalf("amount column mismatch")
	}
	col, err = seg.ReadColumn("country")
	if err != nil {
		t.Fatalf("read country: %v", err)
	}
	if !reflect.DeepEqual(col.([]string), countries) {
		t.Fatalf("country column mismatch")
	}
	col, err = seg.ReadColumn("tag")
	if err != nil {
		t.Fatalf("read tag: %v", err)
	}
	if !reflect.DeepEqual(col.([]string), tags) {
		t.Fatalf("tag column mismatch")
	}
}

func TestSegmentRoundTripFile(t *testing.T) {
	buf, ids, _, countries, _ := buildFixture(t, 50)

	p := filepath.Join(t.TempDir(), "events.uniq")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := WriteSegment(f, fixtureSchema(), buf, WriteSegmentOpts{}); err != nil {
		_ = f.Close()
		t.Fatalf("WriteSegment: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	seg, err := OpenSegment(p)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()

	if seg.RowCount() != 50 {
		t.Fatalf("RowCount() = %d; want 50", seg.RowCount())
	}
	gotIDs, err := seg.ReadColumn("id")
	if err != nil {
		t.Fatalf("read id: %v", err)
	}
	if !reflect.DeepEqual(gotIDs.([]int64), ids) {
		t.Fatalf("id mismatch")
	}
	gotCountries, err := seg.ReadColumn("country")
	if err != nil {
		t.Fatalf("read country: %v", err)
	}
	if !reflect.DeepEqual(gotCountries.([]string), countries) {
		t.Fatalf("country mismatch")
	}
}

// TestSegmentColumnPruning verifies that asking for one column does not
// decode the others. The cache map is the source of truth for "decoded
// so far", so checking it directly is the cleanest signal — no build
// tags or counters needed (we're in the same package).
func TestSegmentColumnPruning(t *testing.T) {
	buf, _, _, _, _ := buildFixture(t, 30)
	var bb bytes.Buffer
	if err := WriteSegment(&bb, fixtureSchema(), buf, WriteSegmentOpts{}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	seg, err := parseSegment(bb.Bytes())
	if err != nil {
		t.Fatalf("parseSegment: %v", err)
	}

	if len(seg.decoded) != 0 {
		t.Fatalf("decoded cache should start empty; got %d entries: %v",
			len(seg.decoded), keys(seg.decoded))
	}

	if _, err := seg.ReadColumn("amount"); err != nil {
		t.Fatalf("ReadColumn amount: %v", err)
	}

	if len(seg.decoded) != 1 {
		t.Fatalf("after one read, decoded cache has %d entries (%v); want 1 (only \"amount\")",
			len(seg.decoded), keys(seg.decoded))
	}
	if _, ok := seg.decoded["amount"]; !ok {
		t.Fatalf("decoded cache missing \"amount\"; has %v", keys(seg.decoded))
	}
	for _, other := range []string{"id", "country", "tag"} {
		if _, ok := seg.decoded[other]; ok {
			t.Fatalf("column %q decoded even though only \"amount\" was requested", other)
		}
	}

	// Repeated read should hit cache, not re-decode (cache size stays 1).
	if _, err := seg.ReadColumn("amount"); err != nil {
		t.Fatalf("re-read amount: %v", err)
	}
	if len(seg.decoded) != 1 {
		t.Fatalf("cache grew on cache-hit re-read: %v", keys(seg.decoded))
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSegmentEmptyBuffer(t *testing.T) {
	schema := Schema{PK: "id", Columns: []Column{{Name: "id", Type: Int64}}}
	buf := NewWriteBuffer(schema)
	var bb bytes.Buffer
	if err := WriteSegment(&bb, schema, buf, WriteSegmentOpts{}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	seg, err := parseSegment(bb.Bytes())
	if err != nil {
		t.Fatalf("parseSegment: %v", err)
	}
	if seg.RowCount() != 0 {
		t.Fatalf("rowCount = %d; want 0", seg.RowCount())
	}
	v, err := seg.ReadColumn("id")
	if err != nil {
		t.Fatalf("ReadColumn: %v", err)
	}
	if len(v.([]int64)) != 0 {
		t.Fatalf("expected empty slice, got %v", v)
	}
}

func TestReadSegmentHeader(t *testing.T) {
	buf, _, _, _, _ := buildFixture(t, 12)
	var bb bytes.Buffer
	if err := WriteSegment(&bb, fixtureSchema(), buf, WriteSegmentOpts{}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	schema, rowCount, err := ReadSegmentHeader(bytes.NewReader(bb.Bytes()))
	if err != nil {
		t.Fatalf("ReadSegmentHeader: %v", err)
	}
	if rowCount != 12 {
		t.Fatalf("rowCount = %d; want 12", rowCount)
	}
	want := fixtureSchema().Columns
	if len(schema.Columns) != len(want) {
		t.Fatalf("columns = %d; want %d", len(schema.Columns), len(want))
	}
	for i := range want {
		if schema.Columns[i].Name != want[i].Name || schema.Columns[i].Type != want[i].Type {
			t.Fatalf("column[%d] = %+v; want %+v", i, schema.Columns[i], want[i])
		}
	}
	if schema.PK != "" {
		t.Fatalf("PK should be empty for a segment-loaded schema, got %q", schema.PK)
	}
}

// validSegmentBytes returns a freshly written segment image for use in
// corruption tests.
func validSegmentBytes(t *testing.T) []byte {
	t.Helper()
	buf, _, _, _, _ := buildFixture(t, 5)
	var bb bytes.Buffer
	if err := WriteSegment(&bb, fixtureSchema(), buf, WriteSegmentOpts{}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	return bb.Bytes()
}

func TestSegmentBadMagic(t *testing.T) {
	data := validSegmentBytes(t)
	data[0] = 'X'
	if _, err := parseSegment(data); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("parseSegment err = %v; want ErrBadMagic", err)
	}
	if _, _, err := ReadSegmentHeader(bytes.NewReader(data)); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("ReadSegmentHeader err = %v; want ErrBadMagic", err)
	}
}

func TestSegmentUnsupportedVersion(t *testing.T) {
	data := validSegmentBytes(t)
	binary.LittleEndian.PutUint16(data[4:6], 99)
	if _, err := parseSegment(data); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("parseSegment err = %v; want ErrUnsupportedVersion", err)
	}
	if _, _, err := ReadSegmentHeader(bytes.NewReader(data)); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("ReadSegmentHeader err = %v; want ErrUnsupportedVersion", err)
	}
}

func TestSegmentTruncated(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"truncated header", 8},
		{"header only", segmentHeaderLen},
		{"mid first column", segmentHeaderLen + 3},
	}
	data := validSegmentBytes(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSegment(data[:tc.size]); !errors.Is(err, ErrTruncated) {
				t.Fatalf("parseSegment(%d bytes) err = %v; want ErrTruncated", tc.size, err)
			}
		})
	}
}

func TestSegmentReadSegmentHeaderTruncated(t *testing.T) {
	data := validSegmentBytes(t)
	if _, _, err := ReadSegmentHeader(bytes.NewReader(data[:8])); !errors.Is(err, ErrTruncated) {
		t.Fatalf("ReadSegmentHeader err = %v; want ErrTruncated", err)
	}
}

func TestSegmentUnknownColumn(t *testing.T) {
	data := validSegmentBytes(t)
	seg, err := parseSegment(data)
	if err != nil {
		t.Fatalf("parseSegment: %v", err)
	}
	if _, err := seg.ReadColumn("nope"); !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("ReadColumn err = %v; want ErrUnknownColumn", err)
	}
}

func TestSegmentBadEncoding(t *testing.T) {
	// Write a 1-column segment then corrupt the encoding byte.
	schema := Schema{PK: "id", Columns: []Column{{Name: "id", Type: Int64}}}
	b := NewWriteBuffer(schema)
	if err := b.Append(Row{int64(1)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var bb bytes.Buffer
	if err := WriteSegment(&bb, schema, b, WriteSegmentOpts{}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	data := bb.Bytes()
	// V2 layout for 1 column named "id" with empty opts:
	//   header(16) | flagsLen=1(1) | flags=0(1) | nameLen=2(1) | "id"(2) | wireType(1) | encoding(1) | ...
	// So wireType is at offset 21 and encoding at 22.
	data[22] = 0xEE
	if _, err := parseSegment(data); err == nil {
		t.Fatalf("expected error for unknown encoding")
	}
}

func TestColumnTypeWireMapping(t *testing.T) {
	for _, ct := range []ColumnType{Int64, Float64, String} {
		w, err := columnTypeToWire(ct)
		if err != nil {
			t.Fatalf("columnTypeToWire(%v): %v", ct, err)
		}
		back, err := wireToColumnType(w)
		if err != nil {
			t.Fatalf("wireToColumnType(%d): %v", w, err)
		}
		if back != ct {
			t.Fatalf("round trip: %v -> %d -> %v", ct, w, back)
		}
	}
	if _, err := columnTypeToWire(ColumnType(99)); err == nil {
		t.Fatalf("expected error for unknown ColumnType")
	}
	if _, err := wireToColumnType(99); err == nil {
		t.Fatalf("expected error for unknown wire type")
	}
}

func TestOpenSegmentMissingFile(t *testing.T) {
	if _, err := OpenSegment(filepath.Join(t.TempDir(), "does-not-exist.uniq")); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestSegmentBadWireColumnType(t *testing.T) {
	schema := Schema{PK: "id", Columns: []Column{{Name: "id", Type: Int64}}}
	b := NewWriteBuffer(schema)
	if err := b.Append(Row{int64(1)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var bb bytes.Buffer
	if err := WriteSegment(&bb, schema, b, WriteSegmentOpts{}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	data := bb.Bytes()
	// wireType byte is at offset 21 in v2 (see TestSegmentBadEncoding).
	data[21] = 0xEE
	if _, err := parseSegment(data); err == nil {
		t.Fatalf("expected error for unknown wire column type")
	}
	// Also exercise ReadSegmentHeader's branch.
	if _, _, err := ReadSegmentHeader(bytes.NewReader(data)); err == nil {
		t.Fatalf("expected ReadSegmentHeader error for unknown wire column type")
	}
}

// writeV1Segment emits the Iteration 1 (legacy) wire format directly:
// 16-byte header with version=1, then column blocks. No flags block, no
// Bloom trailer. Used to verify v1 read-compatibility.
func writeV1Segment(t *testing.T, w io.Writer, schema Schema, buf *WriteBuffer) {
	t.Helper()
	var hdr [segmentHeaderLen]byte
	copy(hdr[0:4], segmentMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], segmentVersionV1)
	binary.LittleEndian.PutUint16(hdr[6:8], uint16(len(schema.Columns)))
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(buf.Len()))
	if _, err := w.Write(hdr[:]); err != nil {
		t.Fatalf("v1 hdr: %v", err)
	}
	var ub [binary.MaxVarintLen64]byte
	for i, c := range schema.Columns {
		var payload []byte
		switch c.Type {
		case Int64:
			payload = encodeRLEInt64(buf.int64Cols[i])
		case Float64:
			payload = encodeRLEFloat64(buf.float64Cols[i])
		case String:
			payload = encodeRLEString(buf.stringCols[i])
		}
		wire, _ := columnTypeToWire(c.Type)
		n := binary.PutUvarint(ub[:], uint64(len(c.Name)))
		_, _ = w.Write(ub[:n])
		_, _ = io.WriteString(w, c.Name)
		_, _ = w.Write([]byte{wire, byte(EncodingRLE)})
		n = binary.PutUvarint(ub[:], uint64(len(payload)))
		_, _ = w.Write(ub[:n])
		_, _ = w.Write(payload)
	}
}

func TestSegmentV1ReadCompatibility(t *testing.T) {
	buf, ids, _, countries, _ := buildFixture(t, 20)
	var bb bytes.Buffer
	writeV1Segment(t, &bb, fixtureSchema(), buf)

	seg, err := parseSegment(bb.Bytes())
	if err != nil {
		t.Fatalf("parseSegment v1: %v", err)
	}
	defer seg.Close()

	if seg.Version() != segmentVersionV1 {
		t.Fatalf("version = %d; want %d", seg.Version(), segmentVersionV1)
	}
	if seg.PKName() != "" {
		t.Fatalf("v1 PKName = %q; want \"\"", seg.PKName())
	}
	if seg.Bloom() != nil {
		t.Fatalf("v1 should have no Bloom filter")
	}
	if seg.Schema().PK != "" {
		t.Fatalf("v1 Schema.PK = %q; want \"\"", seg.Schema().PK)
	}

	gotIDs, err := seg.ReadColumn("id")
	if err != nil {
		t.Fatalf("read id: %v", err)
	}
	if !reflect.DeepEqual(gotIDs.([]int64), ids) {
		t.Fatalf("id mismatch")
	}
	gotCountries, err := seg.ReadColumn("country")
	if err != nil {
		t.Fatalf("read country: %v", err)
	}
	if !reflect.DeepEqual(gotCountries.([]string), countries) {
		t.Fatalf("country mismatch")
	}

	// ReadSegmentHeader on v1 should also succeed.
	schema, rowCount, err := ReadSegmentHeader(bytes.NewReader(bb.Bytes()))
	if err != nil {
		t.Fatalf("ReadSegmentHeader v1: %v", err)
	}
	if rowCount != 20 {
		t.Fatalf("rowCount = %d; want 20", rowCount)
	}
	if schema.PK != "" {
		t.Fatalf("v1 ReadSegmentHeader returned PK %q; want \"\"", schema.PK)
	}
}

func TestSegmentV2WithPKAndBloom(t *testing.T) {
	buf, ids, _, _, _ := buildFixture(t, 25)
	bf, err := bloom.New(1000, 0.01)
	if err != nil {
		t.Fatalf("bloom.New: %v", err)
	}
	for _, id := range ids {
		var keyBuf [8]byte
		binary.LittleEndian.PutUint64(keyBuf[:], uint64(id))
		bf.Add(keyBuf[:])
	}

	var bb bytes.Buffer
	if err := WriteSegment(&bb, fixtureSchema(), buf, WriteSegmentOpts{
		PKName: "id",
		Bloom:  bf,
	}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}

	seg, err := parseSegment(bb.Bytes())
	if err != nil {
		t.Fatalf("parseSegment: %v", err)
	}
	defer seg.Close()

	if seg.Version() != segmentVersionV2 {
		t.Fatalf("version = %d; want %d", seg.Version(), segmentVersionV2)
	}
	if seg.PKName() != "id" {
		t.Fatalf("PKName = %q; want \"id\"", seg.PKName())
	}
	if seg.Schema().PK != "id" {
		t.Fatalf("Schema().PK = %q; want \"id\"", seg.Schema().PK)
	}
	if seg.Bloom() == nil {
		t.Fatalf("expected non-nil Bloom filter")
	}

	// Loaded BF must still flag inserted PKs.
	for _, id := range ids {
		var keyBuf [8]byte
		binary.LittleEndian.PutUint64(keyBuf[:], uint64(id))
		if !seg.Bloom().Contains(keyBuf[:]) {
			t.Fatalf("loaded BF lost id %d (false negative)", id)
		}
	}
}

func TestSegmentV2WithPKNoBloom(t *testing.T) {
	buf, _, _, _, _ := buildFixture(t, 5)
	var bb bytes.Buffer
	if err := WriteSegment(&bb, fixtureSchema(), buf, WriteSegmentOpts{
		PKName: "id",
	}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	seg, err := parseSegment(bb.Bytes())
	if err != nil {
		t.Fatalf("parseSegment: %v", err)
	}
	defer seg.Close()
	if seg.PKName() != "id" {
		t.Fatalf("PKName = %q; want \"id\"", seg.PKName())
	}
	if seg.Bloom() != nil {
		t.Fatalf("Bloom should be nil when not opted in")
	}
}

func TestSegmentBloomTrailerCorruption(t *testing.T) {
	buf, _, _, _, _ := buildFixture(t, 3)
	bf, _ := bloom.New(100, 0.01)
	bf.Add([]byte("x"))

	var bb bytes.Buffer
	if err := WriteSegment(&bb, fixtureSchema(), buf, WriteSegmentOpts{
		PKName: "id",
		Bloom:  bf,
	}); err != nil {
		t.Fatalf("WriteSegment: %v", err)
	}
	good := bb.Bytes()

	t.Run("bad trailer magic", func(t *testing.T) {
		data := bytes.Clone(good)
		// Find UBLM by scanning right-to-left; corrupt first byte.
		idx := bytes.Index(data, []byte(bloomTrailerMagic))
		if idx < 0 {
			t.Fatalf("trailer magic not found in test fixture")
		}
		data[idx] = 'X'
		if _, err := parseSegment(data); !errors.Is(err, ErrBadBloomMagic) {
			t.Fatalf("err = %v; want ErrBadBloomMagic", err)
		}
	})

	t.Run("unsupported trailer version", func(t *testing.T) {
		data := bytes.Clone(good)
		idx := bytes.Index(data, []byte(bloomTrailerMagic))
		// 2-byte trailer version sits right after the magic.
		binary.LittleEndian.PutUint16(data[idx+4:idx+6], 99)
		if _, err := parseSegment(data); !errors.Is(err, ErrUnsupportedBloomVersion) {
			t.Fatalf("err = %v; want ErrUnsupportedBloomVersion", err)
		}
	})

	t.Run("truncated trailer", func(t *testing.T) {
		idx := bytes.Index(good, []byte(bloomTrailerMagic))
		// Cut off mid-trailer (drop the last 4 bytes of the bloom body).
		if _, err := parseSegment(good[:idx+6+4]); err == nil {
			t.Fatalf("expected error on truncated trailer")
		}
	})

	t.Run("missing trailer header", func(t *testing.T) {
		// Cut right after the last column payload — bloom flag set but no
		// trailer bytes at all.
		idx := bytes.Index(good, []byte(bloomTrailerMagic))
		if _, err := parseSegment(good[:idx]); !errors.Is(err, ErrTruncated) {
			t.Fatalf("err = %v; want ErrTruncated", err)
		}
	})
}

func TestSegmentUnsupportedFutureVersion(t *testing.T) {
	data := validSegmentBytes(t)
	binary.LittleEndian.PutUint16(data[4:6], 99) // version > 2
	if _, err := parseSegment(data); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("err = %v; want ErrUnsupportedVersion", err)
	}
}
