//go:build unix

package runtime

// Hermetic unit tests for V6's volume-quota VERIFICATION (microvm_quota.go).
// Mechanism and the rejected alternatives: microvm_quota.go's header.
//
// These carry NO microvm build tag on purpose: they are what genuinely proves
// the verification logic on any box, including one where a prjquota-active
// filesystem cannot be provisioned (that needs root, D7 / Global Constraint
// "Rootless is hard"). The decision is split from the syscall behind
// quotaReadFn precisely so it is covered here rather than left to a leg that
// skips.
//
// The real statfs probe (readVolumeQuota) is exercised too, but only for what is
// honestly assertable without a quota'd filesystem: that it reads a real path,
// and that an unquota'd tree correctly reads as NOT active. A green here does
// not claim quota enforcement was proven — the guest-side ENOSPC/EDQUOT proof is
// the root-gated leg in microvm_isolation_microvm_test.go.

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// quotaReading builds an "active enforced quota" reading: the path's own statfs
// totals are strictly smaller than the mount root's, which is exactly what the
// kernel's project-quota projection produces (xfs_qm_statvfs / ext4_statfs_project).
func quotaReading(limitBytes, usedBytes, limitInodes, usedInodes int64) QuotaReading {
	return QuotaReading{
		Path:             "/srv/compass/volumes",
		MountRoot:        "/srv",
		LimitBytes:       limitBytes,
		UsedBytes:        usedBytes,
		LimitInodes:      limitInodes,
		UsedInodes:       usedInodes,
		FilesystemBytes:  1 << 40, // 1 TiB filesystem
		FilesystemInodes: 1 << 26,
	}
}

// TestQuotaReadingActive is the core detection predicate: a path whose statfs
// totals are smaller than its mount root's has an enforced project quota
// projected onto it; equal totals mean none. Either axis (bytes or inodes) can
// carry the bound, and a filesystem that reports no inode count at all (btrfs
// reports Files == 0) must not make the inode arm read as active.
func TestQuotaReadingActive(t *testing.T) {
	tests := []struct {
		name    string
		reading QuotaReading
		want    bool
	}{
		{
			name:    "byte bound below filesystem size is active",
			reading: quotaReading(10<<30, 1<<30, 0, 0),
			want:    true,
		},
		{
			name:    "inode bound below filesystem inodes is active",
			reading: quotaReading(0, 0, 1<<20, 512),
			want:    true,
		},
		{
			name:    "both bounds set is active",
			reading: quotaReading(10<<30, 1<<30, 1<<20, 512),
			want:    true,
		},
		{
			name: "totals equal to the mount root are not active",
			// An unquota'd tree: statfs at the path and at the mount root
			// report the same filesystem. This is the shape a dev box produces.
			reading: QuotaReading{
				LimitBytes: 1 << 40, UsedBytes: 1 << 30,
				LimitInodes: 1 << 26, UsedInodes: 4096,
				FilesystemBytes: 1 << 40, FilesystemInodes: 1 << 26,
			},
			want: false,
		},
		{
			name: "a bound equal to the whole filesystem reads as absent",
			// Conservative by design: such a quota constrains nothing, and
			// reading it as absent fails a required startup closed rather than
			// passing a tenant-exhaustible volume off as bounded.
			reading: QuotaReading{
				LimitBytes: 1 << 40, FilesystemBytes: 1 << 40,
			},
			want: false,
		},
		{
			name: "zero inode counts (btrfs) do not read as an inode bound",
			// btrfs reports Files == 0; a naive `LimitInodes < FilesystemInodes`
			// would be 0 < 0 = false here, but a reading where only the
			// FILESYSTEM inodes are zero must also not go active.
			reading: QuotaReading{
				LimitBytes: 1 << 40, UsedBytes: 1 << 30,
				LimitInodes: 0, UsedInodes: 0,
				FilesystemBytes: 1 << 40, FilesystemInodes: 0,
			},
			want: false,
		},
		{
			// The XFS fakeinos case (FilesystemInodes' doc): the mount-root
			// inode total is an estimate that SHRINKS as the filesystem fills,
			// so on a full-ish XFS it can dip just below a generous project
			// inode limit. Bare `<` would read that jitter as a projected
			// bound, which is the fail-OPEN direction; the margin rejects it.
			name: "inode arm where the mount-root estimate shrank to just above the project limit",
			reading: QuotaReading{
				LimitBytes: 1 << 40, UsedBytes: 1 << 39,
				LimitInodes: 1_000_000, UsedInodes: 900_000,
				FilesystemBytes: 1 << 40, FilesystemInodes: 1_010_000,
			},
			want: false,
		},
		{
			// Just INSIDE the 1/16 margin: 940_000 < 1_010_000 - 63_125. This
			// pins the boundary, so a change to inodeMarginDivisor is a
			// deliberate edit rather than a silent widening.
			name: "an inode bound past the margin is active",
			reading: QuotaReading{
				LimitBytes: 1 << 40, UsedBytes: 1 << 39,
				LimitInodes: 940_000, UsedInodes: 900_000,
				FilesystemBytes: 1 << 40, FilesystemInodes: 1_010_000,
			},
			want: true,
		},
		{
			// An inode-only bound must NOT be gated behind the byte arm: here
			// the byte limit equals the filesystem size (so the byte arm is
			// silent) while a real inode bound is projected.
			name: "an inode-only bound is active even when the byte limit equals the filesystem size",
			reading: QuotaReading{
				LimitBytes: 1 << 40, UsedBytes: 1 << 30,
				LimitInodes: 1 << 20, UsedInodes: 512,
				FilesystemBytes: 1 << 40, FilesystemInodes: 1 << 26,
			},
			want: true,
		},
		{
			name:    "the zero reading is not active",
			reading: QuotaReading{},
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.reading.Active(); got != tt.want {
				t.Fatalf("Active() = %v, want %v (reading: %s)", got, tt.want, tt.reading)
			}
		})
	}
}

