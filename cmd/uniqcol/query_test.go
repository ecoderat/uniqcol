package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ecoderat/uniqcol/storage"
)

// buildQueryFixture writes a small 6-row segment with mixed countries.
func buildQueryFixture(t *testing.T) string {
	t.Helper()
	schema := storage.Schema{
		PK: "id",
		Columns: []storage.Column{
			{Name: "id", Type: storage.Int64},
			{Name: "amount", Type: storage.Float64},
			{Name: "country", Type: storage.String},
		},
	}
	tbl, err := storage.CreateTable(schema, storage.TableOptions{
		BloomExpectedItems: 100, BloomTargetFPR: 0.01,
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	rows := []storage.Row{
		{int64(1), 10.0, "TR"},
		{int64(2), 20.0, "US"},
		{int64(3), 30.0, "TR"},
		{int64(4), 40.0, "DE"},
		{int64(5), 50.0, "TR"},
		{int64(6), 60.0, "US"},
	}
	for _, r := range rows {
		if got := tbl.Insert(r); !got.Accepted {
			t.Fatalf("setup: %s", got.Reason)
		}
	}
	path := filepath.Join(t.TempDir(), "q.uniq")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tbl.Flush(f); err != nil {
		_ = f.Close()
		t.Fatalf("flush: %v", err)
	}
	_ = f.Close()
	return path
}

func TestRunQuery_SelectProjection(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "SELECT id, country WHERE country = 'TR'"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"id", "country", "TR"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n%s", want, out)
		}
	}
	// Three TR rows in fixture; no truncation since under default limit.
	if c := strings.Count(out, "TR"); c < 3 {
		t.Errorf("TR appears %d times; want >= 3 (header + 3 rows = 4)\n%s", c, out)
	}
	// Wall-time line goes to stderr.
	if !strings.Contains(stderr.String(), "query:") {
		t.Errorf("stderr missing wall-time line: %s", stderr.String())
	}
}

func TestRunQuery_CountStar(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "SELECT COUNT(*) WHERE country = 'TR'"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "COUNT(*)") {
		t.Errorf("missing COUNT(*) header: %s", out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("count should be 3 (TR rows): %s", out)
	}
	// No truncation footer for aggregates.
	if strings.Contains(out, "showing") {
		t.Errorf("aggregate result should not have truncation footer: %s", out)
	}
}

func TestRunQuery_SumAmount(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "SELECT SUM(amount)"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "SUM(amount)") {
		t.Errorf("missing SUM(amount) header: %s", stdout.String())
	}
	// 10+20+30+40+50+60 = 210
	if !strings.Contains(stdout.String(), "210") {
		t.Errorf("expected sum 210 in output: %s", stdout.String())
	}
}

func TestRunQuery_FormatCSV(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "--format", "csv", "SELECT id, country"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	r := csv.NewReader(strings.NewReader(stdout.String()))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v\n%s", err, stdout.String())
	}
	if len(records) != 7 { // header + 6 rows
		t.Errorf("got %d records; want 7", len(records))
	}
	if records[0][0] != "id" || records[0][1] != "country" {
		t.Errorf("bad header: %v", records[0])
	}
}

func TestRunQuery_Limit(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "--limit", "2", "SELECT id, country"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "(showing 2 of 6 rows") {
		t.Errorf("missing truncation footer: %s", out)
	}
}

func TestRunQuery_LimitZero_ShowsAll(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "--limit", "0", "SELECT id"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(stdout.String(), "showing") {
		t.Errorf("--limit 0 should not show truncation: %s", stdout.String())
	}
}

func TestRunQuery_ParseError(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "BOGUS"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "expected SELECT") {
		t.Errorf("stderr missing parse error: %s", stderr.String())
	}
}

func TestRunQuery_ExecError(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	// Filter literal type doesn't match column type.
	code := runQuery([]string{"--db", path, "SELECT id WHERE amount = 50"},
		&stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "type mismatch") {
		t.Errorf("stderr missing type-mismatch: %s", stderr.String())
	}
}

func TestRunQuery_MissingDB(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"SELECT id"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "--db is required") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestRunQuery_BadSegment(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "junk.uniq")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", bad, "SELECT id"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stderr.String(), "open segment") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestRunQuery_MissingSQL(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stderr.String(), "exactly one SQL string") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestRunQuery_BadFormat(t *testing.T) {
	path := buildQueryFixture(t)
	var stdout, stderr bytes.Buffer
	code := runQuery([]string{"--db", path, "--format", "json", "SELECT id"},
		&stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stderr.String(), "unknown --format") {
		t.Errorf("stderr: %s", stderr.String())
	}
}
