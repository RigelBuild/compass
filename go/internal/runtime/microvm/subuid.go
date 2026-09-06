//go:build unix

package microvm

// The /etc/subuid read behind virtiofsd's id mapping. virtiofsd shells out to
// newuidmap(1) for a non-trivial --uid-map, and newuidmap VALIDATES the
// requested host range against subuid(5) ("the range of subordinate user IDs
// must have been set up via subuid(5)", virtiofsd README §--uid-map). So the
// base mapped to namespace-uid 0 is a per-host allocation that must be READ, not
// assumed: shadow-utils happens to allocate 100000 to the first user, but a
// second user on the same box gets 165536, and LDAP/AD-backed or
// container-image-provisioned hosts routinely differ. Assuming it fails in the
// worst way — newuidmap refuses, virtiofsd dies before binding its socket, and
// the boot surfaces as an opaque "waiting for daemon sockets" timeout with the
// real cause only in virtiofsd.log.
//
// Split into a pure parse over an io.Reader plus a thin file-reading wrapper so
// the argv tests can pin the exact mapping against a fixed base with no
// dependence on the test box's own /etc/subuid.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// subordinateIDPath is the subuid(5) map newuidmap validates a requested host
// range against. Its sibling /etc/subgid is deliberately NOT read: the gid map
// reuses this base, matching how shadow-utils allocates the two ranges in
// lockstep and how rootless podman consumes them.
const subordinateIDPath = "/etc/subuid"

// SubordinateIDBase returns the first subordinate uid allocated to the invoking
// user — the host id virtiofsd's mapping makes namespace-uid 0 so the daemon can
// chown guest-created inodes (see virtiofsdIDMapArgs). It fails with a named
// error, never a fallback, when the user has no subordinate range: silently
// mapping an id the user does not own is exactly the opaque boot failure this
// read exists to prevent.
func SubordinateIDBase() (int, error) {
	f, err := os.Open(subordinateIDPath)
	if err != nil {
		return 0, fmt.Errorf(
			"microvm: virtiofsd id-mapping requires a subordinate uid range for the invoking user, "+
				"but %s is unreadable: %w (rootless podman requires the same file; "+
				"provision it with `usermod --add-subuids 100000-165535 <user>`)",
			subordinateIDPath, err)
	}
	// Read-only handle over a small text file; a close error after a completed
	// parse cannot affect the already-returned base and is not actionable.
	defer func() { _ = f.Close() }()

	uid := os.Getuid()
	// The invoking user's NAME as well as its uid: subuid(5) entries key on
	// either, and shadow-utils writes the name.
	name := ""
	if u, lookupErr := user.LookupId(strconv.Itoa(uid)); lookupErr == nil {
		name = u.Username
	}
	return parseSubordinateIDBase(f, uid, name)
}

// VerifySubordinateIDRange is the startup axis over the same read: it resolves
// the invoking user's subordinate base and discards it, so a host with no
// subordinate range fails preflight with the fix named rather than at the first
// session boot as a daemon-socket timeout.
func VerifySubordinateIDRange() error {
	if _, err := SubordinateIDBase(); err != nil {
		return err
	}
	return nil
}

// parseSubordinateIDBase is the pure half: the first subuid(5) entry whose owner
// field matches the invoking user by name or by uid, returning its base. The
// format is `<owner>:<base>:<count>` with `#` comments; a malformed or
// zero-count entry is skipped rather than trusted, since a range that grants no
// id cannot back a mapping.
func parseSubordinateIDBase(r io.Reader, uid int, username string) (int, error) {
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
		return 0, fmt.Errorf("microvm: reading %s: %w", subordinateIDPath, err)
	}
	who := username
	if who == "" {
		who = "uid " + uidStr
	}
	return 0, fmt.Errorf(
		"microvm: virtiofsd id-mapping requires a subordinate uid range for %s in %s, but none is allocated "+
			"(newuidmap validates the mapped host range against subuid(5), so virtiofsd would die before binding "+
			"its socket); rootless podman requires the same — provision one with "+
			"`usermod --add-subuids 100000-165535 %s` and the matching --add-subgids",
		who, subordinateIDPath, who)
}
