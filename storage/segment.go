package storage

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ecoderat/uniqcol/bloom"
)

// Segment wire constants.
const (
	segmentMagic     = "UCOL"
	segmentVersionV1 = uint16(1)
	segmentVersionV2 = uint16(2)
	// segmentCurrentVersion is what WriteSegment emits. v1 is read-only.
	segmentCurrentVersion = segmentVersionV2
	segmentHeaderLen      = 16 // 4 magic + 2 version + 2 colCount + 8 rowCount

	bloomTrailerMagic   = "UBLM"
	bloomTrailerVersion = uint16(1)
)

// Flag bits set in the v2 flags block.
const (
	flagHasPKName uint8 = 1 << 0
	flagHasBloom  uint8 = 1 << 1
)

// Sentinel errors. Distinguishable with errors.Is.
var (
	// ErrBadMagic indicates the file does not start with the expected segment magic.
	ErrBadMagic = errors.New("segment: bad magic bytes")
	// ErrUnsupportedVersion indicates a segment version this build does not understand.
	// v1 (read-only) and v2 are accepted; v >= 3 returns this error.
	ErrUnsupportedVersion = errors.New("segment: unsupported version")
	// ErrTruncated indicates the segment ended before all advertised data was read.
	ErrTruncated = errors.New("segment: truncated input")
	// ErrUnknownColumn indicates a requested column name is not in the segment.
	ErrUnknownColumn = errors.New("segment: unknown column")
	// ErrBadBloomMagic indicates the Bloom trailer does not start with "UBLM".
	ErrBadBloomMagic = errors.New("segment: bad bloom trailer magic")
	// ErrUnsupportedBloomVersion indicates a Bloom trailer this build does not understand.
	ErrUnsupportedBloomVersion = errors.New("segment: unsupported bloom trailer version")
)

// Wire-level ColumnType tags. Intentionally distinct from the in-memory
// ColumnType constants in types.go (which use iota+1 so 0 remains a
// zero-value sentinel). All ColumnType <-> wire conversion goes through
// columnTypeToWire / wireToColumnType.
const (
	wireTypeInt64   uint8 = 0
	wireTypeFloat64 uint8 = 1
	wireTypeString  uint8 = 2
)

func columnTypeToWire(t ColumnType) (uint8, error) {
	switch t {
	case Int64:
		return wireTypeInt64, nil
	case Float64:
		return wireTypeFloat64, nil
	case String:
		return wireTypeString, nil
	default:
		return 0, fmt.Errorf("segment: unknown column type %s", t)
	}
}

func wireToColumnType(w uint8) (ColumnType, error) {
	switch w {
	case wireTypeInt64:
		return Int64, nil
	case wireTypeFloat64:
		return Float64, nil
	case wireTypeString:
		return String, nil
	default:
		return 0, fmt.Errorf("segment: unknown wire column type %d", w)
	}
}

// columnBlock records where a column's payload lives inside Segment.raw
// and how it was encoded. Populated by parseSegment; consumed lazily by
// (*Segment).ReadColumn.
type columnBlock struct {
	encoding   Encoding
	payloadOff int
	payloadLen int
}

// Segment is a memory-resident view of a segment file. Column payloads
// are decoded lazily on first ReadColumn and cached. Not safe for
// concurrent use.
//
// v1 segments (legacy, Iteration 1) carry no PK name and no Bloom
// trailer; PKName() returns "" and Bloom() returns nil. Schema.Validate
// will fail on such a schema because PK is absent — use OpenSegment for
// read-only column scans on v1 files. LoadTable refuses v1.
type Segment struct {
	version     uint16
	schema      Schema
	rowCount    uint64
	raw         []byte
	blocks      map[string]columnBlock
	decoded     map[string]any
	flags       uint8
	pkName      string
	bloomFilter *bloom.Filter
}

