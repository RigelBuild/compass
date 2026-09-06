//go:build !linux

package runtime

// The non-Linux leg of V6's quota probe. The verification mechanism is a Linux
// kernel property (XFS/ext4 project-quota-projected statvfs); mechanism and the
// rejected alternatives: microvm_quota.go's header.
//
// The microVM backend only ever boots on a KVM-capable Linux host, but this
// seam must still RESOLVE on every GOOS the package is built for:
// microvm_quota.go is untagged, so readVolumeQuota — the function its
// quotaReadFn seam is wired to — has to exist under every build constraint or
// the package does not build off Linux at all. Hence a named refusal rather
// than a build break or a silent "quota active".
//
// Refusing is the fail-closed answer: QuotaRequired on a platform where the
// bound cannot be observed must be a legible startup error, never a pass.

import "errors"

// readVolumeQuota refuses on non-Linux: there is no rootless project-quota read
// here, so the required-quota preflight fails closed with the reason named.
func readVolumeQuota(_ string) (QuotaReading, error) {
	return QuotaReading{}, errors.New(
		"session-volume project-quota verification is Linux-only (it reads the kernel's " +
			"project-quota-projected statfs totals); the microVM backend requires a KVM-capable Linux host")
}
