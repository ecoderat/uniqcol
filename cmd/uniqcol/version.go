package main

import (
	"fmt"
	"io"
)

// versionString is the build tag printed by `uniqcol version`.
const versionString = "uniqcol 0.1.0-iter3a"

// runVersion prints the version banner and returns 0.
func runVersion(stdout io.Writer) int {
	fmt.Fprintln(stdout, versionString)
	return 0
}
