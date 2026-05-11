package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestSchemaValidate(t *testing.T) {
	tests := []struct {
		name    string
		schema  Schema
		wantErr error  // sentinel target; nil means expect success
		wantMsg string // optional substring required in the error message
	}{
		{
			name: "valid single column PK",
			schema: Schema{
				PK:      "id",
				Columns: []Column{{Name: "id", Type: Int64}},
			},
		},
		{
			name: "valid multi-column schema",
			schema: Schema{
				PK: "event_id",
				Columns: []Column{
					{Name: "event_id", Type: Int64},
					{Name: "amount", Type: Float64},
					{Name: "country", Type: String},
				},
			},
		},
		{
			name:    "no columns",
			schema:  Schema{PK: "id"},
			wantErr: ErrNoColumns,
		},
		{
			name: "duplicate column name",
			schema: Schema{
				PK: "id",
				Columns: []Column{
					{Name: "id", Type: Int64},
					{Name: "id", Type: String},
				},
			},
			wantErr: ErrDuplicateColumn,
			wantMsg: `"id"`,
		},
		{
			name: "PK missing",
			schema: Schema{
				PK: "missing",
				Columns: []Column{
					{Name: "id", Type: Int64},
				},
			},
			wantErr: ErrPKNotFound,
			wantMsg: `"missing"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.schema.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v; want errors.Is %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("Validate() = %q; want substring %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestSchemaColumnIndex(t *testing.T) {
	s := Schema{
		PK: "b",
		Columns: []Column{
			{Name: "a", Type: Int64},
			{Name: "b", Type: Float64},
			{Name: "c", Type: String},
		},
	}
	cases := []struct {
		lookup    string
		want      int
		wantFound bool
	}{
		{"a", 0, true},
		{"b", 1, true},
		{"c", 2, true},
		{"missing", -1, false},
		{"", -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.lookup, func(t *testing.T) {
			got, found := s.ColumnIndex(tc.lookup)
			if got != tc.want || found != tc.wantFound {
				t.Fatalf("ColumnIndex(%q) = (%d, %v); want (%d, %v)",
					tc.lookup, got, found, tc.want, tc.wantFound)
			}
		})
	}
}

func TestSchemaPKIndex(t *testing.T) {
	s := Schema{
		PK: "b",
		Columns: []Column{
			{Name: "a", Type: Int64},
			{Name: "b", Type: Float64},
		},
	}
	if got := s.PKIndex(); got != 1 {
		t.Fatalf("PKIndex() = %d; want 1", got)
	}

	missing := Schema{PK: "x", Columns: []Column{{Name: "a", Type: Int64}}}
	if got := missing.PKIndex(); got != -1 {
		t.Fatalf("PKIndex() = %d; want -1 for missing PK", got)
	}
}

func TestColumnTypeString(t *testing.T) {
	cases := []struct {
		t    ColumnType
		want string
	}{
		{Int64, "int64"},
		{Float64, "float64"},
		{String, "string"},
		{ColumnType(99), "ColumnType(99)"},
	}
	for _, c := range cases {
		if got := c.t.String(); got != c.want {
			t.Errorf("ColumnType(%d).String() = %q; want %q", c.t, got, c.want)
		}
	}
}