// WriteSegmentOpts holds optional v2 features. A zero value writes a
// minimal v2 segment with no PK name and no Bloom trailer (flags = 0).
type WriteSegmentOpts struct {
	// PKName, if non-empty, is persisted in the flags block so LoadTable
	// can rebuild a Schema with the right PK.
	PKName string
	// Bloom, if non-nil, is appended as a trailer.
	Bloom *bloom.Filter
}

// WriteSegment serializes buf to w in the v2 wire format. Does NOT
// close w. Iteration 1 (v1) is read-only; new writes always emit v2.
func WriteSegment(w io.Writer, schema Schema, buf *WriteBuffer, opts WriteSegmentOpts) error {
	if len(schema.Columns) > 0xFFFF {
		return fmt.Errorf("segment: too many columns (%d)", len(schema.Columns))
	}
	rowCount := uint64(buf.Len())

	var hdr [segmentHeaderLen]byte
	copy(hdr[0:4], segmentMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], segmentCurrentVersion)
	binary.LittleEndian.PutUint16(hdr[6:8], uint16(len(schema.Columns)))
	binary.LittleEndian.PutUint64(hdr[8:16], rowCount)
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("segment: write header: %w", err)
	}

	var flags uint8
	if opts.PKName != "" {
		flags |= flagHasPKName
	}
	if opts.Bloom != nil {
		flags |= flagHasBloom
	}
	var ub [binary.MaxVarintLen64]byte
	// flagsLen = 1 (one flags byte); future versions can grow the field.
	n := binary.PutUvarint(ub[:], 1)
	if _, err := w.Write(ub[:n]); err != nil {
		return fmt.Errorf("segment: write flagsLen: %w", err)
	}
	if _, err := w.Write([]byte{flags}); err != nil {
		return fmt.Errorf("segment: write flags: %w", err)
	}
	if flags&flagHasPKName != 0 {
		n = binary.PutUvarint(ub[:], uint64(len(opts.PKName)))
		if _, err := w.Write(ub[:n]); err != nil {
			return fmt.Errorf("segment: write pkName length: %w", err)
		}
		if _, err := io.WriteString(w, opts.PKName); err != nil {
			return fmt.Errorf("segment: write pkName: %w", err)
		}
	}

	for i, c := range schema.Columns {
		var (
			payload  []byte
			encoding Encoding
		)
		switch c.Type {
		case Int64:
			vals := buf.int64Cols[i]
			encoding = chooseEncoding(vals)
			if encoding == EncodingRLE {
				payload = encodeRLEInt64(vals)
			} else {
				payload = encodeRawInt64(vals)
			}
		case Float64:
			vals := buf.float64Cols[i]
			encoding = chooseEncoding(vals)
			if encoding == EncodingRLE {
				payload = encodeRLEFloat64(vals)
			} else {
				payload = encodeRawFloat64(vals)
			}
		case String:
			vals := buf.stringCols[i]
			encoding = chooseEncoding(vals)
			if encoding == EncodingRLE {
				payload = encodeRLEString(vals)
			} else {
				payload = encodeRawString(vals)
			}
		default:
			return fmt.Errorf("segment: column %q has unknown type %s", c.Name, c.Type)
		}

		wire, err := columnTypeToWire(c.Type)
		if err != nil {
			return err
		}

		n := binary.PutUvarint(ub[:], uint64(len(c.Name)))
		if _, err := w.Write(ub[:n]); err != nil {
			return fmt.Errorf("segment: write column %q name length: %w", c.Name, err)
		}
		if _, err := io.WriteString(w, c.Name); err != nil {
			return fmt.Errorf("segment: write column %q name: %w", c.Name, err)
		}
		if _, err := w.Write([]byte{wire, byte(encoding)}); err != nil {
			return fmt.Errorf("segment: write column %q tags: %w", c.Name, err)
		}
		n = binary.PutUvarint(ub[:], uint64(len(payload)))
		if _, err := w.Write(ub[:n]); err != nil {
			return fmt.Errorf("segment: write column %q payload length: %w", c.Name, err)
		}
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("segment: write column %q payload: %w", c.Name, err)
		}
	}

	if opts.Bloom != nil {
		if _, err := io.WriteString(w, bloomTrailerMagic); err != nil {
			return fmt.Errorf("segment: write bloom magic: %w", err)
		}
		var vbuf [2]byte
		binary.LittleEndian.PutUint16(vbuf[:], bloomTrailerVersion)
		if _, err := w.Write(vbuf[:]); err != nil {
			return fmt.Errorf("segment: write bloom version: %w", err)
		}
		body, err := opts.Bloom.MarshalBinary()
		if err != nil {
			return fmt.Errorf("segment: marshal bloom: %w", err)
		}
		if _, err := w.Write(body); err != nil {
			return fmt.Errorf("segment: write bloom body: %w", err)
		}
	}
	return nil
}

