package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ecoderat/uniqcol/storage"
)

// writeCSV creates a CSV at path with the given header and row records.
// Returns the path for convenience.
func writeCSV(t *testing.T, path string, header []string, rows [][]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(strings.Join(header, ","))
	b.WriteByte('\n')
	for _, r := range rows {
		b.WriteString(strings.Join(r, ","))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writeCSV: %v", err)
	}
	return path
}

func TestParseSchemaSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantLen int
		wantErr bool
	}{
		{"single int64", "id:int64", 1, false},
		{"three types", "id:int64,amount:float64,country:string", 3, false},
		{"empty", "", 0, true},
		{"missing colon", "id-int64", 0, true},
		{"empty name", ":int64", 0, true},
		{"unknown type", "id:int128", 0, true},
		{"duplicate column", "id:int64,id:string", 0, true},
		{"whitespace tolerated", "  id:int64 , amount:float64 ", 2, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cols, err := parseSchemaSpec(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", cols)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cols) != tc.wantLen {
				t.Fatalf("len = %d; want %d", len(cols), tc.wantLen)
			}
		})
	}
}

func TestParseFieldByType(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		typ     storage.ColumnType
		wantVal any
		wantErr bool
	}{
		{"int64 ok", "42", storage.Int64, int64(42), false},
		{"int64 negative", "-7", storage.Int64, int64(-7), false},
		{"int64 empty", "", storage.Int64, nil, true},
		{"int64 bad", "x", storage.Int64, nil, true},
		{"float64 ok", "3.14", storage.Float64, 3.14, false},
		{"float64 empty", "", storage.Float64, nil, true},
		{"string ok", "hello", storage.String, "hello", false},
		{"string empty ok", "", storage.String, "", false},
		{"unsupported type", "x", storage.ColumnType(99), nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFieldByType(tc.field, tc.typ)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantVal {
				t.Errorf("got %v (%T); want %v (%T)", got, got, tc.wantVal, tc.wantVal)
			}
		})
	}
}

// buildEventsCSV writes a small events.csv at dir with unique + duplicate
// rows. Returns the path and the count of unique PKs.
func buildEventsCSV(t *testing.T, dir string, uniques, dups int) (string, int) {
	t.Helper()
	header := []string{"event_id", "user_id", "amount", "country"}
	countries := []string{"TR", "US", "DE", "GB", "FR"}
	var rows [][]string
	for i := range uniques {
		rows = append(rows, []string{
			fmt.Sprintf("%d", 1000+i),
			fmt.Sprintf("%d", 100+i),
			fmt.Sprintf("%.2f", float64(i)*1.5),
			countries[i%len(countries)],
		})
	}
	for i := range dups {
		// duplicate event_id of the i'th unique row
		rows = append(rows, []string{
			fmt.Sprintf("%d", 1000+(i%uniques)),
			"999", "9.99", "ZZ",
		})
	}
	path := filepath.Join(dir, "events.csv")
	writeCSV(t, path, header, rows)
	return path, uniques
}

func TestRunLoad_HappyPath(t *testing.T) {
	dir := t.TempDir()
	csvPath, uniques := buildEventsCSV(t, dir, 90, 10) // 100 rows total
	outPath := filepath.Join(dir, "events.uniq")

	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", outPath,
		"--pk", "event_id",
		"--schema", "event_id:int64,user_id:int64,amount:float64,country:string",
		"--expected-items", "1000",
		"--target-fpr", "0.001",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, sub := range []string{
		"rows read:", "accepted:", "rejected (BF):", "throughput:", "segment size:",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("output missing %q\n---\n%s", sub, out)
		}
	}

	seg, err := storage.OpenSegment(outPath)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()
	if got := int(seg.RowCount()); got < uniques-2 || got > uniques+2 {
		t.Errorf("RowCount = %d; want ~%d (allowing ±2 for BF FPs)", got, uniques)
	}
	if seg.Bloom() == nil {
		t.Errorf("expected segment to carry a Bloom trailer")
	}
}

func TestRunLoad_NoBloom(t *testing.T) {
	dir := t.TempDir()
	csvPath, uniques := buildEventsCSV(t, dir, 90, 10) // 100 rows, 10 duplicates
	outPath := filepath.Join(dir, "events.uniq")

	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", outPath,
		"--pk", "event_id",
		"--schema", "event_id:int64,user_id:int64,amount:float64,country:string",
		"--no-bloom",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	// BF lines should be dashes, not numbers.
	if !strings.Contains(out, "rejected (BF):    —") {
		t.Errorf("expected '—' on rejected line in --no-bloom mode\n%s", out)
	}
	if !strings.Contains(out, "bloom est. FPR:   —") {
		t.Errorf("expected '—' on FPR line in --no-bloom mode\n%s", out)
	}

	seg, err := storage.OpenSegment(outPath)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()
	if seg.Bloom() != nil {
		t.Errorf("expected nil Bloom in BF-off segment")
	}
	if got := int(seg.RowCount()); got != uniques+10 {
		t.Errorf("RowCount = %d; want %d (no dedup in --no-bloom)", got, uniques+10)
	}
}

func TestRunLoad_BadSchemaSpec(t *testing.T) {
	dir := t.TempDir()
	csvPath, _ := buildEventsCSV(t, dir, 5, 0)
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", filepath.Join(dir, "x.uniq"),
		"--pk", "event_id",
		"--schema", "event_id:wat",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "bad --schema") {
		t.Errorf("stderr missing 'bad --schema': %s", stderr.String())
	}
}

