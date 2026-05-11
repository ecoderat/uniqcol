package storage

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Segment wire constants.
const (
	segmentMagic            = "UCOL"
	segmentVersion   uint16 = 1
	segmentHeaderLen        = 16 // 4 magic + 2 version + 2 colCount + 8 rowCount
)

// Sentinel errors returned by the segment reader. Distinguishable with errors.Is.
var (
	// ErrBadMagic indicates the file does not start with the expected magic bytes.
	ErrBadMagic = errors.New("segment: bad magic bytes")
	// ErrUnsupportedVersion indicates a segment version this build does not understand.
	ErrUnsupportedVersion = errors.New("segment: unsupported version")
	// ErrTruncated indicates the segment ended before all advertised data was read.
	ErrTruncated = errors.New("segment: truncated input")
	// ErrUnknownColumn indicates a requested column name is not in the segment.
	ErrUnknownColumn = errors.New("segment: unknown column")
)

// Wire-level ColumnType tags. These are intentionally distinct from the
// in-memory ColumnType constants in types.go (which use iota+1 so 0
// remains a zero-value sentinel). All ColumnType <-> wire conversion
// goes through columnTypeToWire / wireToColumnType to keep the two
// representations decoupled.
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
// The Schema returned by Schema() does NOT carry a PK: the segment wire
// format does not persist PK. Callers that need PK should supply it
// from configuration. Schema.Validate will therefore fail on a
// segment-loaded Schema with the original PK absent.
type Segment struct {
	schema   Schema
	rowCount uint64
	raw      []byte
	blocks   map[string]columnBlock
	decoded  map[string]any
}

// WriteSegment serializes the buffer to w in the segment wire format
// described in the README ("Veri Formatı"). It does NOT close w.
//
// The PK is not persisted; segment readers will return a Schema with
// an empty PK.
func WriteSegment(w io.Writer, schema Schema, buf *WriteBuffer) error {
	if len(schema.Columns) > 0xFFFF {
		return fmt.Errorf("segment: too many columns (%d)", len(schema.Columns))
	}
	rowCount := uint64(buf.Len())

	var hdr [segmentHeaderLen]byte
	copy(hdr[0:4], segmentMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], segmentVersion)
	binary.LittleEndian.PutUint16(hdr[6:8], uint16(len(schema.Columns)))
	binary.LittleEndian.PutUint64(hdr[8:16], rowCount)
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("segment: write header: %w", err)
	}

	var ub [binary.MaxVarintLen64]byte
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
	return nil
}

// ReadSegmentHeader reads the file header and per-column metadata from
// r, skipping over column payloads. It returns the reconstructed schema
// (with empty PK; see Segment) and the segment's row count. The reader
// is left positioned just after the last column payload.
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
	if version != segmentVersion {
		return Schema{}, 0, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, version, segmentVersion)
	}
	columnCount := int(binary.LittleEndian.Uint16(hdr[6:8]))
	rowCount := binary.LittleEndian.Uint64(hdr[8:16])

	schema := Schema{Columns: make([]Column, 0, columnCount)}
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
// header and per-column metadata. Column payloads are not decoded until
// ReadColumn is called.
//
// TODO: switch to mmap or windowed reads when segment sizes grow beyond
// what we want to load eagerly.
func OpenSegment(path string) (*Segment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("segment: open %q: %w", path, err)
	}
	return parseSegment(data)
}

// parseSegment parses an in-memory segment image. It is the shared
// implementation behind OpenSegment and is used directly by tests.
func parseSegment(data []byte) (*Segment, error) {
	if len(data) < segmentHeaderLen {
		return nil, fmt.Errorf("%w: file shorter than header (%d bytes)", ErrTruncated, len(data))
	}
	if string(data[0:4]) != segmentMagic {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrBadMagic, data[0:4], segmentMagic)
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != segmentVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, version, segmentVersion)
	}
	columnCount := int(binary.LittleEndian.Uint16(data[6:8]))
	rowCount := binary.LittleEndian.Uint64(data[8:16])

	s := &Segment{
		rowCount: rowCount,
		raw:      data,
		blocks:   make(map[string]columnBlock, columnCount),
		decoded:  make(map[string]any),
	}
	s.schema.Columns = make([]Column, 0, columnCount)

	offset := segmentHeaderLen
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
	return s, nil
}

// Schema returns the schema reconstructed from the segment header.
// The PK field is empty; see Segment doc.
func (s *Segment) Schema() Schema { return s.schema }

// RowCount returns the number of rows stored in the segment.
func (s *Segment) RowCount() uint64 { return s.rowCount }

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
	return nil
}