// TestQuotaReadingUsedRatio pins the utilization value V6 exposes and V7 will
// meter: used/limit, and zero (not NaN, not +Inf) when no limit was observed.
func TestQuotaReadingUsedRatio(t *testing.T) {
	tests := []struct {
		name    string
		reading QuotaReading
		want    float64
	}{
		{name: "quarter full", reading: quotaReading(4<<30, 1<<30, 0, 0), want: 0.25},
		{name: "empty", reading: quotaReading(4<<30, 0, 0, 0), want: 0},
		{name: "full", reading: quotaReading(4<<30, 4<<30, 0, 0), want: 1},
		// No limit must not divide by zero into NaN/+Inf — a metric consumer
		// (V7) would otherwise export a poisoned sample.
		{name: "no limit observed", reading: QuotaReading{UsedBytes: 1 << 30}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.reading.UsedRatio()
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("UsedRatio() = %v, want a finite value", got)
			}
			if got != tt.want {
				t.Fatalf("UsedRatio() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVerifyVolumeQuota is the verification decision per axis, driven through
// the injected probe so no quota'd filesystem is needed: an active quota meeting
// the want passes; an absent one errors naming the volume and the operator fix;
// a present-but-too-small bound errors naming both numbers; a probe failure
// propagates rather than reading as "absent" or "fine".
func TestVerifyVolumeQuota(t *testing.T) {
	const volume = "/srv/compass/volumes"

	tests := []struct {
		name      string
		path      string
		want      VolumeQuota
		read      quotaReadFn
		wantOK    bool
		wantParts []string
	}{
		{
			name:   "active quota, presence-only want passes",
			path:   volume,
			want:   VolumeQuota{},
			read:   func(string) (QuotaReading, error) { return quotaReading(10<<30, 1<<30, 1<<20, 512), nil },
			wantOK: true,
		},
		{
			name:   "active quota at or above the wanted bound passes",
			path:   volume,
			want:   VolumeQuota{Bytes: 8 << 30, Inodes: 1 << 19},
			read:   func(string) (QuotaReading, error) { return quotaReading(10<<30, 1<<30, 1<<20, 512), nil },
			wantOK: true,
		},
		{
			name: "absent quota names the volume and the operator fix",
			path: volume,
			want: VolumeQuota{},
			read: func(string) (QuotaReading, error) {
				return QuotaReading{
					LimitBytes: 1 << 40, UsedBytes: 1 << 30,
					FilesystemBytes: 1 << 40,
				}, nil
			},
			wantParts: []string{volume, "no enforced project quota", "prjquota", "FS_XFLAG_PROJINHERIT", "quota-required off"},
		},
		{
			name:      "byte bound below the wanted bound names both numbers",
			path:      volume,
			want:      VolumeQuota{Bytes: 100 << 30},
			read:      func(string) (QuotaReading, error) { return quotaReading(10<<30, 1<<30, 0, 0), nil },
			wantParts: []string{volume, "10737418240", "107374182400", "raise the provisioned project block limit"},
		},
		{
			name:      "inode bound below the wanted bound names both numbers",
			path:      volume,
			want:      VolumeQuota{Inodes: 1 << 24},
			read:      func(string) (QuotaReading, error) { return quotaReading(10<<30, 1<<30, 1<<20, 512), nil },
			wantParts: []string{volume, "1048576", "16777216", "raise the provisioned project inode limit"},
		},
		{
			name:      "a probe failure propagates, never reads as absent-or-fine",
			path:      volume,
			want:      VolumeQuota{},
			read:      func(string) (QuotaReading, error) { return QuotaReading{}, errors.New("statfs: permission denied") },
			wantParts: []string{volume, "permission denied"},
		},
		{
			// The knob must be the VOLUME root, never the retired run-root: the
			// run-root is the socket dir on a routinely different filesystem, so
			// setting it cannot make this check pass. Pinning the run-root text
			// here is what regression-locked that stale advice.
			name:      "an empty volume path names the volume-root knob, not the retired run-root",
			path:      "",
			want:      VolumeQuota{},
			read:      func(string) (QuotaReading, error) { return quotaReading(10<<30, 0, 0, 0), nil },
			wantParts: []string{"--microvm-volume-root", "COMPASS_MICROVM_VOLUME_ROOT"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reading, err := verifyVolumeQuota(tt.path, tt.want, tt.read)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("verifyVolumeQuota = %v, want nil", err)
				}
				// A passing verify must hand back the utilization the caller
				// logs and V7 meters, not a zero struct.
				if reading.LimitBytes == 0 {
					t.Fatalf("verifyVolumeQuota returned reading %+v with no limit; the caller needs the observed bound", reading)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyVolumeQuota = nil, want an error mentioning %v", tt.wantParts)
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not name %q", err.Error(), part)
				}
			}
		})
	}
}

// TestVerifyVolumeQuotaNeverAssigns is the D7 posture as a structural test: the
// verification path takes a READ function and nothing else, so there is no seam
// through which it could set a project id or a limit. Driven by counting probe
// calls and asserting the reading is returned unmutated apart from Path —
// verification observes, it does not write.
func TestVerifyVolumeQuotaNeverAssigns(t *testing.T) {
	reads := 0
	fixed := quotaReading(10<<30, 2<<30, 1<<20, 1024)
	read := func(string) (QuotaReading, error) {
		reads++
		return fixed, nil
	}

	got, err := verifyVolumeQuota("/srv/compass/volumes", VolumeQuota{Bytes: 1 << 30}, read)
	if err != nil {
		t.Fatalf("verifyVolumeQuota = %v, want nil", err)
	}
	if reads != 1 {
		t.Fatalf("the quota probe ran %d times, want exactly 1 (one read-only observation, D7)", reads)
	}
	if got.LimitBytes != fixed.LimitBytes || got.UsedBytes != fixed.UsedBytes ||
		got.LimitInodes != fixed.LimitInodes || got.UsedInodes != fixed.UsedInodes {
		t.Fatalf("verifyVolumeQuota returned %+v, want the probed reading unchanged %+v", got, fixed)
	}
	if got.Path != "/srv/compass/volumes" {
		t.Fatalf("returned reading Path = %q, want the verified path", got.Path)
	}
}

// TestReadVolumeQuotaOnRealPath exercises the PRODUCTION statfs probe against a
// real directory. What it can honestly assert without root is bounded but real:
// the probe succeeds, reports a plausible filesystem, resolves a mount root, and
// — since no test box's temp dir carries a project quota — reads as NOT active.
// That negative is load-bearing: it is what proves the detection does not
// false-positive and pass an unbounded volume off as quota'd.
func TestReadVolumeQuotaOnRealPath(t *testing.T) {
	dir := t.TempDir()
	reading, err := readVolumeQuota(dir)
	if err != nil {
		t.Fatalf("readVolumeQuota(%q) = %v, want a successful rootless read", dir, err)
	}
	if reading.LimitBytes <= 0 {
		t.Fatalf("reading %s has no block total; statfs must report the filesystem size", reading)
	}
	if reading.MountRoot == "" {
		t.Fatalf("reading %s resolved no mount root", reading)
	}
	if reading.UsedBytes < 0 || reading.UsedBytes > reading.LimitBytes {
		t.Fatalf("reading %s has nonsensical usage", reading)
	}
	if reading.Active() {
		t.Fatalf("reading %s reports an active project quota on a plain temp dir; "+
			"the detection must not false-positive (that would pass an unbounded volume as quota'd)", reading)
	}
	// The utilization the preflight logs must be finite and in range even with
	// no quota — V7 meters this value.
	if ratio := reading.UsedRatio(); ratio < 0 || ratio > 1 || math.IsNaN(ratio) {
		t.Fatalf("UsedRatio() = %v on reading %s, want a finite ratio in [0,1]", ratio, reading)
	}
}

// TestReadVolumeQuotaAbsentPath: a path that does not exist is a probe ERROR,
// not a silent "no quota". Under QuotaRequired that difference decides whether
// startup fails with the real cause (an unreachable volume) or with a misleading
// missing-quota message.
func TestReadVolumeQuotaAbsentPath(t *testing.T) {
	if _, err := readVolumeQuota(t.TempDir() + "/does-not-exist"); err == nil {
		t.Fatal("readVolumeQuota on an absent path = nil error, want a failure naming the path")
	}
}