func TestRunLoad_MissingPKInSchema(t *testing.T) {
	dir := t.TempDir()
	csvPath, _ := buildEventsCSV(t, dir, 5, 0)
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", filepath.Join(dir, "x.uniq"),
		"--pk", "nope",
		"--schema", "event_id:int64,user_id:int64,amount:float64,country:string",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "not present in --schema") {
		t.Errorf("stderr missing expected message: %s", stderr.String())
	}
}

func TestRunLoad_RequiredFlagsMissing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{"--csv", "/tmp/x"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
}

func TestRunLoad_SchemaColumnMissingFromHeader(t *testing.T) {
	dir := t.TempDir()
	// Schema asks for 'amount', but CSV header lacks it.
	csvPath := writeCSV(t, filepath.Join(dir, "x.csv"),
		[]string{"event_id", "user_id", "country"},
		[][]string{{"1001", "42", "TR"}})
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", filepath.Join(dir, "x.uniq"),
		"--pk", "event_id",
		"--schema", "event_id:int64,user_id:int64,amount:float64,country:string",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"amount"`) {
		t.Errorf("stderr should name the missing column 'amount': %s", stderr.String())
	}
}

func TestRunLoad_MissingCSVFile(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", filepath.Join(dir, "missing.csv"),
		"--out", filepath.Join(dir, "x.uniq"),
		"--pk", "event_id",
		"--schema", "event_id:int64",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "open csv") {
		t.Errorf("stderr missing 'open csv': %s", stderr.String())
	}
}

func TestRunLoad_RowParseErrorWarnsContinues(t *testing.T) {
	dir := t.TempDir()
	// Row 2 is bad; rows 1, 3, 4 are fine.
	csvPath := writeCSV(t, filepath.Join(dir, "x.csv"),
		[]string{"event_id", "amount"},
		[][]string{
			{"1", "1.0"},
			{"not-an-int", "2.0"},
			{"3", "3.0"},
			{"4", "4.0"},
		})
	outPath := filepath.Join(dir, "x.uniq")
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", outPath,
		"--pk", "event_id",
		"--schema", "event_id:int64,amount:float64",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("per-row parse errors must not fail the run; exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warn: row") {
		t.Errorf("expected a warn line: %s", stderr.String())
	}

	seg, err := storage.OpenSegment(outPath)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()
	if got := seg.RowCount(); got != 3 {
		t.Errorf("RowCount = %d; want 3 (4 rows minus 1 parse error)", got)
	}
}

func TestRunLoad_WarningSuppression(t *testing.T) {
	dir := t.TempDir()
	header := []string{"event_id", "amount"}
	var rows [][]string
	// 25 rows; every other row is malformed → 12 or 13 parse errors.
	for i := range 25 {
		pk := fmt.Sprintf("%d", i)
		if i%2 == 1 {
			pk = "not-int"
		}
		rows = append(rows, []string{pk, "1.0"})
	}
	csvPath := writeCSV(t, filepath.Join(dir, "x.csv"), header, rows)
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", filepath.Join(dir, "x.uniq"),
		"--pk", "event_id",
		"--schema", "event_id:int64,amount:float64",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	se := stderr.String()
	warnCount := strings.Count(se, "warn: row")
	if warnCount != maxLoadWarnings {
		t.Errorf("warn lines = %d; want exactly %d", warnCount, maxLoadWarnings)
	}
	if !strings.Contains(se, "suppressed") {
		t.Errorf("expected 'suppressed' summary line: %s", se)
	}
}

func TestRunLoad_UnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runLoad([]string{"--bogus"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit on unknown flag")
	}
}

// TestRunLoad_SmallExpectedItemsWarning fires the pre-flight warning by
// asking for a too-tight filter. The load itself must still succeed.
// Inputs sized so the post-load saturation warning does NOT also fire,
// which would muddy what this test asserts.
func TestRunLoad_SmallExpectedItemsWarning(t *testing.T) {
	dir := t.TempDir()
	csvPath, _ := buildEventsCSV(t, dir, 5, 0) // tiny CSV, 5 unique rows
	outPath := filepath.Join(dir, "out.uniq")

	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", outPath,
		"--pk", "event_id",
		"--schema", "event_id:int64,user_id:int64,amount:float64,country:string",
		"--expected-items", "100",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("load should still succeed; exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--expected-items=100 is very small") {
		t.Errorf("stderr missing pre-flight warning:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "may saturate") {
		t.Errorf("stderr missing 'may saturate' hint:\n%s", stderr.String())
	}
}

// TestRunLoad_SaturationWarning over-fills a correctly-sized-by-flag
// filter (expected-items=1000 → no pre-flight) with 2000 distinct PKs so
// the actual estimated FPR climbs past 2x the target.
func TestRunLoad_SaturationWarning(t *testing.T) {
	dir := t.TempDir()
	csvPath, _ := buildEventsCSV(t, dir, 2000, 0)
	outPath := filepath.Join(dir, "out.uniq")

	var stdout, stderr bytes.Buffer
	code := runLoad([]string{
		"--csv", csvPath,
		"--out", outPath,
		"--pk", "event_id",
		"--schema", "event_id:int64,user_id:int64,amount:float64,country:string",
		"--expected-items", "1000", // not < 1000, so pre-flight stays quiet
		"--target-fpr", "0.01",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("load should still succeed; exit=%d stderr=%q", code, stderr.String())
	}
	se := stderr.String()
	if strings.Contains(se, "is very small") {
		t.Errorf("pre-flight warning should NOT fire at expected-items=1000:\n%s", se)
	}
	if !strings.Contains(se, "exceeds target") {
		t.Errorf("stderr missing saturation warning:\n%s", se)
	}
	if !strings.Contains(se, "consider raising --expected-items") {
		t.Errorf("stderr missing remediation hint:\n%s", se)
	}
}
