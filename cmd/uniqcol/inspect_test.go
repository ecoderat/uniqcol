package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ecoderat/uniqcol/storage"
)

// buildSegment programmatically creates a v2 segment with the given
// options. Useful for inspect tests that don't want to go through CSV.
func buildSegment(t *testing.T, opts storage.TableOptions, rows int) (path string) {
	t.Helper()
	schema := storage.Schema{
		PK: "event_id",
		Columns: []storage.Column{
			{Name: "event_id", Type: storage.Int64},
			{Name: "user_id", Type: storage.Int64},
			{Name: "amount", Type: storage.Float64},
			{Name: "country", Type: storage.String},
		},
	}
	tbl, err := storage.CreateTable(schema, opts)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	countries := []string{"TR", "US", "DE"}
	for i := range rows {
		row := storage.Row{
			int64(1000 + i),
			int64(100 + i),
			float64(i) * 1.25,
			countries[i%len(countries)],
		}
		if r := tbl.Insert(row); !r.Accepted {
			t.Fatalf("setup insert: %s", r.Reason)
		}
	}
	path = filepath.Join(t.TempDir(), "events.uniq")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tbl.Flush(f); err != nil {
		_ = f.Close()
		t.Fatalf("Flush: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func TestRunInspect_V2WithBloom(t *testing.T) {
	path := buildSegment(t, storage.TableOptions{
		BloomExpectedItems: 500,
		BloomTargetFPR:     0.01,
	}, 50)

	var stdout, stderr bytes.Buffer
	code := runInspect([]string{path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	wantSubstrings := []string{
		"format:  v2",
		"schema (4 columns):",
		"event_id", "user_id", "amount", "country",
		"[PK]",
		"rows:    50",
		"bloom filter:",
		"enabled:        yes",
		"m (bits):",
		"k (hashes):",
		"n_added:        50",
		"est. FPR:",
		"column payloads:",
		"RLE",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(out, sub) {
			t.Errorf("output missing %q\n---\n%s", sub, out)
		}
	}
}

func TestRunInspect_V2NoBloom(t *testing.T) {
	path := buildSegment(t, storage.TableOptions{BloomDisabled: true}, 10)

	var stdout, stderr bytes.Buffer
	code := runInspect([]string{path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "enabled:        no") {
		t.Errorf("expected 'enabled: no' for BF-off segment\n%s", out)
	}
	if strings.Contains(out, "m (bits):") {
		t.Errorf("BF-off segment must not print Bloom params\n%s", out)
	}
	if !strings.Contains(out, "format:  v2") {
		t.Errorf("missing v2 marker")
	}
}

// writeV1Segment emits a v1 (legacy) wire image so the inspect path can
// be exercised on a pre-Iteration-2 segment.
func writeV1Segment(t *testing.T, path string) {
	t.Helper()
	// Build buffer + payloads via the storage package's public surface.
	schema := storage.Schema{
		PK: "id",
		Columns: []storage.Column{
			{Name: "id", Type: storage.Int64},
			{Name: "country", Type: storage.String},
		},
	}
	tbl, err := storage.CreateTable(schema, storage.TableOptions{
		BloomExpectedItems: 100, BloomTargetFPR: 0.01,
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	for i := range 5 {
		if r := tbl.Insert(storage.Row{int64(i), "TR"}); !r.Accepted {
			t.Fatalf("setup: %s", r.Reason)
		}
	}
	// Flush a v2 segment, then byte-patch the header to look like v1 and
	// strip the prefix/suffix that v1 wouldn't have. This is brittle but
	// scoped to one test — and uses only public surface to produce the
	// raw column blocks.
	var v2 bytes.Buffer
	if err := tbl.Flush(&v2); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// V2 layout (no bloom because we won't set the flag): header(16) |
	// flagsLen(1)=1 | flags(1) | pkName-block (uvarintLen + bytes) |
	// columns... | UBLM-trailer
	// To synthesize a v1 image we patch version=1 and rewrite the body
	// without flags block & without trailer.
	//
	// Simplest: build v1 ourselves by parsing v2, dropping the extension
	// metadata, and writing back. But that duplicates segment.go internals
	// here. A cleaner approach: peel the trailer off the v2 image and
	// peel the prefix bytes for the flags/PK block.

	data := v2.Bytes()
	// version is at bytes [4:6].
	binary.LittleEndian.PutUint16(data[4:6], 1)
	// Find the bloom trailer (UBLM) and cut it. The trailer is always at the
	// tail of a Bloom-bearing v2 segment.
	if idx := bytes.Index(data, []byte("UBLM")); idx >= 0 {
		data = data[:idx]
	}
	// Now drop the v2 flags block + PK-name block (everything between byte
	// 16 and the first column nameLen). We know: flagsLen uvarint = 1 (1
	// byte), flags = 1 byte, pkName uvarint len + pkName bytes ("id" = 2 +
	// 1 = 3 bytes). So drop bytes [16:16+1+1+1+2] = [16:21].
	cleaned := make([]byte, 0, len(data)-5)
	cleaned = append(cleaned, data[:16]...)
	cleaned = append(cleaned, data[21:]...)
	if err := os.WriteFile(path, cleaned, 0o600); err != nil {
		t.Fatalf("write v1 fixture: %v", err)
	}

	// Sanity-check that the synthesized image is parseable as v1.
	seg, err := storage.OpenSegment(path)
	if err != nil {
		t.Fatalf("v1 fixture is unreadable: %v", err)
	}
	if seg.Version() != 1 {
		t.Fatalf("v1 fixture parsed as version %d", seg.Version())
	}
	seg.Close()
}

func TestRunInspect_V1Legacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.uniq")
	writeV1Segment(t, path)

	var stdout, stderr bytes.Buffer
	code := runInspect([]string{path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "v1 (legacy") {
		t.Errorf("expected v1 legacy marker in output\n%s", out)
	}
	if strings.Contains(out, "bloom filter:") {
		t.Errorf("v1 inspect must not print the Bloom block\n%s", out)
	}
	if strings.Contains(out, "[PK]") {
		t.Errorf("v1 inspect must not tag a PK column\n%s", out)
	}
}

func TestRunInspect_MissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInspect([]string{filepath.Join(t.TempDir(), "nope.uniq")}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "inspect:") {
		t.Errorf("stderr should be prefixed with 'inspect:': %s", stderr.String())
	}
}

func TestRunInspect_BadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.uniq")
	if err := os.WriteFile(path, []byte("not a segment"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runInspect([]string{path}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
}

func TestRunInspect_WrongArgCount(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInspect([]string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("zero args exit=%d; want 1", code)
	}
	stderr.Reset()
	code = runInspect([]string{"a", "b"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("two args exit=%d; want 1", code)
	}
}

func TestRunInspect_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInspect([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help exit=%d; want 0", code)
	}
	if !strings.Contains(stderr.String(), "usage: uniqcol inspect") {
		t.Errorf("--help missing usage line: %s", stderr.String())
	}
}

func TestDispatch(t *testing.T) {
	t.Run("no args", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := dispatch(nil, &stdout, &stderr); got != 2 {
			t.Errorf("exit=%d; want 2", got)
		}
	})
	t.Run("version", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"version"}, &stdout, &stderr); got != 0 {
			t.Errorf("exit=%d; want 0", got)
		}
		if !strings.Contains(stdout.String(), "uniqcol") {
			t.Errorf("version output missing 'uniqcol': %s", stdout.String())
		}
	})
	t.Run("help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"--help"}, &stdout, &stderr); got != 0 {
			t.Errorf("exit=%d; want 0", got)
		}
		if !strings.Contains(stdout.String(), "subcommands:") {
			t.Errorf("help missing 'subcommands:': %s", stdout.String())
		}
	})
	t.Run("unknown", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"bogus"}, &stdout, &stderr); got != 2 {
			t.Errorf("exit=%d; want 2", got)
		}
		if !strings.Contains(stderr.String(), `unknown subcommand: "bogus"`) {
			t.Errorf("stderr missing expected message: %s", stderr.String())
		}
	})
	t.Run("load delegates", func(t *testing.T) {
		// Just confirm dispatch -> runLoad with bad args returns 1.
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"load"}, &stdout, &stderr); got == 0 {
			t.Errorf("expected non-zero exit when load called with no flags")
		}
	})
	t.Run("inspect delegates", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := dispatch([]string{"inspect"}, &stdout, &stderr); got != 1 {
			t.Errorf("exit=%d; want 1", got)
		}
	})
}
