package runtime

// microvm_quota.go is V6's session-volume quota VERIFICATION path: the expected
// bound (VolumeQuota), what a host probe observed (QuotaReading), and the pure
// decision that turns a reading into a startup verdict (verifyVolumeQuota).
//
// Per D7 the Runner NEVER assigns quota. Project-quota assignment
// (FS_IOC_FSSETXATTR + quotactl) and the loopback-image fallback (mount(2)) both
// need CAP_SYS_ADMIN the rootless Runner lacks, and the frozen no-copy invariant
// forbids swapping the virtio-fs volume for a quota-bounded block device. So the
// multi-tenant deployment provisions per-directory project quota on the
// session-volume filesystem via operator IaC at deploy, and this file's job is
// exactly one read-only, rootless-safe question: *is that quota active?*
//
// Mechanism — statvfs-derived, deliberately NOT quotactl (record §(d) leaves the
// choice to "the rootless-safe mechanism"). The obvious reads both fail rootless
// or answer the wrong question:
//   - quotactl(Q_XGETQUOTA, PRJQUOTA, …) returns the real limits AND usage, but
//     the kernel gates a non-self quota id on CAP_SYS_ADMIN, and a project id is
//     never "self" — so it EPERMs for exactly the caller this check exists for.
//   - FS_IOC_FSGETXATTR yields the directory's project id, but a project id is
//     only a *label*: it is set identically whether or not the filesystem is
//     mounted with enforcement on, so it cannot answer "is the bound live".
//
// statfs(2) answers both at once, unprivileged. XFS (xfs_qm_statvfs, gated on
// project quota ACCT+ENFD && a non-zero project id && FS_XFLAG_PROJINHERIT) and
// ext4 (ext4_statfs_project, gated on the prjquota mount option, PROJINHERIT and
// a non-zero block hard limit) both REWRITE the statfs block/inode totals of a
// project-quota'd directory to the project's limit and usage. So a statfs on the
// volume that reports SMALLER totals than a statfs at its mount root is precisely
// the kernel telling us an enforced project quota is scoping this subtree — and
// the same call hands back the utilization V7 will meter. No syscall wrapper, no
// unsafe, no capability.
//
// The syscall half lives in microvm_quota_linux.go (with a named-refusal stub in
// microvm_quota_unsupported.go); everything here is pure and hermetically tested.

import "fmt"

// VolumeQuota is the EXPECTED bound on a session-volume filesystem: a byte
// ceiling and an inode ceiling. It is the operator-declared bound the preflight
// compares an observed reading against, and the target V7's
// compass_microvm_quota_used_ratio meters against. A zero field means
// unbounded/unknown — a zero VolumeQuota asks the presence-only question ("is
// *some* enforced quota active"), which is what the QuotaRequired preflight
// poses, since MicroVMConfig carries the required-ness flag and not a number.
type VolumeQuota struct {
	// Bytes is the expected block ceiling in bytes; zero means unspecified.
	Bytes int64
	// Inodes is the expected inode ceiling; zero means unspecified.
	Inodes int64
}