// ReadSegmentHeader reads the file header (and v2 flags block) and
// per-column metadata from r, skipping over column payloads and the
// Bloom trailer if any. Returns the reconstructed schema (PK populated
// only if the segment carries a PK name) and the row count.
func ReadSegmentHeader(r io.Reader) (Schema, uint64, error) {
	br := bufio.NewReader(r)

	var hdr [segmentHeaderLen]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Schema{}, 0, fmt.Errorf("%w: file header", ErrTruncated)
		}
		return Schema{}, 0, fmt.Errorf("segment: read header: %w", err)
	}
	if string(hdr[0:4]) != segmentMagic {
		return Schema{}, 0, fmt.Errorf("%w: got %q, want %q", ErrBadMagic, hdr[0:4], segmentMagic)
	}
	version := binary.LittleEndian.Uint16(hdr[4:6])
	if version != segmentVersionV1 && version != segmentVersionV2 {
		return Schema{}, 0, fmt.Errorf("%w: got %d, supported {1, 2}", ErrUnsupportedVersion, version)
	}
	columnCount := int(binary.LittleEndian.Uint16(hdr[6:8]))
	rowCount := binary.LittleEndian.Uint64(hdr[8:16])
	schema := Schema{Columns: make([]Column, 0, columnCount)}

	if version == segmentVersionV2 {
		flagsLen, err := binary.ReadUvarint(br)
		if err != nil {
			return Schema{}, 0, fmt.Errorf("%w: flagsLen", ErrTruncated)
		}
		if flagsLen == 0 {
			return Schema{}, 0, fmt.Errorf("segment: v2 flagsLen must be >= 1")
		}
		flagsBuf := make([]byte, flagsLen)
		if _, err := io.ReadFull(br, flagsBuf); err != nil {
			return Schema{}, 0, fmt.Errorf("%w: flags payload", ErrTruncated)
		}
		flags := flagsBuf[0]
		if flags&flagHasPKName != 0 {
			nameLen, err := binary.ReadUvarint(br)
			if err != nil {
				return Schema{}, 0, fmt.Errorf("%w: pkName length", ErrTruncated)
			}
			nameBuf := make([]byte, nameLen)
			if _, err := io.ReadFull(br, nameBuf); err != nil {
				return Schema{}, 0, fmt.Errorf("%w: pkName", ErrTruncated)
			}
			schema.PK = string(nameBuf)
		}
	}

	for i := range columnCount {
		nameLen, err := binary.ReadUvarint(br)
		if err != nil {
			return Schema{}, 0, fmt.Errorf("%w: column %d name length", ErrTruncated, i)
		}
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(br, nameBuf); err != nil {
			return Schema{}, 0, fmt.Errorf("%w: column %d name", ErrTruncated, i)
		}
		var tagBuf [2]byte
		if _, err := io.ReadFull(br, tagBuf[:]); err != nil {
			return Schema{}, 0, fmt.Errorf("%w: column %d tags", ErrTruncated, i)
		}
		colType, err := wireToColumnType(tagBuf[0])
		if err != nil {
			return Schema{}, 0, fmt.Errorf("segment: column %d (%q): %w", i, nameBuf, err)
		}
		payloadLen, err := binary.ReadUvarint(br)
		if err != nil {
			return Schema{}, 0, fmt.Errorf("%w: column %d payload length", ErrTruncated, i)
		}
		if _, err := io.CopyN(io.Discard, br, int64(payloadLen)); err != nil {
			return Schema{}, 0, fmt.Errorf("%w: column %d payload", ErrTruncated, i)
		}
		schema.Columns = append(schema.Columns, Column{Name: string(nameBuf), Type: colType})
	}
	return schema, rowCount, nil
}

