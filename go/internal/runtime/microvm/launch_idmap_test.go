//go:build unix

package microvm

// Hermetic argv assertions for virtiofsd's id mapping and capability trim
// (record §(d) host-ownership parity), following launch_cmdline_test.go's shape:
// no VMM boots, no daemon spawns — the exact flag/value pairs are the contract,
// because a wrong pair fails at first boot as an opaque daemon-socket timeout
// rather than as a legible error.
//
// The subordinate bases are injected as fixed values rather than read from the
// test box's /etc/subuid and /etc/subgid, which is the whole point of the
// parse/spawn split in subuid.go: the mapping is pinned identically on a host
// allocated 100000 and one allocated 165536 — and, crucially, on a host whose
// two files DIVERGE, which this box's own /etc (both `mattw:100000:65536`)
// cannot exhibit.

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// testSubBase is a deliberately NON-conventional subordinate base: 100000 would
// pass even against the old hardcode, so the assertions below would not detect a
// regression back to it.
const testSubBase = 165536

// testSubGIDBase is a subordinate GID base DISTINCT from testSubBase, for the
// rows that must prove the two axes carry independent values. /etc/subuid and
// /etc/subgid are separate allocations, so a gid map built from the subuid base
// boots on a lockstep host and kills virtiofsd on a divergent one.
const testSubGIDBase = 265536

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

// TestVirtiofsdIDMapArgsExactSpecs pins all four mapping pairs for a real agent
// uid. Two arms are discriminating: the gid arm's HOST side must be os.Getgid(),
// NOT os.Getuid() — a host user's gid is routinely different from its uid (e.g.
// 1000:100), and collapsing gid onto uid is precisely the host-ownership parity
// break the KVM parity leg detects — and the gid arm's NAMESPACE-ROOT side must
// come from the /etc/subgid base, not the /etc/subuid one (see
// TestVirtiofsdIDMapArgsDivergentSubordinateBases).
func TestVirtiofsdIDMapArgsExactSpecs(t *testing.T) {
	const agentUID = 1000
	argv := virtiofsdIDMapArgs(agentUID, testSubBase, testSubGIDBase)

	agent := strconv.Itoa(agentUID)
	wantUID := []string{
		":0:" + strconv.Itoa(testSubBase) + ":1:",
		":" + agent + ":" + strconv.Itoa(os.Getuid()) + ":1:",
	}
	wantGID := []string{
		":0:" + strconv.Itoa(testSubGIDBase) + ":1:",
		":" + agent + ":" + strconv.Itoa(os.Getgid()) + ":1:",
	}

	if got := flagValues(argv, "--uid-map"); !slices.Equal(got, wantUID) {
		t.Errorf("--uid-map specs = %v, want %v (argv %v)", got, wantUID, argv)
	}
	if got := flagValues(argv, "--gid-map"); !slices.Equal(got, wantGID) {
		t.Errorf("--gid-map specs = %v, want %v (argv %v)", got, wantGID, argv)
	}
	// Neither base may come from a hardcoded 100000 — the correctness bug on
	// any host whose /etc/subuid or /etc/subgid range starts elsewhere.
	for _, spec := range append(flagValues(argv, "--uid-map"), flagValues(argv, "--gid-map")...) {
		if strings.Contains(spec, ":100000:") {
			t.Errorf("spec %q carries a hardcoded 100000 base; the subordinate bases must be the parsed ones (%d/%d)",
				spec, testSubBase, testSubGIDBase)
		}
	}
}

