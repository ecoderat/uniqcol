package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrRLECorrupt indicates that an RLE payload is malformed: it ended
// before producing expectedRows values, contained trailing garbage, or
// had a syntactically invalid run.
var ErrRLECorrupt = errors.New("rle: corrupt payload")

// RLE wire format
// ---------------
// A payload is the concatenation of zero or more runs. Each run is:
//
//	[count: uvarint][value: type-specific bytes]
//
// Fixed-width values are written little-endian. Strings are framed
// in-run as [strLen: uvarint][utf8 bytes]. The total row count is NOT
// embedded in the payload; the segment layer supplies it via
// expectedRows and the decoder validates the result against it.

func encodeRLEInt64(values []int64) []byte {
	if len(values) == 0 {
		return []byte{}
	}
	out := make([]byte, 0, len(values))
	var vbuf [binary.MaxVarintLen64]byte
	var lbuf [8]byte
	for i := 0; i < len(values); {
		j := i + 1
		for j < len(values) && values[j] == values[i] {
			j++
		}
		n := binary.PutUvarint(vbuf[:], uint64(j-i))
		out = append(out, vbuf[:n]...)
		binary.LittleEndian.PutUint64(lbuf[:], uint64(values[i]))
		out = append(out, lbuf[:]...)
		i = j
	}
	return out
}

func decodeRLEInt64(data []byte, expectedRows int) ([]int64, error) {
	out := make([]int64, 0, expectedRows)
	offset := 0
	for offset < len(data) {
		count, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: bad run count at offset %d", ErrRLECorrupt, offset)
		}
		offset += n
		if offset+8 > len(data) {
			return nil, fmt.Errorf("%w: truncated int64 value at offset %d", ErrRLECorrupt, offset)
		}
		v := int64(binary.LittleEndian.Uint64(data[offset:]))
		offset += 8
		remaining := expectedRows - len(out)
		if remaining < 0 || count > uint64(remaining) {
			return nil, fmt.Errorf("%w: produced more than %d rows", ErrRLECorrupt, expectedRows)
		}
		for range count {
			out = append(out, v)
		}
	}
	if len(out) != expectedRows {
		return nil, fmt.Errorf("%w: produced %d rows, expected %d", ErrRLECorrupt, len(out), expectedRows)
	}
	return out, nil
}

func encodeRLEFloat64(values []float64) []byte {
	if len(values) == 0 {
		return []byte{}
	}
	out := make([]byte, 0, len(values))
	var vbuf [binary.MaxVarintLen64]byte
	var lbuf [8]byte
	for i := 0; i < len(values); {
		j := i + 1
		// Bit-level equality keeps NaNs grouped deterministically.
		for j < len(values) && math.Float64bits(values[j]) == math.Float64bits(values[i]) {
			j++
		}
		n := binary.PutUvarint(vbuf[:], uint64(j-i))
		out = append(out, vbuf[:n]...)
		binary.LittleEndian.PutUint64(lbuf[:], math.Float64bits(values[i]))
		out = append(out, lbuf[:]...)
		i = j
	}
	return out
}

func decodeRLEFloat64(data []byte, expectedRows int) ([]float64, error) {
	out := make([]float64, 0, expectedRows)
	offset := 0
	for offset < len(data) {
		count, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: bad run count at offset %d", ErrRLECorrupt, offset)
		}
		offset += n
		if offset+8 > len(data) {
			return nil, fmt.Errorf("%w: truncated float64 value at offset %d", ErrRLECorrupt, offset)
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(data[offset:]))
		offset += 8
		remaining := expectedRows - len(out)
		if remaining < 0 || count > uint64(remaining) {
			return nil, fmt.Errorf("%w: produced more than %d rows", ErrRLECorrupt, expectedRows)
		}
		for range count {
			out = append(out, v)
		}
	}
	if len(out) != expectedRows {
		return nil, fmt.Errorf("%w: produced %d rows, expected %d", ErrRLECorrupt, len(out), expectedRows)
	}
	return out, nil
}

func encodeRLEString(values []string) []byte {
	if len(values) == 0 {
		return []byte{}
	}
	out := make([]byte, 0, len(values)*4)
	var vbuf [binary.MaxVarintLen64]byte
	for i := 0; i < len(values); {
		j := i + 1
		for j < len(values) && values[j] == values[i] {
			j++
		}
		n := binary.PutUvarint(vbuf[:], uint64(j-i))
		out = append(out, vbuf[:n]...)
		s := values[i]
		n = binary.PutUvarint(vbuf[:], uint64(len(s)))
		out = append(out, vbuf[:n]...)
		out = append(out, s...)
		i = j
	}
	return out
}

func decodeRLEString(data []byte, expectedRows int) ([]string, error) {
	out := make([]string, 0, expectedRows)
	offset := 0
	for offset < len(data) {
		count, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: bad run count at offset %d", ErrRLECorrupt, offset)
		}
		offset += n
		strLen, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: bad string length at offset %d", ErrRLECorrupt, offset)
		}
		offset += n
		if strLen > uint64(len(data)-offset) {
			return nil, fmt.Errorf("%w: truncated string value at offset %d (need %d bytes)", ErrRLECorrupt, offset, strLen)
		}
		// string(b) allocates, so out does not pin data.
		s := string(data[offset : offset+int(strLen)])
		offset += int(strLen)
		remaining := expectedRows - len(out)
		if remaining < 0 || count > uint64(remaining) {
			return nil, fmt.Errorf("%w: produced more than %d rows", ErrRLECorrupt, expectedRows)
		}
		for range count {
			out = append(out, s)
		}
	}
	if len(out) != expectedRows {
		return nil, fmt.Errorf("%w: produced %d rows, expected %d", ErrRLECorrupt, len(out), expectedRows)
	}
	return out, nil
}