// OpenSegment reads the segment file at path into memory and parses its
// header, optional flags / PK name, per-column metadata, and optional
// Bloom trailer. Column payloads are not decoded until ReadColumn.
//
// TODO: switch to mmap or windowed reads when segments grow beyond what
// we want to load eagerly.
func OpenSegment(path string) (*Segment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("segment: open %q: %w", path, err)
	}
	return parseSegment(data)
}

func parseSegment(data []byte) (*Segment, error) {
	if len(data) < segmentHeaderLen {
		return nil, fmt.Errorf("%w: file shorter than header (%d bytes)", ErrTruncated, len(data))
	}
	if string(data[0:4]) != segmentMagic {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrBadMagic, data[0:4], segmentMagic)
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != segmentVersionV1 && version != segmentVersionV2 {
		return nil, fmt.Errorf("%w: got %d, supported {1, 2}", ErrUnsupportedVersion, version)
	}
	columnCount := int(binary.LittleEndian.Uint16(data[6:8]))
	rowCount := binary.LittleEndian.Uint64(data[8:16])

	s := &Segment{
		version:  version,
		rowCount: rowCount,
		raw:      data,
		blocks:   make(map[string]columnBlock, columnCount),
		decoded:  make(map[string]any),
	}
	s.schema.Columns = make([]Column, 0, columnCount)

	offset := segmentHeaderLen

	if version == segmentVersionV2 {
		flagsLen, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: flagsLen at offset %d", ErrTruncated, offset)
		}
		offset += n
		if flagsLen == 0 {
			return nil, fmt.Errorf("segment: v2 flagsLen must be >= 1")
		}
		if flagsLen > uint64(len(data)-offset) {
			return nil, fmt.Errorf("%w: flags payload (need %d bytes)", ErrTruncated, flagsLen)
		}
		s.flags = data[offset]
		offset += int(flagsLen)
		if s.flags&flagHasPKName != 0 {
			nameLen, n := binary.Uvarint(data[offset:])
			if n <= 0 {
				return nil, fmt.Errorf("%w: pkName length at offset %d", ErrTruncated, offset)
			}
			offset += n
			if nameLen > uint64(len(data)-offset) {
				return nil, fmt.Errorf("%w: pkName (need %d bytes)", ErrTruncated, nameLen)
			}
			s.pkName = string(data[offset : offset+int(nameLen)])
			s.schema.PK = s.pkName
			offset += int(nameLen)
		}
	}

	for i := range columnCount {
		nameLen, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: column %d name length at offset %d", ErrTruncated, i, offset)
		}
		offset += n
		if nameLen > uint64(len(data)-offset) {
			return nil, fmt.Errorf("%w: column %d name (need %d bytes)", ErrTruncated, i, nameLen)
		}
		name := string(data[offset : offset+int(nameLen)])
		offset += int(nameLen)

		if offset+2 > len(data) {
			return nil, fmt.Errorf("%w: column %d tags (%q)", ErrTruncated, i, name)
		}
		wireType := data[offset]
		encoding := Encoding(data[offset+1])
		offset += 2
		colType, err := wireToColumnType(wireType)
		if err != nil {
			return nil, fmt.Errorf("segment: column %d (%q): %w", i, name, err)
		}
		if encoding != EncodingRaw && encoding != EncodingRLE {
			return nil, fmt.Errorf("segment: column %d (%q): unknown encoding %d", i, name, encoding)
		}

		payloadLen, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: column %d (%q) payload length at offset %d", ErrTruncated, i, name, offset)
		}
		offset += n
		if payloadLen > uint64(len(data)-offset) {
			return nil, fmt.Errorf("%w: column %d (%q) payload (need %d, have %d)", ErrTruncated, i, name, payloadLen, len(data)-offset)
		}

		s.schema.Columns = append(s.schema.Columns, Column{Name: name, Type: colType})
		s.blocks[name] = columnBlock{
			encoding:   encoding,
			payloadOff: offset,
			payloadLen: int(payloadLen),
		}
		offset += int(payloadLen)
	}

	if version == segmentVersionV2 && s.flags&flagHasBloom != 0 {
		const trailerHeader = 4 + 2 // magic + version
		if len(data)-offset < trailerHeader {
			return nil, fmt.Errorf("%w: bloom trailer header", ErrTruncated)
		}
		if string(data[offset:offset+4]) != bloomTrailerMagic {
			return nil, fmt.Errorf("%w: got %q, want %q", ErrBadBloomMagic, data[offset:offset+4], bloomTrailerMagic)
		}
		offset += 4
		bv := binary.LittleEndian.Uint16(data[offset : offset+2])
		offset += 2
		if bv != bloomTrailerVersion {
			return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedBloomVersion, bv, bloomTrailerVersion)
		}
		var bf bloom.Filter
		if err := bf.UnmarshalBinary(data[offset:]); err != nil {
			return nil, fmt.Errorf("segment: bloom trailer: %w", err)
		}
		s.bloomFilter = &bf
	}

	return s, nil
}

