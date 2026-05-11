// Package storage defines the in-memory and (in later iterations) on-disk
// data structures of uniqcol's columnar engine. This file declares the
// type system and table schema.
package storage

import (
	"errors"
	"fmt"
)

// ColumnType identifies the storage type of a column.
//
// uniqcol intentionally supports only three primitive types; date/time,
// decimal and nested types are out of scope (see README, "Kapsam ve
// Sınırlamalar").
type ColumnType uint8

// Supported column types. Values are stable and must only be appended:
// they will be persisted in segment headers in a later iteration.
const (
	// Int64 is a 64-bit signed integer column.
	Int64 ColumnType = iota + 1
	// Float64 is an IEEE-754 double-precision floating point column.
	Float64
	// String is a UTF-8 string column with no length cap.
	String
)

// String returns the human-readable name of the type. It is used in
// error messages and (eventually) segment headers.
func (t ColumnType) String() string {
	switch t {
	case Int64:
		return "int64"
	case Float64:
		return "float64"
	case String:
		return "string"
	default:
		return fmt.Sprintf("ColumnType(%d)", uint8(t))
	}
}

// Column describes a single column in a Schema.
type Column struct {
	// Name is the column's identifier; must be unique within a Schema.
	Name string
	// Type is one of Int64, Float64, String.
	Type ColumnType
}

// Schema describes a table: its columns and which column acts as the
// primary key for Bloom-filter dedup.
type Schema struct {
	// PK names the primary-key column; it must match one entry in Columns.
	PK string
	// Columns lists the columns in positional order. The order is
	// significant: Row values are positional.
	Columns []Column
}

// Row is a positional list of values whose length and element types must
// match a Schema's Columns. nil values are not allowed; uniqcol has no
// NULL semantics in this iteration.
type Row []any

// Sentinel errors returned by Schema.Validate. Callers can match with
// errors.Is to react to specific schema problems.
var (
	// ErrNoColumns indicates that Schema.Columns is empty.
	ErrNoColumns = errors.New("schema has no columns")
	// ErrDuplicateColumn indicates that two columns share a name.
	ErrDuplicateColumn = errors.New("duplicate column name")
	// ErrPKNotFound indicates that Schema.PK does not match any column.
	ErrPKNotFound = errors.New("primary key column not found")
)

// Validate reports the first structural error in the schema, or nil if
// the schema is well-formed. The checks are:
//   - at least one column
//   - column names are unique
//   - PK names a column in Columns
func (s Schema) Validate() error {
	if len(s.Columns) == 0 {
		return ErrNoColumns
	}
	seen := make(map[string]struct{}, len(s.Columns))
	for _, c := range s.Columns {
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateColumn, c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	if _, ok := seen[s.PK]; !ok {
		return fmt.Errorf("%w: %q", ErrPKNotFound, s.PK)
	}
	return nil
}

// ColumnIndex returns the positional index of the column named name.
// The second return value is false if no column matches; the index is
// then -1.
func (s Schema) ColumnIndex(name string) (int, bool) {
	for i, c := range s.Columns {
		if c.Name == name {
			return i, true
		}
	}
	return -1, false
}

// PKIndex returns the positional index of the primary-key column, or -1
// if PK does not match any column. Callers should Validate the schema
// first to rule out the -1 case.
func (s Schema) PKIndex() int {
	i, _ := s.ColumnIndex(s.PK)
	return i
}
