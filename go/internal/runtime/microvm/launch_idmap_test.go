//go:build unix

package microvm

// Hermetic argv assertions for virtiofsd's id translation and capability trim
// (record §(d) host-ownership parity), following launch_cmdline_test.go's shape:
// no VMM boots, no daemon spawns — the exact flag/value pairs are the contract,
// because a wrong pair fails at first boot rather than as a legible error.

import (
	"os"
	"slices"
	"strconv"
	"testing"
)

// flagValues returns every value token following an occurrence of flag in argv,
// in order — so a test asserts both the count and the exact specs of a repeated
// flag rather than a single Contains.
func flagValues(argv []string, flag string) []string {
	var out []string
	for i, tok := range argv {
		if tok == flag && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// TestVirtiofsdTranslateArgsExactSpecs pins both translation pairs for a real
// agent uid. The gid arm is the discriminating one: its host side must be
// os.Getgid(), NOT os.Getuid() — a host user's gid routinely differs from its
// uid (e.g. 1000:100), and collapsing gid onto uid is the parity break the KVM
// parity leg detects. No --uid-map/--gid-map may appear: those make the daemon
// namespace-root.
func TestVirtiofsdTranslateArgsExactSpecs(t *testing.T) {
	const agentUID = 1000
	argv := virtiofsdTranslateArgs(agentUID)

	agent := strconv.Itoa(agentUID)
	wantUID := []string{"map:" + agent + ":" + strconv.Itoa(os.Getuid()) + ":1"}
	wantGID := []string{"map:" + agent + ":" + strconv.Itoa(os.Getgid()) + ":1"}

	if got := flagValues(argv, "--translate-uid"); !slices.Equal(got, wantUID) {
		t.Errorf("--translate-uid specs = %v, want %v (argv %v)", got, wantUID, argv)
	}
	if got := flagValues(argv, "--translate-gid"); !slices.Equal(got, wantGID) {
		t.Errorf("--translate-gid specs = %v, want %v (argv %v)", got, wantGID, argv)
	}
	for _, banned := range []string{"--uid-map", "--gid-map"} {
		if slices.Contains(argv, banned) {
			t.Errorf("argv %v carries %s, which maps an id to namespace-root", argv, banned)
		}
	}
}

// TestVirtiofsdTranslateArgsUnmappedSpike pins the V2a carve-out: a zero
// agentUID (the spike harness, which shares a throwaway dir and asserts nothing
// about ownership) gets NO translation at all, so that suite keeps booting
// unchanged.
func TestVirtiofsdTranslateArgsUnmappedSpike(t *testing.T) {
	if argv := virtiofsdTranslateArgs(0); argv != nil {
		t.Fatalf("virtiofsdTranslateArgs(0) = %v, want nil (the V2a spike share stays untranslated)", argv)
	}
}

// TestVirtiofsdArgsDropsMknod pins the capability trim on the full argv: a
// workspace share has no legitimate device nodes, so CAP_MKNOD is dropped from
// the set virtiofsd retains in its sandbox — on EVERY boot, including the
// unmapped spike, since the flag costs nothing there.
//
// The assertion is against the modcapsDropMknod CONST, not a literal, and that
// is load-bearing: virtiofsd does NOT validate capability names — verified by
// execution, `--modcaps=-not_a_real_cap` starts the daemon normally — so a typo
// makes the flag a silent no-op with no diagnostic anywhere, and this argv
// assertion is the only guard. A literal repeated here would agree with a typo
// in launch.go; sharing the const means a typo has to be made once to be wrong
// in both places, which the exact-literal check below then catches.
func TestVirtiofsdArgsDropsMknod(t *testing.T) {
	if modcapsDropMknod != "--modcaps=-mknod" {
		t.Fatalf("modcapsDropMknod = %q, want %q — virtiofsd silently ACCEPTS unknown capability names, "+
			"so a drifted literal is an undetectable no-op that leaves CAP_MKNOD on the share", modcapsDropMknod, "--modcaps=-mknod")
	}
	for _, agentUID := range []uint32{0, 1000} {
		cfg := BootConfig{
			FSSocket:    "/tmp/cvm/virtiofsd.sock",
			FSSharedDir: "/tmp/cvm/share",
			AgentUID:    agentUID,
		}
		argv := virtiofsdArgs(cfg)
		if !slices.Contains(argv, modcapsDropMknod) {
			t.Errorf("virtiofsdArgs(AgentUID=%d) = %v, want it to carry %q", agentUID, argv, modcapsDropMknod)
		}
		for _, want := range []string{
			"--socket-path=" + cfg.FSSocket,
			"--shared-dir=" + cfg.FSSharedDir,
			"--sandbox=namespace",
		} {
			if !slices.Contains(argv, want) {
				t.Errorf("virtiofsdArgs(AgentUID=%d) = %v, want it to carry %q", agentUID, argv, want)
			}
		}
	}
}

// TestVirtiofsdArgsIncludesTranslation is the seam assertion the launch path
// depends on: the full argv carries the translation for a real agent uid and
// none for the spike.
func TestVirtiofsdArgsIncludesTranslation(t *testing.T) {
	mapped := virtiofsdArgs(BootConfig{AgentUID: 1000})
	if got := len(flagValues(mapped, "--translate-uid")); got != 1 {
		t.Errorf("mapped argv %v carries %d --translate-uid specs, want 1", mapped, got)
	}
	if got := len(flagValues(mapped, "--translate-gid")); got != 1 {
		t.Errorf("mapped argv %v carries %d --translate-gid specs, want 1", mapped, got)
	}
	spike := virtiofsdArgs(BootConfig{AgentUID: 0})
	for _, flag := range []string{"--translate-uid", "--translate-gid"} {
		if got := flagValues(spike, flag); len(got) != 0 {
			t.Errorf("spike argv %v carries %s specs %v, want none", spike, flag, got)
		}
	}
}
