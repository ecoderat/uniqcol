package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ecoderat/uniqcol/storage"
)

// maxLoadWarnings caps per-row parse-error lines on stderr. Subsequent
// errors are counted and summarized at the end.
const maxLoadWarnings = 10

type colSpec struct {
	name string
	typ  storage.ColumnType
}

// parseSchemaSpec parses "col1:type1,col2:type2,...". Types are
// lowercase int64/float64/string. Returns the column specs in order.
func parseSchemaSpec(spec string) ([]colSpec, error) {
	if spec == "" {
		return nil, errors.New("empty schema")
	}
	parts := strings.Split(spec, ",")
	out := make([]colSpec, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		nameType := strings.SplitN(p, ":", 2)
		if len(nameType) != 2 {
			return nil, fmt.Errorf("schema item %d (%q): expected name:type", i+1, p)
		}
		name := strings.TrimSpace(nameType[0])
		typ := strings.TrimSpace(nameType[1])
		if name == "" {
			return nil, fmt.Errorf("schema item %d: empty column name", i+1)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("schema item %d: duplicate column name %q", i+1, name)
		}
		seen[name] = struct{}{}
		var ct storage.ColumnType
		switch typ {
		case "int64":
			ct = storage.Int64
		case "float64":
			ct = storage.Float64
		case "string":
			ct = storage.String
		default:
			return nil, fmt.Errorf("schema item %d (%q): unknown type %q (want int64|float64|string)", i+1, name, typ)
		}
		out = append(out, colSpec{name: name, typ: ct})
	}
	return out, nil
}

// parseFieldByType converts a raw CSV field into the value expected by
// the column's declared type. An empty string for int64/float64 is a
// parse error; for string it is the empty string (a valid value).
func parseFieldByType(field string, typ storage.ColumnType) (any, error) {
	switch typ {
	case storage.Int64:
		if field == "" {
			return nil, errors.New("empty value for int64")
		}
		v, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case storage.Float64:
		if field == "" {
			return nil, errors.New("empty value for float64")
		}
		v, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case storage.String:
		return field, nil
	default:
		return nil, fmt.Errorf("unsupported type %s", typ)
	}
}

// runLoad parses args, ingests a CSV into a v2 segment, and prints a
// human-readable summary. Returns 0 on success, 1 on schema/CSV-level
// errors. Per-row parse errors are warnings; they do not change the
// exit code.
func runLoad(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		csvPath  = fs.String("csv", "", "path to input CSV file (required)")
		outPath  = fs.String("out", "", "path to output segment file (required)")
		pkName   = fs.String("pk", "", "primary-key column name (must appear in --schema) (required)")
		schema   = fs.String("schema", "", "column schema as col:type,col:type,... using int64|float64|string (required)")
		expItems = fs.Uint64("expected-items", 1_000_000, "Bloom filter sizing: expected number of unique items")
		fpr      = fs.Float64("target-fpr", 0.01, "Bloom filter target false-positive rate in (0,1)")
		noBloom  = fs.Bool("no-bloom", false, "disable the Bloom filter; segment is written without a trailer (write/benchmark only)")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: uniqcol load --csv <path> --out <path> --pk <col> --schema <spec> [flags]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  Ingests a CSV into a v2 segment. CSV columns not named in --schema are")
		fmt.Fprintln(stderr, "  silently dropped; missing schema columns fail loudly. Per-row parse")
		fmt.Fprintln(stderr, "  errors are warnings, not failures.")
		fmt.Fprintln(stderr, "")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *csvPath == "" || *outPath == "" || *pkName == "" || *schema == "" {
		fmt.Fprintln(stderr, "load: --csv, --out, --pk and --schema are required")
		fs.Usage()
		return 1
	}

	cols, err := parseSchemaSpec(*schema)
	if err != nil {
		fmt.Fprintf(stderr, "load: bad --schema: %v\n", err)
		return 1
	}

	// Build storage.Schema; validate PK names a column.
	storageCols := make([]storage.Column, len(cols))
	pkFound := false
	for i, c := range cols {
		storageCols[i] = storage.Column{Name: c.name, Type: c.typ}
		if c.name == *pkName {
			pkFound = true
		}
	}
	if !pkFound {
		fmt.Fprintf(stderr, "load: --pk %q not present in --schema\n", *pkName)
		return 1
	}
	sch := storage.Schema{PK: *pkName, Columns: storageCols}

	tbl, err := storage.CreateTable(sch, storage.TableOptions{
		BloomExpectedItems: *expItems,
		BloomTargetFPR:     *fpr,
		BloomDisabled:      *noBloom,
	})
	if err != nil {
		fmt.Fprintf(stderr, "load: create table: %v\n", err)
		return 1
	}

	f, err := os.Open(*csvPath)
	if err != nil {
		fmt.Fprintf(stderr, "load: open csv: %v\n", err)
		return 1
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // allow rows with varying field counts; we'll catch mismatches per row

	header, err := r.Read()
	if err != nil {
		fmt.Fprintf(stderr, "load: read csv header: %v\n", err)
		return 1
	}
	csvIdx := make([]int, len(cols))
	headerIdx := make(map[string]int, len(header))
	for i, h := range header {
		headerIdx[strings.TrimSpace(h)] = i
	}
	for i, c := range cols {
		j, ok := headerIdx[c.name]
		if !ok {
			fmt.Fprintf(stderr, "load: schema column %q is missing from CSV header\n", c.name)
			return 1
		}
		csvIdx[i] = j
	}

	var (
		rowsRead     uint64
		parseErrors  uint64
		warningsEmit int
		start        = time.Now()
	)
	row := make(storage.Row, len(cols))
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		rowsRead++
		rowNum := rowsRead + 1 // +1 because the header was row 1
		if err != nil {
			parseErrors++
			emitWarning(stderr, &warningsEmit, "row %d: csv read error: %v", rowNum, err)
			continue
		}
		// Reorder + parse each schema column.
		badRow := false
		for i, c := range cols {
			if csvIdx[i] >= len(rec) {
				parseErrors++
				emitWarning(stderr, &warningsEmit, "row %d: column %q: missing field (CSV has %d fields, want index %d)",
					rowNum, c.name, len(rec), csvIdx[i])
				badRow = true
				break
			}
			v, perr := parseFieldByType(rec[csvIdx[i]], c.typ)
			if perr != nil {
				parseErrors++
				emitWarning(stderr, &warningsEmit, "row %d: column %q: %v", rowNum, c.name, perr)
				badRow = true
				break
			}
			row[i] = v
		}
		if badRow {
			continue
		}
		_ = tbl.Insert(row)
	}

	elapsed := time.Since(start)
	if parseErrors > uint64(warningsEmit) {
		fmt.Fprintf(stderr, "... and %d more parse errors suppressed\n",
			parseErrors-uint64(warningsEmit))
	}

	outFile, err := os.Create(*outPath)
	if err != nil {
		fmt.Fprintf(stderr, "load: create output: %v\n", err)
		return 1
	}
	if err := tbl.Flush(outFile); err != nil {
		_ = outFile.Close()
		fmt.Fprintf(stderr, "load: flush: %v\n", err)
		return 1
	}
	if err := outFile.Close(); err != nil {
		fmt.Fprintf(stderr, "load: close output: %v\n", err)
		return 1
	}
	info, statErr := os.Stat(*outPath)
	var segSize int64
	if statErr == nil {
		segSize = info.Size()
	}

	stats := tbl.Stats()
	printLoadSummary(stdout, loadSummary{
		rowsRead:      rowsRead,
		accepted:      stats.Accepted,
		rejected:      stats.Rejected,
		parseErrors:   parseErrors,
		elapsed:       elapsed,
		bloomDisabled: *noBloom,
		estimatedFPR:  estimatedFPR(tbl),
		segmentBytes:  segSize,
	})
	return 0
}

