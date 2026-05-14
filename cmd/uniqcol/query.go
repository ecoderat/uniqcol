package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/ecoderat/uniqcol/query"
	"github.com/ecoderat/uniqcol/storage"
)

// runQuery executes a SELECT statement against a segment file.
// Returns 0 on success, 1 on segment open / parse / execution errors.
func runQuery(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dbPath = fs.String("db", "", "path to the segment file (required)")
		limit  = fs.Int("limit", 100, "max rows printed for projections; 0 = unlimited; ignored for aggregates")
		format = fs.String("format", "table", "output format: table | csv")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: uniqcol query --db <segment> [--limit N] [--format table|csv] \"<sql>\"")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  Supported SQL (FROM is implicit; segment comes from --db):")
		fmt.Fprintln(stderr, "    SELECT <cols | * | COUNT(*) | SUM(col) | <col>, <AGG>>")
		fmt.Fprintln(stderr, "      [WHERE <cond> [(AND | OR) <cond>]* ]")
		fmt.Fprintln(stderr, "      [GROUP BY <col>]")
		fmt.Fprintln(stderr, "    <cond>:   <col> <op> <literal>")
		fmt.Fprintln(stderr, "    Ops:      = != < > <= >=")
		fmt.Fprintln(stderr, "    Literals: int (42), float (3.14), or single-quoted string ('TR')")
		fmt.Fprintln(stderr, "    AND binds tighter than OR. Parentheses, NOT, IN, LIKE, BETWEEN,")
		fmt.Fprintln(stderr, "    column-to-column comparison, HAVING, ORDER BY, and multi-column")
		fmt.Fprintln(stderr, "    GROUP BY are NOT supported.")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  Examples:")
		fmt.Fprintln(stderr, "    uniqcol query --db data/events.uniq 'SELECT amount WHERE country = '\\''TR'\\'''")
		fmt.Fprintln(stderr, "    uniqcol query --db data/events.uniq 'SELECT id WHERE country = '\\''TR'\\'' AND amount > 50.0'")
		fmt.Fprintln(stderr, "    uniqcol query --db data/events.uniq 'SELECT COUNT(*) WHERE country = '\\''TR'\\'' OR country = '\\''US'\\'''")
		fmt.Fprintln(stderr, "    uniqcol query --db data/events.uniq 'SELECT SUM(amount) WHERE amount > 100.0'")
		fmt.Fprintln(stderr, "    uniqcol query --db data/events.uniq 'SELECT country, COUNT(*) GROUP BY country'")
		fmt.Fprintln(stderr, "    uniqcol query --db data/events.uniq 'SELECT country, SUM(amount) WHERE amount > 10.0 GROUP BY country'")
		fmt.Fprintln(stderr, "")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(stderr, "query: --db is required")
		fs.Usage()
		return 1
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "query: expected exactly one SQL string argument")
		fs.Usage()
		return 1
	}
	sql := fs.Arg(0)
	if *format != "table" && *format != "csv" {
		fmt.Fprintf(stderr, "query: unknown --format %q (want table or csv)\n", *format)
		return 1
	}

	seg, err := storage.OpenSegment(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "query: open segment: %v\n", err)
		return 1
	}
	defer seg.Close()

	q, err := query.Parse(sql)
	if err != nil {
		fmt.Fprintf(stderr, "query: %v\n", err)
		return 1
	}

	start := time.Now()
	result, err := query.Execute(seg, q)
	if err != nil {
		fmt.Fprintf(stderr, "query: %v\n", err)
		return 1
	}
	elapsed := time.Since(start)

	switch *format {
	case "csv":
		if err := renderCSV(stdout, result); err != nil {
			fmt.Fprintf(stderr, "query: render csv: %v\n", err)
			return 1
		}
	default:
		renderTable(stdout, result, *limit)
	}
	fmt.Fprintf(stderr, "query: %s\n", elapsed.Round(10*time.Microsecond))
	return 0
}

// renderTable prints a tabwriter-aligned grid. For projections, applies
// the row limit and prints a footer showing what was truncated.
func renderTable(w io.Writer, r *query.Result, limit int) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for i, c := range r.Columns {
		if i > 0 {
			fmt.Fprint(tw, "\t")
		}
		fmt.Fprint(tw, c)
	}
	fmt.Fprintln(tw)

	totalRows := len(r.Rows)
	shown := totalRows
	if !r.IsAggregate() && limit > 0 && totalRows > limit {
		shown = limit
	}
	for i := 0; i < shown; i++ {
		row := r.Rows[i]
		for j, v := range row {
			if j > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprint(tw, formatValue(v))
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()

	if !r.IsAggregate() && shown < totalRows {
		fmt.Fprintf(w, "(showing %s of %s rows; pass --limit 0 for all)\n",
			commaSepInt64(int64(shown)), commaSepInt64(int64(totalRows)))
	}
}

func renderCSV(w io.Writer, r *query.Result) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(r.Columns); err != nil {
		return err
	}
	rec := make([]string, len(r.Columns))
	for _, row := range r.Rows {
		for j, v := range row {
			rec[j] = formatValue(v)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// formatValue renders any of int64, uint64, float64, string for output.
// uint64 covers the COUNT(*) result; everything else maps cleanly.
func formatValue(v any) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", v)
	}
}