// TestVirtiofsdIDMapArgsDivergentSubordinateBases is the row the whole
// subuid/subgid split exists for: with DIFFERENT uid and gid bases injected, the
// two namespace-root arms must carry DIFFERENT host ids.
//
// It cannot be a host-dependent test. /etc/subuid and /etc/subgid on a
// shadow-utils-default box (this one included: both read `mattw:100000:65536`)
// hold the SAME base, which is exactly why a gid map that reuses the subuid base
// passes everywhere it is developed and then dies on a host provisioned with
// `usermod --add-subgids` alone. newgidmap validates the --gid-map host range
// against /etc/subgid, so on such a host virtiofsd refuses the mapping — AFTER
// binding its socket, which is what makes the failure so illegible.
func TestVirtiofsdIDMapArgsDivergentSubordinateBases(t *testing.T) {
	const agentUID = 1000
	argv := virtiofsdIDMapArgs(agentUID, testSubBase, testSubGIDBase)

	uidSpecs := flagValues(argv, "--uid-map")
	gidSpecs := flagValues(argv, "--gid-map")
	// Guarded rather than indexed blind: a mapping that lost an arm entirely
	// would panic here and report a crash instead of the missing flag.
	if len(uidSpecs) != 2 || len(gidSpecs) != 2 {
		t.Fatalf("argv %v carries %d --uid-map and %d --gid-map specs, want 2 of each "+
			"(namespace-root plus the agent id, per axis)", argv, len(uidSpecs), len(gidSpecs))
	}
	uidRoot, gidRoot := uidSpecs[0], gidSpecs[0]
	if uidRoot == gidRoot {
		t.Fatalf("the --uid-map and --gid-map namespace-root specs are both %q with uidBase=%d != gidBase=%d; "+
			"the gid arm is reusing the SUBUID base, which newgidmap validates against /etc/subgid and rejects "+
			"on any host whose two allocations diverge", uidRoot, testSubBase, testSubGIDBase)
	}
	if want := ":0:" + strconv.Itoa(testSubBase) + ":1:"; uidRoot != want {
		t.Errorf("--uid-map namespace-root spec = %q, want %q (the /etc/subuid base)", uidRoot, want)
	}
	if want := ":0:" + strconv.Itoa(testSubGIDBase) + ":1:"; gidRoot != want {
		t.Errorf("--gid-map namespace-root spec = %q, want %q (the /etc/subgid base)", gidRoot, want)
	}
}

// TestVirtiofsdIDMapArgsUnmappedSpike pins the V2a carve-out: a zero agentUID
// (the spike harness, which shares a throwaway dir and asserts nothing about
// ownership) gets NO mapping at all, so that suite keeps booting unchanged.
func TestVirtiofsdIDMapArgsUnmappedSpike(t *testing.T) {
	if argv := virtiofsdIDMapArgs(0, testSubBase, testSubGIDBase); argv != nil {
		t.Fatalf("virtiofsdIDMapArgs(0) = %v, want nil (the V2a spike share stays unmapped)", argv)
	}
}

// TestVirtiofsdArgsDropsMknod pins the capability trim on the full argv: a
// workspace share has no legitimate device nodes, so CAP_MKNOD is dropped from
// the set virtiofsd retains as namespace-root — on EVERY boot, including the
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
		argv := virtiofsdArgs(cfg, testSubBase, testSubGIDBase)
		if !slices.Contains(argv, modcapsDropMknod) {
			t.Errorf("virtiofsdArgs(AgentUID=%d) = %v, want it to carry %q", agentUID, argv, modcapsDropMknod)
		}
		// The pre-existing flags must survive the argv restructure.
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

// TestVirtiofsdArgsIncludesMapping is the seam assertion the launch path depends
// on: the full argv carries the mapping for a real agent uid and none for the
// spike, so the restructure into a named local did not drop the id args — and it
// carries the gid base through to the gid arm, so the seam cannot collapse the
// two bases onto one on the way from launch to argv.
func TestVirtiofsdArgsIncludesMapping(t *testing.T) {
	mapped := virtiofsdArgs(BootConfig{AgentUID: 1000}, testSubBase, testSubGIDBase)
	if got := len(flagValues(mapped, "--uid-map")); got != 2 {
		t.Errorf("mapped argv %v carries %d --uid-map specs, want 2", mapped, got)
	}
	if got := flagValues(mapped, "--gid-map"); len(got) != 2 || !strings.Contains(got[0], strconv.Itoa(testSubGIDBase)) {
		t.Errorf("mapped argv %v --gid-map specs = %v, want 2 specs whose namespace-root arm carries the subgid base %d",
			mapped, got, testSubGIDBase)
	}
	spike := virtiofsdArgs(BootConfig{AgentUID: 0}, testSubBase, testSubGIDBase)
	if got := flagValues(spike, "--uid-map"); len(got) != 0 {
		t.Errorf("spike argv %v carries --uid-map specs %v, want none", spike, got)
	}
}

