package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/ecoderat/uniqcol/bloom"
)

// ErrIncompatibleSegment is returned by LoadTable when the on-disk
// segment cannot back a writable Table — typically because it is a v1
// (legacy) segment with no PK name or no Bloom filter persisted.
var ErrIncompatibleSegment = errors.New("table: incompatible segment")

// TableOptions configures a new Table.
type TableOptions struct {
	// BloomExpectedItems sizes the Bloom filter for the anticipated row
	// count. Once exceeded, the filter's false-positive rate grows beyond
	// the target and rebuilding is recommended (see EstimatedFPR).
	// Ignored when BloomDisabled is true.
	BloomExpectedItems uint64
	// BloomTargetFPR is the desired false-positive rate at saturation,
	// in (0, 1). Typical values: 0.01 (1%) or 0.001 (0.1%). Ignored when
	// BloomDisabled is true.
	BloomTargetFPR float64
	// BloomDisabled turns off write-time dedup entirely. The Table is
	// constructed without a filter, Insert always accepts (subject to
	// type validation), and Flush writes a segment with no Bloom trailer.
	//
	// This is a write-only / benchmark-only mode: LoadTable refuses
	// segments without a Bloom trailer, because uniqueness cannot be
	// enforced on reload. Use it for measuring the cost of the filter,
	// not for production ingest.
	BloomDisabled bool
}

// InsertResult is the outcome of a single Table.Insert call.
type InsertResult struct {
	// Accepted reports whether the row was written to the buffer.
	Accepted bool
	// Reason explains a rejection. Empty when Accepted is true.
	Reason string
}

// TableStats holds running counters.
type TableStats struct {
	Accepted, Rejected uint64
	BufferLen          int
}

// Table is uniqcol's top-level write surface. It owns a schema, a
// column-major write buffer, and a Bloom filter keyed on the schema's
// PK column. Insert enforces write-time uniqueness: a row whose PK is
// already in the filter is rejected without being buffered.
//
// IMPORTANT: rejection is "probably duplicate," not "definitely
// duplicate." A Bloom filter false positive will reject a genuinely
// unique key. The expected false-positive rate is bounded by the
// filter's target FPR at the configured BloomExpectedItems; benchmarks
// measure the actual rate and the project report owns this trade-off
// as a design choice rather than a bug.
//
// Not safe for concurrent use; uniqcol assumes a single writer.
type Table struct {
	schema   Schema
	buf      *WriteBuffer
	bloom    *bloom.Filter
	pkIdx    int
	pkType   ColumnType
	accepted uint64
	rejected uint64
}

// CreateTable builds a fresh Table around schema and (unless
// BloomDisabled) a newly constructed Bloom filter. Returns an error if
// the schema fails Validate or the Bloom parameters are out of range.
func CreateTable(schema Schema, opts TableOptions) (*Table, error) {
	if err := schema.Validate(); err != nil {
		return nil, fmt.Errorf("table: schema: %w", err)
	}
	var bf *bloom.Filter
	if !opts.BloomDisabled {
		var err error
		bf, err = bloom.New(opts.BloomExpectedItems, opts.BloomTargetFPR)
		if err != nil {
			return nil, fmt.Errorf("table: bloom: %w", err)
		}
	}
	pkIdx := schema.PKIndex()
	return &Table{
		schema: schema,
		buf:    NewWriteBuffer(schema),
		bloom:  bf,
		pkIdx:  pkIdx,
		pkType: schema.Columns[pkIdx].Type,
	}, nil
}

