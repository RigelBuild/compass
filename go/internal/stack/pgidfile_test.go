//go:build unix

package stack

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPgidFileRoundTrip proves the format survives a write→read cycle including
// the start-time identity column, in start order.
func TestPgidFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := pgidRecord{
		WriterPid: 4242,
		Version:   pgidFileVersion,
		Entries: []pgidEntry{
			{Component: ComponentPostgres, Pgid: 1001, StartTime: 10010},
			{Component: ComponentServer, Pgid: 1002, StartTime: 10020},
			{Component: ComponentRunner, Pgid: 1003, StartTime: 10030},
		},
	}
	if err := writePgidFile(dir, rec); err != nil {
		t.Fatalf("writePgidFile = %v", err)
	}

	got, err := readPgidFile(dir)
	if err != nil {
		t.Fatalf("readPgidFile = %v", err)
	}
	if !reflect.DeepEqual(got, rec) {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, rec)
	}
}

// TestPgidFileMode0600 pins the record file's permissions.
func TestPgidFileMode0600(t *testing.T) {
	dir := t.TempDir()
	if err := writePgidFile(dir, pgidRecord{Version: pgidFileVersion, WriterPid: 1}); err != nil {
		t.Fatalf("writePgidFile = %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, pgidFileName))
	if err != nil {
		t.Fatalf("stat = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("pgid file mode = %o, want 600", perm)
	}
}

// TestPgidFileTrailingNewline pins the plain-text format: a header line plus one
// entry line per child, each newline-terminated.
func TestPgidFileTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	rec := pgidRecord{
		WriterPid: 7,
		Version:   pgidFileVersion,
		Entries: []pgidEntry{
			{Component: ComponentPostgres, Pgid: 200, StartTime: 999},
		},
	}
	if err := writePgidFile(dir, rec); err != nil {
		t.Fatalf("writePgidFile = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, pgidFileName))
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	want := "1 7\npostgres 200 999\n"
	if string(data) != want {
		t.Fatalf("file content = %q, want %q", string(data), want)
	}
}

// TestPgidFilePartialSequenceRewrite models spawnChain's rewrite-after-each-spawn
// discipline: the file is rewritten as 1, then 2, then 3 children, and each read
// observes exactly the prefix started so far — the one-child-wide crash window.
func TestPgidFilePartialSequenceRewrite(t *testing.T) {
	dir := t.TempDir()
	entries := []pgidEntry{
		{Component: ComponentPostgres, Pgid: 1, StartTime: 11},
		{Component: ComponentServer, Pgid: 2, StartTime: 22},
		{Component: ComponentRunner, Pgid: 3, StartTime: 33},
	}
	for n := 1; n <= len(entries); n++ {
		rec := pgidRecord{WriterPid: 1, Version: pgidFileVersion, Entries: entries[:n]}
		if err := writePgidFile(dir, rec); err != nil {
			t.Fatalf("writePgidFile prefix %d = %v", n, err)
		}
		got, err := readPgidFile(dir)
		if err != nil {
			t.Fatalf("readPgidFile prefix %d = %v", n, err)
		}
		if len(got.Entries) != n {
			t.Fatalf("prefix %d: read %d entries, want %d", n, len(got.Entries), n)
		}
		if !reflect.DeepEqual(got.Entries, entries[:n]) {
			t.Fatalf("prefix %d entries:\n got  %+v\n want %+v", n, got.Entries, entries[:n])
		}
	}
}

// TestPgidFileNoLeftoverTemp proves the atomic write leaves no temp file behind
// (the rename consumes it), so a reader never sees a created-but-partial file
// under the state dir — the torn-write-impossibility guarantee.
func TestPgidFileNoLeftoverTemp(t *testing.T) {
	dir := t.TempDir()
	for n := range 3 {
		if err := writePgidFile(dir, pgidRecord{Version: pgidFileVersion, WriterPid: n}); err != nil {
			t.Fatalf("writePgidFile = %v", err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir = %v", err)
	}
	for _, e := range ents {
		if e.Name() != pgidFileName {
			t.Fatalf("unexpected leftover file %q in state dir; only %q should remain", e.Name(), pgidFileName)
		}
	}
}

// TestReadPgidFileAbsent reports a missing file as os.ErrNotExist so the caller
// can branch on "no teardown record" vs a genuine read failure.
func TestReadPgidFileAbsent(t *testing.T) {
	_, err := readPgidFile(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readPgidFile on absent = %v, want os.ErrNotExist", err)
	}
}

// TestReadPgidFileMalformed proves parsing is defensive: a garbled header or
// entry is a hard error, never a silent partial parse that would let down signal
// off a half-understood record.
func TestReadPgidFileMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"header only one col":  "1\n",
		"bad writer pid":       "1 notanumber\n",
		"entry too few fields": "1 7\npostgres 200\n",
		"unknown component":    "1 7\nnot-a-component 200 999\n",
		"bad pgid":             "1 7\npostgres xx 999\n",
		"bad starttime":        "1 7\npostgres 200 zz\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, pgidFileName), []byte(content), 0o600); err != nil {
				t.Fatalf("seed file = %v", err)
			}
			if _, err := readPgidFile(dir); err == nil {
				t.Fatalf("readPgidFile(%q) = nil, want parse error", content)
			}
		})
	}
}

// TestRemovePgidFileIdempotent proves removal succeeds whether or not the file
// exists.
func TestRemovePgidFileIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := removePgidFile(dir); err != nil {
		t.Fatalf("removePgidFile on absent = %v, want nil", err)
	}
	if err := writePgidFile(dir, pgidRecord{Version: pgidFileVersion, WriterPid: 1}); err != nil {
		t.Fatalf("writePgidFile = %v", err)
	}
	if err := removePgidFile(dir); err != nil {
		t.Fatalf("removePgidFile on present = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, pgidFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still present after remove: %v", err)
	}
}

// TestPgidRecordMatches is the identity predicate: a matching pgid+token is
// found; a right pgid with the wrong token (a recycled pid) is NOT.
func TestPgidRecordMatches(t *testing.T) {
	rec := pgidRecord{Entries: []pgidEntry{
		{Component: ComponentServer, Pgid: 42, StartTime: 100},
	}}
	if !rec.matches(42, 100) {
		t.Fatal("matches(42,100) = false, want true (exact pgid+token)")
	}
	if rec.matches(42, 999) {
		t.Fatal("matches(42,999) = true, want false (recycled pid: right pgid, wrong start-time token)")
	}
	if rec.matches(99, 100) {
		t.Fatal("matches(99,100) = true, want false (unknown pgid)")
	}
}

// TestReadStartTimeProcParsesParenthesizedComm is the /proc/<pid>/stat field-22
// parse gotcha: a comm containing spaces AND parentheses must not throw off the
// field count. The reader finds the LAST ')' and counts fields from there, so
// starttime is read correctly regardless of comm contents. This exercises the
// real parser against a synthesized stat line rather than a live process.
func TestReadStartTimeProcParsesParenthesizedComm(t *testing.T) {
	// Fields 1..: pid comm state ppid pgrp session tty_nr tpgid flags minflt
	// cminflt majflt cmajflt utime stime cutime cstime priority nice
	// num_threads itrealvalue starttime(22) ...
	// comm here is "(weird )(name)" — embedded spaces and parens.
	line := "1234 (weird )(name) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 ...\n"
	got, err := parseStatStartTime(line)
	if err != nil {
		t.Fatalf("parseStatStartTime = %v", err)
	}
	if got != 987654 {
		t.Fatalf("starttime = %d, want 987654", got)
	}
}