// QuotaReading is one rootless statfs observation of a volume path: the totals
// the kernel reports FOR THAT PATH (rewritten to the project bound when an
// enforced project quota scopes it) alongside the same totals read at the
// containing mount root (never rewritten). The pair is what makes an active
// quota detectable without a capability: they are identical on an unquota'd
// tree and diverge exactly when the kernel projected a quota onto the path.
type QuotaReading struct {
	// Path is the volume path that was probed.
	Path string
	// MountRoot is the mount point the path resolves into — the reference the
	// path's own totals are compared against.
	MountRoot string
	// LimitBytes is the block total statfs reports for Path: the project's byte
	// bound under an enforced project quota, else the whole filesystem's size.
	LimitBytes int64
	// UsedBytes is the consumed bytes within LimitBytes' scope.
	UsedBytes int64
	// LimitInodes is the inode total statfs reports for Path (0 on filesystems
	// that do not report an inode count, e.g. btrfs).
	LimitInodes int64
	// UsedInodes is the consumed inodes within LimitInodes' scope.
	UsedInodes int64
	// FilesystemBytes is the block total at MountRoot — the unprojected size.
	FilesystemBytes int64
	// FilesystemInodes is the inode total at MountRoot — unprojected, and on
	// XFS an ESTIMATE rather than a fixed filesystem property: xfs_statfs_inodes
	// computes f_files = min(icount + fakeinos, XFS_MAXINUMBER) where
	// fakeinos = XFS_FSB_TO_INO(mp, f_bfree) (fs/xfs/xfs_super.c), because XFS
	// allocates inodes on demand — so it SHRINKS as the filesystem fills and is
	// read at a different instant than LimitInodes. Active()'s inode arm carries
	// a margin for exactly this.
	FilesystemInodes int64
}

// Active reports whether an ENFORCED project quota is scoping Path: the path's
// own statfs totals are smaller than the mount root's, which only the kernel's
// project-quota projection produces (see the file header). Either axis suffices
// — an operator may bound bytes, inodes, or both — but the two axes are
// ASYMMETRIC: the byte arm accepts any strict inequality, while the inode arm
// requires a 1/16 jitter margin (the reason is two paragraphs down). The inode
// arm is also only consulted on a filesystem that reports inode counts at all.
//
// Deliberately conservative in one direction: a project quota whose byte limit
// happens to equal the whole filesystem's size projects no observable difference
// and reads as absent. That is a bound which constrains nothing, and reading it
// as absent fails a QuotaRequired startup closed (a legible operator refusal)
// rather than passing a tenant-exhaustible volume as bounded.
//
// The INODE arm requires a MARGIN, not merely strict inequality, because the
// mount-root inode total it compares against is an XFS estimate that moves
// between the two statfs calls (FilesystemInodes' doc). Bare `<` on two samples
// of one dynamic number can go true from jitter alone — an unquota'd volume
// reading as bounded, the fail-OPEN direction on a security-relevant preflight.
// A real project inode limit is orders of magnitude below the estimate, so the
// margin costs nothing there.
//
// The arm stays INDEPENDENT of the byte arm rather than being gated behind it:
// an inode-only project quota on a filesystem whose byte limit happens to equal
// its size is a real bound, and gating would read it as absent. The residual
// case the margin cannot rescue — a nearly-full XFS whose shrinking estimate
// falls within the margin of a genuine project inode limit — reads as absent and
// therefore REFUSES a QuotaRequired startup with the observed numbers in the
// message, which is the fail-closed direction.
func (r QuotaReading) Active() bool {
	if r.LimitBytes > 0 && r.FilesystemBytes > 0 && r.LimitBytes < r.FilesystemBytes {
		return true
	}
	if r.LimitInodes <= 0 || r.FilesystemInodes <= 0 {
		return false
	}
	return r.LimitInodes < r.FilesystemInodes-r.FilesystemInodes/inodeMarginDivisor
}

// inodeMarginDivisor sets how much smaller than the mount-root inode estimate a
// path's inode total must be before it counts as a projected bound: 1/16 (6.25%)
// of the estimate. It is a jitter floor, not a tuned threshold — adjacent statfs
// samples of XFS's fakeinos estimate differ by far less, while a provisioned
// project inode limit is smaller by orders of magnitude.
const inodeMarginDivisor = 16

