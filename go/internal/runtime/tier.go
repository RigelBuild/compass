// Tier identity: what kind of isolation boundary a backend puts an agent
// behind. Reported outward per session so an operator can see which boundary
// their agent actually got, rather than inferring it from a green launch.

package runtime

// WorkloadTier names the isolation boundary a backend provides.
type WorkloadTier string

const (
	// WorkloadTierPodman is a rootless container with its own network namespace.
	WorkloadTierPodman WorkloadTier = "podman"
	// WorkloadTierMicroVM is a hardware-virtualized guest.
	WorkloadTierMicroVM WorkloadTier = "microvm"
	// WorkloadTierAppleContainer is Apple's container runtime on macOS.
	WorkloadTierAppleContainer WorkloadTier = "apple-container"
	// WorkloadTierHost is a direct child process of the Runner — no boundary.
	WorkloadTierHost WorkloadTier = "host"
)

// workloadTierNamer is a backend that names its own tier. Like the egress
// markers, it is deliberately NOT a verb on the frozen WorkloadRuntime
// interface (podman.go): callers probe for it. A WorkloadRuntime decorator must
// re-expose Tier, or the session surface loses the tier it reports.
type workloadTierNamer interface {
	Tier() WorkloadTier
}

// TierOf names the tier a backend provides. An unrecognized backend reports the
// empty tier rather than guessing: a wrong tier on a security surface is worse
// than an absent one, and the wire's unspecified value renders as unknown.
func TierOf(engine WorkloadRuntime) WorkloadTier {
	if namer, ok := engine.(workloadTierNamer); ok {
		return namer.Tier()
	}
	return ""
}

// PostureOf reports how a backend constrains agent egress, probing the same
// egressUnenforcer marker AgentRuntime.EgressPosture does so the value a Runner
// declares at enrollment matches the one a live workload would report. A backend
// with no isolation boundary to firewall is unenforced; every other backend is
// armed.
func PostureOf(engine WorkloadRuntime) EgressPosture {
	if unenforcer, ok := engine.(egressUnenforcer); ok && unenforcer.EgressUnenforced() {
		return EgressPostureUnenforced
	}
	return EgressPostureArmed
}

// Tier reports the podman tier.
func (p *PodmanCLI) Tier() WorkloadTier { return WorkloadTierPodman }

// Tier reports the microVM tier.
func (m *MicroVMRuntime) Tier() WorkloadTier { return WorkloadTierMicroVM }

// Tier reports the Apple-container tier.
func (a *AppleContainerCLI) Tier() WorkloadTier { return WorkloadTierAppleContainer }

// Tier reports the host tier.
func (h *HostRuntime) Tier() WorkloadTier { return WorkloadTierHost }
