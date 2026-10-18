//go:build unix

package stack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pgidFileName stores child teardown identities for a fresh down, which has no in-memory process handles.
// Successful teardown removes the record.
const pgidFileName = "stack.pgids"

// pgidFileVersion identifies the record format, not child liveness.
// V2 tags process and container entries; v1 entries remain readable as processes.
// Older v1 binaries reject v2 entries by grammar; this reader rejects future versions.
const pgidFileVersion = "2"

// pgidFileVersionV1 is the prior format this build still reads for back-compat:
// untagged 3-field proc lines (no kind tag), parsed as all-process entries.
const pgidFileVersionV1 = "1"

// pgidEntryKind tags an entry line's grammar in the v2 discriminated union.
type pgidEntryKind int

const (
	// entryProc is a process-group child, torn down by group signal
	// (identity-checked pgid + leader start time). It is the zero value, so an
	// unqualified pgidEntry literal is a process entry — the v1 shape unchanged.
	entryProc pgidEntryKind = iota
	// entryContainer is a container child (S4's containerized postgres), torn
	// down by name via podman (stop, then rm -f). A container has no process
	// group of its own under rootless podman (it runs beneath conmon), so its
	// teardown identity is its stable per-state-dir name, not a pgid.
	entryContainer
)

// entryKindTag renders a kind as its on-disk line tag.
func (k pgidEntryKind) entryKindTag() string {
	switch k {
	case entryContainer:
		return "ctr"
	default:
		return "proc"
	}
}

// pgidEntry is a kind-tagged teardown identity: process groups use pgid plus start time; containers use their stable name.
type pgidEntry struct {
	Kind          pgidEntryKind
	Component     Component
	Pgid          int
	StartTime     uint64
	ContainerName string
}

// pgidRecord holds provenance and child entries in start order.
// WriterPid and Version never indicate liveness; only entry identity and prior-boot handling do.
type pgidRecord struct {
	WriterPid int
	Version   string
	// BootID is the writer's boot identity; empty means unknown (a pre-boot-id record).
	BootID  string
	Entries []pgidEntry
}

// writePgidFile atomically publishes a 0600 text record: a version/writer/boot header,
// then tagged proc or ctr entries. Spawn updates are complete prefixes; partial teardown
// rewrites contain only survivors. V1 entries read as untagged process records;
// unknown boot IDs disable only the reboot shortcut.
func writePgidFile(ctx context.Context, stateDir string, rec pgidRecord) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d", pgidFileVersion, rec.WriterPid)
	if boot, err := readBootID(); err != nil || !isBootUUID(boot) {
		// An unknown boot only forgoes the reboot shortcut; identity checks still guard every signal.
		slog.WarnContext(ctx, "pgid record written without a boot id", "boot_id", boot, "err", err)
	} else {
		fmt.Fprintf(&b, " %s", boot)
	}
	b.WriteString("\n")
	for _, e := range rec.Entries {
		switch e.Kind {
		case entryContainer:
			fmt.Fprintf(&b, "%s %s %s\n", e.Kind.entryKindTag(), e.Component, e.ContainerName)
		default:
			fmt.Fprintf(&b, "%s %s %d %d\n", e.Kind.entryKindTag(), e.Component, e.Pgid, e.StartTime)
		}
	}

	path := filepath.Join(stateDir, pgidFileName)
	tmp, err := os.CreateTemp(stateDir, pgidFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create pgid temp in %q: %w", stateDir, err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()        // best-effort cleanup; the chmod error is the actionable failure
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("chmod pgid temp %q: %w", tmpName, err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()        // best-effort cleanup; the write error is the actionable failure
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("write pgid temp %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("close pgid temp %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup: the rename failed, so the temp still exists
		return fmt.Errorf("publish pgid file %q: %w", path, err)
	}
	return nil
}

// readPgidFile rejects malformed or incomplete records instead of risking teardown from partial identities.
// It reads v2 tagged entries and v1 process entries, and refuses unknown versions.
func readPgidFile(stateDir string) (pgidRecord, error) {
	path := filepath.Join(stateDir, pgidFileName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the stack-owned pgid file in the state dir, not user input
	if err != nil {
		return pgidRecord{}, err
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return pgidRecord{}, fmt.Errorf("pgid file %q: empty or missing header", path)
	}

	header := strings.Fields(lines[0])
	if len(header) != 2 && len(header) != 3 {
		return pgidRecord{}, fmt.Errorf("pgid file %q: malformed header %q", path, lines[0])
	}
	version := header[0]
	if version != pgidFileVersion && version != pgidFileVersionV1 {
		return pgidRecord{}, fmt.Errorf("pgid file %q: unsupported record version %q (this build reads %q and %q); stop the stack with the build that started it", path, version, pgidFileVersionV1, pgidFileVersion)
	}
	// v1 predates the boot id, so a third column there is a malformed header.
	if len(header) == 3 && version == pgidFileVersionV1 {
		return pgidRecord{}, fmt.Errorf("pgid file %q: malformed v1 header %q", path, lines[0])
	}
	writerPid, err := strconv.Atoi(header[1])
	if err != nil {
		return pgidRecord{}, fmt.Errorf("pgid file %q: unparseable writer pid %q: %w", path, header[1], err)
	}
	rec := pgidRecord{WriterPid: writerPid, Version: version}
	if len(header) == 3 {
		if !isBootUUID(header[2]) {
			return pgidRecord{}, fmt.Errorf("pgid file %q: malformed boot id in header %q", path, lines[0])
		}
		rec.BootID = header[2]
	}

	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		entry, err := parsePgidLine(version, line)
		if err != nil {
			return pgidRecord{}, fmt.Errorf("pgid file %q: %w", path, err)
		}
		rec.Entries = append(rec.Entries, entry)
	}
	return rec, nil
}

// isBootUUID reports whether s has the 8-4-4-4-12 hex UUID shape, in either case.
func isBootUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// parsePgidLine dispatches v1 process entries and v2 tagged process or container entries.
// Unknown kinds or mismatched grammar are hard errors, including for older readers of v2 records.
func parsePgidLine(version, line string) (pgidEntry, error) {
	if version == pgidFileVersionV1 {
		// v1: the whole line is an untagged proc body.
		return parseProcEntry(line, strings.Fields(line))
	}

	// v2: the leading token is the kind tag.
	f := strings.Fields(line)
	if len(f) == 0 {
		return pgidEntry{}, fmt.Errorf("malformed entry line %q", line)
	}
	switch f[0] {
	case entryProc.entryKindTag():
		return parseProcEntry(line, f[1:])
	case entryContainer.entryKindTag():
		return parseContainerEntry(line, f[1:])
	default:
		return pgidEntry{}, fmt.Errorf("unknown entry kind %q in entry line %q", f[0], line)
	}
}

// parseProcEntry parses a process entry's "<component> <pgid> <starttime>" body
// (the fields after the kind tag, or the whole v1 line). It is the identity-and-
// safety-checked path: an unknown component, an unparseable or degenerate pgid,
// or an unparseable start time is a hard error.
func parseProcEntry(line string, f []string) (pgidEntry, error) {
	if len(f) != 3 {
		return pgidEntry{}, fmt.Errorf("malformed proc entry line %q", line)
	}
	comp, ok := componentFromString(f[0])
	if !ok {
		return pgidEntry{}, fmt.Errorf("unknown component %q in entry line %q", f[0], line)
	}
	pgid, err := strconv.Atoi(f[1])
	if err != nil {
		return pgidEntry{}, fmt.Errorf("unparseable pgid %q in entry line %q: %w", f[1], line, err)
	}
	// A process-group leader must have pid > 1; refusing smaller values prevents kill(-1) or kill(0) blast radius.
	if pgid <= 1 {
		return pgidEntry{}, fmt.Errorf("invalid pgid %d in entry line %q: a process-group leader pid is always > 1", pgid, line)
	}
	startTime, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return pgidEntry{}, fmt.Errorf("unparseable start time %q in entry line %q: %w", f[2], line, err)
	}
	return pgidEntry{Kind: entryProc, Component: comp, Pgid: pgid, StartTime: startTime}, nil
}