// UsedRatio is the observed byte utilization in [0,1] — the value the preflight
// logs and V7's compass_microvm_quota_used_ratio will meter. Zero when no limit
// was observed.
//
// DUAL MEANING, and the caller must gate on Active(): under an enforced project
// quota the kernel has rewritten both totals to the project's own limit and
// usage, so this is the tenant's utilization; with no quota projected the same
// expression is whole-FILESYSTEM utilization, including every other tenant's
// data and the OS's. Logging both under one key would give a dashboard a number
// whose denominator silently changes, so the preflight logs used_ratio ONLY when
// Active() is true and the raw used/total pair otherwise (microvm_preflight.go's
// verifyQuota). V7 inherits a single-meaning number.
//
// NOTE: this file registers NO meter; exposing the number is V6's job, owning
// the metric set is V7's.
func (r QuotaReading) UsedRatio() float64 {
	if r.LimitBytes <= 0 {
		return 0
	}
	return float64(r.UsedBytes) / float64(r.LimitBytes)
}

// String renders the reading as one operator-legible line, so an error or log
// carries the observed numbers rather than a struct dump.
func (r QuotaReading) String() string {
	return fmt.Sprintf("path=%s mount=%s limit=%dB used=%dB inodes=%d/%d filesystem=%dB/%d inodes",
		r.Path, r.MountRoot, r.LimitBytes, r.UsedBytes, r.UsedInodes, r.LimitInodes,
		r.FilesystemBytes, r.FilesystemInodes)
}

// quotaReadFn reads the active quota scoping a path. It is the effectful seam
// verifyVolumeQuota drives, mirroring preflightProbes' openKVM/statImage split,
// so the decision below is unit-testable against a fabricated reading with no
// quota'd filesystem (which cannot be provisioned rootless).
type quotaReadFn func(path string) (QuotaReading, error)

// verifyVolumeQuota answers whether an operator-provisioned project quota is
// active on path and at least as large as want, returning the observed reading
// (for the caller's utilization log) alongside the verdict. It VERIFIES ONLY —
// no FS_IOC_FSSETXATTR, no quotactl set, no mount (D7).
//
// nil when an enforced quota is active and meets want (a zero want asks
// presence only). Otherwise a startup error naming the volume, what was
// observed, and the operator fix — the D3 "name the missing capability and the
// fix" posture, never a degrade signal. The caller decides whether an absent
// quota is fatal (QuotaRequired) or merely logged.
func verifyVolumeQuota(path string, want VolumeQuota, read quotaReadFn) (QuotaReading, error) {
	if path == "" {
		return QuotaReading{}, fmt.Errorf(
			"microvm preflight: session-volume quota: no volume path to verify: %s",
			"set --microvm-volume-root or $COMPASS_MICROVM_VOLUME_ROOT to the parent dir session volumes "+
				"are minted under (NOT the run-root: that is the socket dir, held to a short /tmp path by the "+
				"AF_UNIX sun_path budget, and routinely a different filesystem, so it is not a valid proxy)")
	}
	reading, err := read(path)
	if err != nil {
		return QuotaReading{}, fmt.Errorf("microvm preflight: reading the session-volume quota for %q: %w", path, err)
	}
	reading.Path = path
	if !reading.Active() {
		return reading, fmt.Errorf(
			"microvm preflight: no enforced project quota on the session-volume filesystem at %q (observed %s): "+
				"provision a per-directory project quota on that filesystem via operator IaC "+
				"(e.g. mount it with prjquota, set a project id + FS_XFLAG_PROJINHERIT on the volume dir, and set its block/inode limits), "+
				"or run the single-tenant profile with quota-required off",
			path, reading)
	}
	if want.Bytes > 0 && reading.LimitBytes < want.Bytes {
		return reading, fmt.Errorf(
			"microvm preflight: the session-volume project quota at %q bounds %d bytes, below the required %d "+
				"(observed %s): raise the provisioned project block limit",
			path, reading.LimitBytes, want.Bytes, reading)
	}
	if want.Inodes > 0 && reading.LimitInodes < want.Inodes {
		return reading, fmt.Errorf(
			"microvm preflight: the session-volume project quota at %q bounds %d inodes, below the required %d "+
				"(observed %s): raise the provisioned project inode limit",
			path, reading.LimitInodes, want.Inodes, reading)
	}
	return reading, nil
}
