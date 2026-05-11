package storage

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Encoding identifies how a column payload is serialized on disk.
type Encoding uint8

// Supported encodings. Values are stable wire constants.
const (
	// EncodingRaw stores values as a flat concatenation. Fixed-width
	// types take exactly 8 bytes each. Strings are framed per element
	// as [strLen: uvarint][utf8 bytes] because there is no other way
	// to recover variable-length values from a flat blob; the "no
	// length prefix" clause in the spec refers to the payload as a
	// whole, which the segment header's payloadLen already carries.
	EncodingRaw Encoding = 0
	// EncodingRLE stores values as run-length encoded runs; see rle.go.
	EncodingRLE Encoding = 1
)

// String returns the human-readable name of the encoding.
func (e Encoding) String() string {
	switch e {
	case EncodingRaw:
		return "raw"
	case EncodingRLE:
		return "rle"
	default:
		return fmt.Sprintf("Encoding(%d)", uint8(e))
	}
}

// chooseEncoding picks an encoding for a typed column slice.
//
// Today it always returns EncodingRLE; the signature exists so a future
// heuristic can drop in without callsite churn.
//
// TODO(iteration-2+): heuristic — e.g. pick RLE only when the estimated
// compression ratio exceeds ~1.2x, otherwise fall back to Raw.
func chooseEncoding(values any) Encoding {
	_ = values
	return EncodingRLE
}

func encodeRawInt64(values []int64) []byte {
	out := make([]byte, 8*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint64(out[i*8:], uint64(v))
	}
	return out
}

func decodeRawInt64(data []byte, expectedRows int) ([]int64, error) {
	if len(data) != 8*expectedRows {
		return nil, fmt.Errorf("raw int64: have %d bytes, expected %d", len(data), 8*expectedRows)
	}
	out := make([]int64, expectedRows)
	for i := range out {
		out[i] = int64(binary.LittleEndian.Uint64(data[i*8:]))
	}
	return out, nil
}

func encodeRawFloat64(values []float64) []byte {
	out := make([]byte, 8*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint64(out[i*8:], math.Float64bits(v))
	}
	return out
}

func decodeRawFloat64(data []byte, expectedRows int) ([]float64, error) {
	if len(data) != 8*expectedRows {
		return nil, fmt.Errorf("raw float64: have %d bytes, expected %d", len(data), 8*expectedRows)
	}
	out := make([]float64, expectedRows)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[i*8:]))
	}
	return out, nil
}

func encodeRawString(values []string) []byte {
	var out []byte
	var buf [binary.MaxVarintLen64]byte
	for _, s := range values {
		n := binary.PutUvarint(buf[:], uint64(len(s)))
		out = append(out, buf[:n]...)
		out = append(out, s...)
	}
	return out
}

func decodeRawString(data []byte, expectedRows int) ([]string, error) {
	out := make([]string, 0, expectedRows)
	offset := 0
	for offset < len(data) {
		if len(out) >= expectedRows {
			return nil, fmt.Errorf("raw string: more elements than expected %d", expectedRows)
		}
		strLen, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("raw string: bad length at offset %d", offset)
		}
		offset += n
		if strLen > uint64(len(data)-offset) {
			return nil, fmt.Errorf("raw string: truncated value at offset %d (need %d bytes)", offset, strLen)
		}
		out = append(out, string(data[offset:offset+int(strLen)]))
		offset += int(strLen)
	}
	if len(out) != expectedRows {
		return nil, fmt.Errorf("raw string: produced %d, expected %d", len(out), expectedRows)
	}
	return out, nil
}