// parseContainerEntry parses a container entry's "<component> <name>" body (the
// fields after the ctr kind tag). An unknown component or a missing/empty name
// is a hard error — the name is the container's whole teardown identity, so an
// unusable one must not reach the podman sink.
func parseContainerEntry(line string, f []string) (pgidEntry, error) {
	if len(f) != 2 {
		return pgidEntry{}, fmt.Errorf("malformed ctr entry line %q", line)
	}
	comp, ok := componentFromString(f[0])
	if !ok {
		return pgidEntry{}, fmt.Errorf("unknown component %q in entry line %q", f[0], line)
	}
	name := f[1]
	if name == "" {
		return pgidEntry{}, fmt.Errorf("empty container name in entry line %q", line)
	}
	return pgidEntry{Kind: entryContainer, Component: comp, ContainerName: name}, nil
}

// componentFromString is the inverse of Component.String for every supervised
// child. It is defined here beside the parser (its only consumer)
// rather than on Component, keeping the pgid file format self-contained.
func componentFromString(s string) (Component, bool) {
	switch s {
	case ComponentPostgres.String():
		return ComponentPostgres, true
	case ComponentServer.String():
		return ComponentServer, true
	case ComponentRunner.String():
		return ComponentRunner, true
	case ComponentCollector.String():
		return ComponentCollector, true
	case ComponentNats.String():
		return ComponentNats, true
	case ComponentGateway.String():
		return ComponentGateway, true
	default:
		return 0, false
	}
}

// removePgidFile is idempotent so successful teardown and retries can call it unconditionally.
func removePgidFile(stateDir string) error {
	path := filepath.Join(stateDir, pgidFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pgid file %q: %w", path, err)
	}
	return nil
}

// readStartTime is a replaceable test seam with platform-specific identity encodings.
// Spawn and teardown readers must pack the same token or live children look recycled.
var readStartTime = readProcessStartTime

// readBootID is the boot-identity seam: a var so tests can model a reboot.
var readBootID = readCurrentBootID

// parseStatStartTime reads field 22 after the final ')' because comm may contain spaces and parentheses.
func parseStatStartTime(line string) (uint64, error) {
	rparen := strings.LastIndexByte(line, ')')
	if rparen < 0 {
		return 0, fmt.Errorf("no comm terminator ')' in %q", line)
	}
	// Fields after comm, space-separated. field[0] == field 3 (state).
	rest := strings.Fields(line[rparen+1:])
	const startTimeIndexAfterComm = 22 - 3 // field 22 (starttime); field 3 is the first post-comm field
	if len(rest) <= startTimeIndexAfterComm {
		return 0, fmt.Errorf("only %d fields after comm, need field 22", len(rest))
	}
	startTime, err := strconv.ParseUint(rest[startTimeIndexAfterComm], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable starttime %q: %w", rest[startTimeIndexAfterComm], err)
	}
	return startTime, nil
}
