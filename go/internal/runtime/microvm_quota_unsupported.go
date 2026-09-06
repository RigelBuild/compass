//go:build !linux

package runtime

// The non-Linux leg of V6's quota probe. The verification mechanism is a Linux
// kernel property (XFS/ext4 project-quota-projected statvfs, see
// microvm_quota.go's header), and the microVM backend only ever boots on a
// KVM-capable Linux host — but the runtime package must still type-check
// everywhere (the darwin CI lane compiles it, microvm.go's header), so the seam
// gets a named refusal rather than a build break or a silent "quota active".
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