// Version returns the segment's wire-format version (1 or 2).
func (s *Segment) Version() uint16 { return s.version }

// Schema returns the schema reconstructed from the segment. PK is set
// only if a PK name was persisted (v2 with flagHasPKName); v1 segments
// have PK == "".
func (s *Segment) Schema() Schema { return s.schema }

// RowCount returns the number of rows stored in the segment.
func (s *Segment) RowCount() uint64 { return s.rowCount }

// PKName returns the segment's PK column name, or "" if not present.
func (s *Segment) PKName() string { return s.pkName }

// Bloom returns the segment's Bloom filter, or nil if no trailer was
// present. The returned filter is owned by the Segment and should not
// be mutated by callers (LoadTable hands it off to a new Table).
func (s *Segment) Bloom() *bloom.Filter { return s.bloomFilter }

// ReadColumn decodes and returns the named column. The concrete return
// type is []int64, []float64, or []string depending on the column's
// declared type. Repeated calls return the cached decoded slice.
func (s *Segment) ReadColumn(name string) (any, error) {
	if v, ok := s.decoded[name]; ok {
		return v, nil
	}
	block, ok := s.blocks[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownColumn, name)
	}
	idx, _ := s.schema.ColumnIndex(name)
	colType := s.schema.Columns[idx].Type
	payload := s.raw[block.payloadOff : block.payloadOff+block.payloadLen]
	rows := int(s.rowCount)

	var (
		v   any
		err error
	)
	switch colType {
	case Int64:
		if block.encoding == EncodingRLE {
			v, err = decodeRLEInt64(payload, rows)
		} else {
			v, err = decodeRawInt64(payload, rows)
		}
	case Float64:
		if block.encoding == EncodingRLE {
			v, err = decodeRLEFloat64(payload, rows)
		} else {
			v, err = decodeRawFloat64(payload, rows)
		}
	case String:
		if block.encoding == EncodingRLE {
			v, err = decodeRLEString(payload, rows)
		} else {
			v, err = decodeRawString(payload, rows)
		}
	default:
		return nil, fmt.Errorf("segment: column %q has unknown type %s", name, colType)
	}
	if err != nil {
		return nil, fmt.Errorf("segment: decode column %q: %w", name, err)
	}
	s.decoded[name] = v
	return v, nil
}

// Close releases the segment's in-memory buffers. After Close the
// segment must not be used.
func (s *Segment) Close() error {
	s.raw = nil
	s.blocks = nil
	s.decoded = nil
	s.bloomFilter = nil
	return nil
}
