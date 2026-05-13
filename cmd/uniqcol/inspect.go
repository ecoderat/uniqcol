package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/ecoderat/uniqcol/storage"
)

// runInspect prints a human-readable, demo-friendly metadata dump of a
// segment file. Usage: uniqcol inspect <path>. Returns 0 on success, 1
// if the file cannot be opened or parsed.
func runInspect(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "usage: uniqcol inspect <path>")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  Prints segment metadata: version, schema, row count, Bloom filter")
		fmt.Fprintln(stderr, "  parameters, and per-column payload sizes. Read-only.")
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: uniqcol inspect <path>")
		return 1
	}
	path := args[0]
	seg, err := storage.OpenSegment(path)
	if err != nil {
		fmt.Fprintf(stderr, "inspect: %v\n", err)
		return 1
	}
	defer seg.Close()

	info, err := os.Stat(path)
	var diskSize int64
	if err == nil {
		diskSize = info.Size()
	}

	fmt.Fprintf(stdout, "segment: %s\n", path)
	fmt.Fprintf(stdout, "format:  %s\n", formatVersionLabel(seg.Version()))
	fmt.Fprintf(stdout, "size:    %s\n", humanBytes(diskSize))
	fmt.Fprintln(stdout, "")

	schema := seg.Schema()
	fmt.Fprintf(stdout, "schema (%d columns):\n", len(schema.Columns))
	tw := tabwriter.NewWriter(stdout, 0, 0, 4, ' ', 0)
	pk := seg.PKName()
	for _, c := range schema.Columns {
		pkTag := ""
		if pk != "" && c.Name == pk {
			pkTag = "[PK]"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", c.Name, c.Type, pkTag)
	}
	tw.Flush()
	fmt.Fprintln(stdout, "")

	fmt.Fprintf(stdout, "rows:    %s\n", commaSepUint64(seg.RowCount()))
	fmt.Fprintln(stdout, "")

	if seg.Version() == 1 {
		// v1: no PK, no BF block to print. Note already conveyed in format
		// line, so just skip the bloom section entirely.
	} else {
		fmt.Fprintln(stdout, "bloom filter:")
		bf := seg.Bloom()
		if bf == nil {
			fmt.Fprintln(stdout, "  enabled:        no")
		} else {
			fmt.Fprintln(stdout, "  enabled:        yes")
			fmt.Fprintf(stdout, "  m (bits):       %s\n", commaSepUint64(bf.M()))
			fmt.Fprintf(stdout, "  k (hashes):     %d\n", bf.K())
			fmt.Fprintf(stdout, "  n_added:        %s\n", commaSepUint64(bf.NumAdded()))
			fmt.Fprintf(stdout, "  est. FPR:       %.5f\n", bf.EstimatedFPR())
		}
		fmt.Fprintln(stdout, "")
	}

	fmt.Fprintln(stdout, "column payloads:")
	tw = tabwriter.NewWriter(stdout, 0, 0, 4, ' ', 0)
	for _, ci := range seg.ColumnInfo() {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			ci.Name, ci.Type, encodingLabel(ci.Encoding), humanBytes(int64(ci.PayloadLen)))
	}
	tw.Flush()
	return 0
}

func formatVersionLabel(v uint16) string {
	switch v {
	case 1:
		return "v1 (legacy, no PK/no Bloom)"
	case 2:
		return "v2"
	default:
		return fmt.Sprintf("v%d (unsupported)", v)
	}
}

func encodingLabel(e storage.Encoding) string {
	switch e {
	case storage.EncodingRaw:
		return "raw"
	case storage.EncodingRLE:
		return "RLE"
	default:
		return e.String()
	}
}
