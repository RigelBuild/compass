//go:build unix

package microvm

// The subuid(5)/subgid(5) reads behind virtiofsd's id mapping. virtiofsd shells
// out to newuidmap(1)/newgidmap(1) for a non-trivial --uid-map/--gid-map, and
// those setuid helpers VALIDATE the requested host range against subuid(5) /
// subgid(5) respectively ("the range of subordinate user IDs must have been set
// up via subuid(5)", virtiofsd README §--uid-map). So each base mapped to a
// namespace id 0 is a per-host allocation that must be READ, not assumed:
// shadow-utils happens to allocate 100000 to the first user, but a second user
// on the same box gets 165536, and LDAP/AD-backed or container-image-provisioned
// hosts routinely differ. Assuming it fails in the worst way — the map helper
// refuses, virtiofsd dies, and the boot surfaces as an opaque "waiting for
// daemon sockets" timeout (or worse: virtiofsd binds its socket BEFORE the
// id-map step, so a mapping failure can leave a live-looking socket behind a
// dead daemon — see waitForSockets' liveness check) with the real cause only in
// virtiofsd.log.
//
// Split into a pure parse over an io.Reader plus a thin file-reading wrapper so
// the argv tests can pin the exact mapping against fixed bases with no
// dependence on the test box's own /etc/subuid or /etc/subgid.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// The two subordinate-id maps the id-mapping depends on. They are SEPARATE,
// INDEPENDENT allocations and each is read on its own axis: newuidmap validates
// the --uid-map host range against /etc/subuid, newgidmap validates the
// --gid-map host range against /etc/subgid. shadow-utils' useradd default
// happens to allocate the same base in both, but that is a DEFAULT, NOT AN
// INVARIANT — `usermod --add-subuids` and `--add-subgids` are separate flags,
// and LDAP/AD-backed or image-provisioned hosts routinely allocate the two
// ranges independently. Reusing the subuid base for the gid map therefore boots
// fine on a lockstep box and dies on a divergent one, which is why both files
// are resolved here and at preflight (VerifySubordinateIDRange).
const (
	subordinateUIDPath = "/etc/subuid"
	subordinateGIDPath = "/etc/subgid"
)

// subordinateProvisionHint is the one remediation string every failure on either
// axis carries. The range it shows is an EXAMPLE, not a canonical value: the
// whole point of reading these files is that the conventional 100000 base must
// never be assumed. Both usermod flags are named because the two allocations are
// independent — provisioning only one leaves the other axis broken.
const subordinateProvisionHint = "provision both ranges with " +
	"`usermod --add-subuids <base>-<count> <user>` and `usermod --add-subgids <base>-<count> <user>` " +
	"(the base is an operator choice — 100000-65536 is only the shadow-utils convention, not a required value); " +
	"rootless podman consumes the same two files"

// SubordinateIDBases returns the first subordinate uid AND the first subordinate
// gid allocated to the invoking user — the host ids virtiofsd's mapping makes
// namespace-uid 0 and namespace-gid 0 so the daemon can chown guest-created
// inodes (see virtiofsdIDMapArgs). The two are read independently because
// /etc/subuid and /etc/subgid are independent allocations (see the consts
// above). It fails with a named error, never a fallback, when either range is
// missing: silently mapping an id the user does not own is exactly the opaque
// boot failure these reads exist to prevent.
func SubordinateIDBases() (uidBase, gidBase int, err error) {
	return subordinateIDBases(subordinateUIDPath, subordinateGIDPath)
}

// subordinateIDBases is the path-injected core, so the divergent-host and
// missing-subgid axes are unit-testable against fixture files rather than
// against the test box's own /etc — which on a shadow-utils default box carries
// IDENTICAL bases in both files and therefore cannot detect a gid map that
// wrongly reuses the uid base.
func subordinateIDBases(uidPath, gidPath string) (uidBase, gidBase int, err error) {
	uidBase, err = subordinateBase(uidPath)
	if err != nil {
		return 0, 0, err
	}
	gidBase, err = subordinateBase(gidPath)
	if err != nil {
		return 0, 0, err
	}
	return uidBase, gidBase, nil
}

// VerifySubordinateIDRange is the startup axis over the same reads: it resolves
// BOTH subordinate bases and discards them, so a host missing EITHER range —
// including the host that has a subuid range but no subgid range, which boots
// past every uid-only check and then dies in virtiofsd's gid map — fails
// preflight with the fix named rather than at the first session boot.
func VerifySubordinateIDRange() error {
	if _, _, err := SubordinateIDBases(); err != nil {
		return err
	}
	return nil
}

// subordinateBase reads one subordinate-id map file and returns the invoking
// user's base within it. Path-taking rather than hardcoded so the uid and gid
// axes share one implementation and neither can silently borrow the other's
// allocation.
func subordinateBase(path string) (int, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is one of this package's two subordinateUIDPath/subordinateGIDPath consts (or a test fixture), never caller input
	if err != nil {
		return 0, fmt.Errorf(
			"microvm: virtiofsd id-mapping requires a subordinate id range for the invoking user in both "+
				"%s and %s, but %s is unreadable: %w (%s)",
			subordinateUIDPath, subordinateGIDPath, path, err, subordinateProvisionHint)
	}
	// Read-only handle over a small text file; a close error after a completed
	// parse cannot affect the already-returned base and is not actionable.
	defer func() { _ = f.Close() }() // deliberate: read-only handle, nothing to flush, and the parse result is already in hand

	uid := os.Getuid()
	// The invoking user's NAME as well as its uid: subuid(5)/subgid(5) entries
	// key on either, and shadow-utils writes the name.
	name := ""
	if u, lookupErr := user.LookupId(strconv.Itoa(uid)); lookupErr == nil {
		name = u.Username
	}
	return parseSubordinateIDBase(f, uid, name, path)
}

// parseSubordinateIDBase is the pure half: the first subuid(5)/subgid(5) entry
// whose owner field matches the invoking user by name or by uid, returning its
// base. The format is `<owner>:<base>:<count>` with `#` comments; a malformed or
// zero-count entry is skipped rather than trusted, since a range that grants no
// id cannot back a mapping. path names the file being parsed, so a failure on
// the gid axis is not misreported as a subuid problem.
func parseSubordinateIDBase(r io.Reader, uid int, username, path string) (int, error) {
	uidStr := strconv.Itoa(uid)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 3 {
			continue
		}
		owner := strings.TrimSpace(fields[0])
		if owner != uidStr && (username == "" || owner != username) {
			continue
		}
		base, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			continue
		}
		count, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || count < 1 || base < 0 {
			continue
		}
		return base, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("microvm: reading %s: %w", path, err)
	}
	who := username
	if who == "" {
		who = "uid " + uidStr
	}
	return 0, fmt.Errorf(
		"microvm: virtiofsd id-mapping requires a subordinate id range for %s in %s, but none is allocated "+
			"(newuidmap validates the mapped host uid range against %s and newgidmap validates the gid range "+
			"against %s — the two are INDEPENDENT allocations, so one being present does not cover the other, "+
			"and virtiofsd would die after binding its socket); %s",
		who, path, subordinateUIDPath, subordinateGIDPath, subordinateProvisionHint)
}
