// Wire mapping for the runtime tier and egress posture a session reports.
// Both map an unknown value onto UNSPECIFIED rather than a plausible default: a
// security surface that guesses is worse than one that says it does not know.

package runner

import (
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

func runtimeTierProto(tier runtime.WorkloadTier) compassv1.RuntimeTier {
	switch tier {
	case runtime.WorkloadTierPodman:
		return compassv1.RuntimeTier_RUNTIME_TIER_PODMAN
	case runtime.WorkloadTierMicroVM:
		return compassv1.RuntimeTier_RUNTIME_TIER_MICROVM
	case runtime.WorkloadTierAppleContainer:
		return compassv1.RuntimeTier_RUNTIME_TIER_APPLE_CONTAINER
	case runtime.WorkloadTierHost:
		return compassv1.RuntimeTier_RUNTIME_TIER_HOST
	default:
		return compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED
	}
}

func egressPostureProto(posture runtime.EgressPosture) compassv1.EgressPosture {
	switch posture {
	case runtime.EgressPostureArmed:
		return compassv1.EgressPosture_EGRESS_POSTURE_ARMED
	case runtime.EgressPostureUnenforced:
		return compassv1.EgressPosture_EGRESS_POSTURE_UNENFORCED
	default:
		return compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED
	}
}
