// Command uniqcol is the CLI front-end for the uniqcol columnar
// storage engine. Subcommands:
//
//	load     — ingest a CSV into a segment file
//	inspect  — dump segment metadata (schema, Bloom filter, columns)
//	version  — print the version banner
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch routes the first positional argument to its subcommand
// runner. Exposed for testing; main() is a thin os.Exit wrapper.
func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "load":
		return runLoad(rest, stdout, stderr)
	case "inspect":
		return runInspect(rest, stdout, stderr)
	case "query":
		return runQuery(rest, stdout, stderr)
	case "version":
		return runVersion(stdout)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand: %q\n\n", cmd)
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: uniqcol <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "subcommands:")
	fmt.Fprintln(w, "  load     ingest a CSV into a segment file")
	fmt.Fprintln(w, "  inspect  print segment metadata")
	fmt.Fprintln(w, "  query    run a SELECT/WHERE/COUNT/SUM query")
	fmt.Fprintln(w, "  version  print version")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "use `uniqcol <subcommand> --help` for subcommand-specific flags")
}