// TestParseSubordinateIDBase pins the subuid(5)/subgid(5) parse (one format,
// both files): the invoking user's entry is matched by NAME or by uid, comments
// and malformed/zero-count entries are skipped, and a user with no range is a
// named error rather than a silent fallback to a base the user does not own.
// The error must name the FILE it was parsing, so a missing subgid range is not
// misreported as a subuid problem.
func TestParseSubordinateIDBase(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		uid      int
		username string
		path     string
		want     int
		wantErr  []string
	}{
		{
			name:     "matched by username",
			content:  "root:100000:65536\nmattw:165536:65536\n",
			uid:      1000,
			username: "mattw",
			want:     165536,
		},
		{
			name:    "matched by uid when the name is unresolvable",
			content: "1000:200000:65536\n",
			uid:     1000,
			want:    200000,
		},
		{
			name:     "comments and blank lines are skipped",
			content:  "# subuid(5)\n\nmattw:100000:65536\n",
			uid:      1000,
			username: "mattw",
			want:     100000,
		},
		{
			name:     "a zero-count range is not trusted",
			content:  "mattw:100000:0\nmattw:300000:65536\n",
			uid:      1000,
			username: "mattw",
			want:     300000,
		},
		{
			name:     "a malformed entry is skipped, not parsed as a base",
			content:  "mattw:not-a-number:65536\nmattw:400000:1\n",
			uid:      1000,
			username: "mattw",
			want:     400000,
		},
		{
			name:     "no range for the user names the user, the file and the fix",
			content:  "root:100000:65536\nother:165536:65536\n",
			uid:      1000,
			username: "mattw",
			path:     subordinateUIDPath,
			wantErr: []string{
				"mattw", "/etc/subuid", "/etc/subgid",
				"usermod --add-subuids", "usermod --add-subgids", "newuidmap", "newgidmap",
			},
		},
		{
			name:     "a missing SUBGID range names the subgid file, not the subuid one",
			content:  "root:100000:65536\n",
			uid:      1000,
			username: "mattw",
			path:     subordinateGIDPath,
			wantErr:  []string{"mattw", "in /etc/subgid", "usermod --add-subgids", "newgidmap"},
		},
		{
			name:    "an empty file names the fix",
			content: "",
			uid:     1000,
			path:    subordinateUIDPath,
			wantErr: []string{"uid 1000", "/etc/subuid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSubordinateIDBase(strings.NewReader(tt.content), tt.uid, tt.username, tt.path)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("parseSubordinateIDBase = %d, want an error naming %v", got, tt.wantErr)
				}
				for _, part := range tt.wantErr {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("error %q does not name %q", err.Error(), part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSubordinateIDBase = %v, want base %d", err, tt.want)
			}
			if got != tt.want {
				t.Fatalf("parseSubordinateIDBase = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestSubordinateIDBasesResolvesBothFilesIndependently is HIGH-1's preflight
// axis: the two bases come from the two files SEPARATELY, so a host whose
// allocations diverge yields divergent bases rather than one base used twice.
//
// It drives fixture files through the path-injected core because this box cannot
// exhibit the bug: its /etc/subuid and /etc/subgid both read
// `mattw:100000:65536`, so a gid arm reusing the subuid base is observationally
// identical to a correct one here.
func TestSubordinateIDBasesResolvesBothFilesIndependently(t *testing.T) {
	dir := t.TempDir()
	// The owner field must match THIS process, since subordinateBase resolves
	// the invoking uid/username itself — the file contents are the only
	// injectable half.
	owner := strconv.Itoa(os.Getuid())
	uidPath := filepath.Join(dir, "subuid")
	gidPath := filepath.Join(dir, "subgid")
	if err := os.WriteFile(uidPath, []byte(owner+":100000:65536\n"), 0o600); err != nil {
		t.Fatalf("writing the subuid fixture: %v", err)
	}
	if err := os.WriteFile(gidPath, []byte(owner+":165536:65536\n"), 0o600); err != nil {
		t.Fatalf("writing the subgid fixture: %v", err)
	}

	uidBase, gidBase, err := subordinateIDBases(uidPath, gidPath)
	if err != nil {
		t.Fatalf("subordinateIDBases over divergent fixtures = %v, want both bases", err)
	}
	if uidBase != 100000 {
		t.Errorf("uid base = %d, want 100000 (from %s)", uidBase, uidPath)
	}
	if gidBase != 165536 {
		t.Errorf("gid base = %d, want 165536 (from %s); reading it from /etc/subuid instead is the divergence bug", gidBase, gidPath)
	}
	if uidBase == gidBase {
		t.Fatal("the two bases collapsed onto one value despite divergent fixture files; the gid axis is not read independently")
	}
}

// TestSubordinateIDBasesRefusesAMissingSubgidRange pins the OTHER half of the
// preflight fix: a host with a perfectly good /etc/subuid range and NO
// /etc/subgid range must fail at PREFLIGHT with both files and both usermod
// flags named — not at first boot, where newgidmap refuses the gid map AFTER
// virtiofsd has bound its socket and the operator sees only a vhost-user
// negotiation error.
func TestSubordinateIDBasesRefusesAMissingSubgidRange(t *testing.T) {
	dir := t.TempDir()
	owner := strconv.Itoa(os.Getuid())
	uidPath := filepath.Join(dir, "subuid")
	gidPath := filepath.Join(dir, "subgid")
	if err := os.WriteFile(uidPath, []byte(owner+":100000:65536\n"), 0o600); err != nil {
		t.Fatalf("writing the subuid fixture: %v", err)
	}
	// A subgid file that exists but allocates the invoking user nothing — the
	// `usermod --add-subuids`-only host.
	if err := os.WriteFile(gidPath, []byte("root:100000:65536\n"), 0o600); err != nil {
		t.Fatalf("writing the subgid fixture: %v", err)
	}

	_, _, err := subordinateIDBases(uidPath, gidPath)
	if err == nil {
		t.Fatal("subordinateIDBases = nil error with no subgid range for the invoking user; " +
			"a divergent host must fail PREFLIGHT, not at the first boot as an opaque virtiofsd death")
	}
	for _, part := range []string{
		gidPath, subordinateUIDPath, subordinateGIDPath,
		"usermod --add-subuids", "usermod --add-subgids", "newgidmap",
	} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not name %q", err.Error(), part)
		}
	}
}

// TestSubordinateIDBasesRefusesAnUnreadableFile pins the open-error arm on BOTH
// axes: an absent map file is a named refusal, never a fallback to the
// conventional base. The message must show its example range as an example
// (LOW-8) and name the matching --add-subgids alongside --add-subuids, since the
// two allocations are independent.
func TestSubordinateIDBasesRefusesAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	owner := strconv.Itoa(os.Getuid())
	present := filepath.Join(dir, "subuid")
	if err := os.WriteFile(present, []byte(owner+":100000:65536\n"), 0o600); err != nil {
		t.Fatalf("writing the subuid fixture: %v", err)
	}
	absent := filepath.Join(dir, "does-not-exist")

	for _, tt := range []struct{ name, uidPath, gidPath string }{
		{"an unreadable subuid file", absent, present},
		{"an unreadable subgid file", present, absent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := subordinateIDBases(tt.uidPath, tt.gidPath)
			if err == nil {
				t.Fatal("subordinateIDBases = nil error over an absent map file; an unreadable range must refuse, never fall back")
			}
			for _, part := range []string{absent, "usermod --add-subuids", "usermod --add-subgids", "only the shadow-utils convention"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not name %q", err.Error(), part)
				}
			}
		})
	}
}