// estimatedFPR returns the bloom filter's current FPR estimate, or 0
// when the table has no filter (BF-off mode).
func estimatedFPR(tbl *storage.Table) float64 {
	if bf := tbl.Bloom(); bf != nil {
		return bf.EstimatedFPR()
	}
	return 0
}

func emitWarning(stderr io.Writer, count *int, format string, args ...any) {
	if *count >= maxLoadWarnings {
		return
	}
	fmt.Fprintf(stderr, "warn: "+format+"\n", args...)
	*count++
}

type loadSummary struct {
	rowsRead      uint64
	accepted      uint64
	rejected      uint64
	parseErrors   uint64
	elapsed       time.Duration
	bloomDisabled bool
	estimatedFPR  float64
	segmentBytes  int64
}

func printLoadSummary(stdout io.Writer, s loadSummary) {
	rejectedField := commaSepUint64(s.rejected)
	rejectedAnnotation := ""
	if !s.bloomDisabled && s.rowsRead > 0 {
		pct := 100 * float64(s.rejected) / float64(s.rowsRead)
		rejectedAnnotation = fmt.Sprintf("   (%.2f%% — probably duplicate)", pct)
	}
	throughput := "—"
	if s.elapsed > 0 && s.rowsRead > 0 {
		rps := float64(s.rowsRead) / s.elapsed.Seconds()
		throughput = commaSepInt64(int64(rps)) + " rows/sec"
	}

	bfRejectedLine := rejectedField + rejectedAnnotation
	bfFPRLine := fmt.Sprintf("%.5f", s.estimatedFPR)
	if s.bloomDisabled {
		bfRejectedLine = "—"
		bfFPRLine = "—"
	}

	fmt.Fprintf(stdout, "rows read:        %s\n", commaSepUint64(s.rowsRead))
	fmt.Fprintf(stdout, "accepted:         %s\n", commaSepUint64(s.accepted))
	fmt.Fprintf(stdout, "rejected (BF):    %s\n", bfRejectedLine)
	fmt.Fprintf(stdout, "parse errors:     %s\n", commaSepUint64(s.parseErrors))
	fmt.Fprintf(stdout, "wall time:        %s\n", s.elapsed.Round(time.Millisecond))
	fmt.Fprintf(stdout, "throughput:       %s\n", throughput)
	fmt.Fprintf(stdout, "bloom est. FPR:   %s\n", bfFPRLine)
	fmt.Fprintf(stdout, "segment size:     %s\n", humanBytes(s.segmentBytes))
}