// LoadTable opens path, requires a v2 segment with both a PK name and a
// Bloom trailer, and returns a Table whose buffer is empty and whose
// Bloom filter carries the loaded state. Further Insert calls dedup
// against keys already accepted in past sessions.
//
// Returns ErrIncompatibleSegment if path is v1 or missing PK / Bloom.
func LoadTable(path string) (*Table, error) {
	seg, err := OpenSegment(path)
	if err != nil {
		return nil, err
	}
	if seg.Version() != segmentVersionV2 {
		return nil, fmt.Errorf("%w: segment is v%d (read-only); use OpenSegment to read columns",
			ErrIncompatibleSegment, seg.Version())
	}
	if seg.PKName() == "" {
		return nil, fmt.Errorf("%w: no PK name persisted", ErrIncompatibleSegment)
	}
	if seg.Bloom() == nil {
		return nil, fmt.Errorf("%w: no Bloom trailer", ErrIncompatibleSegment)
	}
	schema := seg.Schema()
	if err := schema.Validate(); err != nil {
		return nil, fmt.Errorf("table: loaded schema invalid: %w", err)
	}
	pkIdx := schema.PKIndex()
	return &Table{
		schema: schema,
		buf:    NewWriteBuffer(schema),
		bloom:  seg.Bloom(),
		pkIdx:  pkIdx,
		pkType: schema.Columns[pkIdx].Type,
	}, nil
}

// Insert validates row and, unless the Bloom filter is disabled, checks
// it on the PK column. On accept the row is appended to the buffer; on
// reject the buffer is left unchanged and the reason is returned.
func (t *Table) Insert(row Row) InsertResult {
	if len(row) != len(t.schema.Columns) {
		t.rejected++
		return InsertResult{
			Reason: fmt.Sprintf("type error: row has %d values, schema has %d columns",
				len(row), len(t.schema.Columns)),
		}
	}
	if t.bloom != nil {
		pkBytes, err := pkBytes(row[t.pkIdx], t.pkType)
		if err != nil {
			t.rejected++
			return InsertResult{
				Reason: fmt.Sprintf("type error: PK column %q: %v",
					t.schema.Columns[t.pkIdx].Name, err),
			}
		}
		if t.bloom.Contains(pkBytes) {
			t.rejected++
			return InsertResult{Reason: "duplicate (bloom positive)"}
		}
		if err := t.buf.Append(row); err != nil {
			t.rejected++
			return InsertResult{Reason: fmt.Sprintf("type error: %v", err)}
		}
		t.bloom.Add(pkBytes)
		t.accepted++
		return InsertResult{Accepted: true}
	}
	// BloomDisabled path: validate types via buffer Append; no dedup.
	if err := t.buf.Append(row); err != nil {
		t.rejected++
		return InsertResult{Reason: fmt.Sprintf("type error: %v", err)}
	}
	t.accepted++
	return InsertResult{Accepted: true}
}

// Flush writes the buffered rows plus the current Bloom filter and PK
// name as a v2 segment to w. Does NOT close w.
func (t *Table) Flush(w io.Writer) error {
	return WriteSegment(w, t.schema, t.buf, WriteSegmentOpts{
		PKName: t.schema.PK,
		Bloom:  t.bloom,
	})
}

// Stats returns the running insertion counters and the current buffer length.
func (t *Table) Stats() TableStats {
	return TableStats{
		Accepted:  t.accepted,
		Rejected:  t.rejected,
		BufferLen: t.buf.Len(),
	}
}

// Schema returns the table's schema.
func (t *Table) Schema() Schema { return t.schema }

// Bloom returns the table's Bloom filter for diagnostics. Callers
// should not mutate it directly.
func (t *Table) Bloom() *bloom.Filter { return t.bloom }

// pkBytes returns canonical bytes for hashing the PK column value:
//   - Int64:   8 bytes little-endian
//   - Float64: 8 bytes little-endian of math.Float64bits (so distinct
//     NaN bit patterns key to distinct entries)
//   - String:  raw UTF-8 bytes (copied so callers cannot mutate)
//
// Returns an error if v's concrete type does not match t.
func pkBytes(v any, t ColumnType) ([]byte, error) {
	switch t {
	case Int64:
		x, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("expected int64, got %T", v)
		}
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, uint64(x))
		return b, nil
	case Float64:
		x, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("expected float64, got %T", v)
		}
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, math.Float64bits(x))
		return b, nil
	case String:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", v)
		}
		// Defensive copy via []byte(s) — string conversion allocates.
		return []byte(s), nil
	default:
		return nil, fmt.Errorf("unsupported PK type %s", t)
	}
}
