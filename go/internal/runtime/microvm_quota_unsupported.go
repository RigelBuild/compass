//go:build !linux

package runtime

// The non-Linux leg of V6's quota probe. The verification mechanism is a Linux
// kernel property (XFS/ext4 project-quota-projected statvfs); mechanism and the
// rejected alternatives: microvm_quota.go's header.

// The microVM backend only boots on Linux, but this seam must still RESOLVE on
// every GOOS: microvm_quota.go is untagged, so readVolumeQuota must exist under
// every build constraint. Hence a named refusal — the fail-closed answer, since
// QuotaRequired where the bound can't be observed must error, never pass.

import "errors"

// readVolumeQuota refuses on non-Linux: there is no rootless project-quota read
// here, so the required-quota preflight fails closed with the reason named.
func readVolumeQuota(_ string) (QuotaReading, error) {
	return QuotaReading{}, errors.New(
		"session-volume project-quota verification is Linux-only (it reads the kernel's " +
			"project-quota-projected statfs totals); the microVM backend requires a KVM-capable Linux host")
}
