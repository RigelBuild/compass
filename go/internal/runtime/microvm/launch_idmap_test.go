//go:build unix

package microvm

// Hermetic argv assertions for virtiofsd's id mapping and capability trim
// (record §(d) host-ownership parity), following launch_cmdline_test.go's shape:
// no VMM boots, no daemon spawns — the exact flag/value pairs are the contract,
// because a wrong pair fails at first boot as an opaque daemon-socket timeout
// rather than as a legible error.
//
// The subordinate base is injected as a fixed value rather than read from the
// test box's /etc/subuid, which is the whole point of the parse/spawn split in
// subuid.go: the mapping is pinned identically on a host allocated 100000 and
// one allocated 165536.

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// testSubBase is a deliberately NON-conventional subordinate base: 100000 would
// pass even against the old hardcode, so the assertions below would not detect a
// regression back to it.
const testSubBase = 165536

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
// uid. The gid arm is the discriminating one: its host side must be os.Getgid(),
// NOT os.Getuid() — a host user's gid is routinely different from its uid (e.g.
// 1000:100), and collapsing gid onto uid is precisely the host-ownership parity
// break the KVM parity leg detects.
func TestVirtiofsdIDMapArgsExactSpecs(t *testing.T) {
	const agentUID = 1000
	argv := virtiofsdIDMapArgs(agentUID, testSubBase)

	base := strconv.Itoa(testSubBase)
	agent := strconv.Itoa(agentUID)
	wantUID := []string{":0:" + base + ":1:", ":" + agent + ":" + strconv.Itoa(os.Getuid()) + ":1:"}
	wantGID := []string{":0:" + base + ":1:", ":" + agent + ":" + strconv.Itoa(os.Getgid()) + ":1:"}

	if got := flagValues(argv, "--uid-map"); !slices.Equal(got, wantUID) {
		t.Errorf("--uid-map specs = %v, want %v (argv %v)", got, wantUID, argv)
	}
	if got := flagValues(argv, "--gid-map"); !slices.Equal(got, wantGID) {
		t.Errorf("--gid-map specs = %v, want %v (argv %v)", got, wantGID, argv)
	}
	// The subordinate base must come from the injected value, never from a
	// hardcoded 100000 — the correctness bug on any host whose /etc/subuid
	// range starts elsewhere.
	for _, spec := range append(flagValues(argv, "--uid-map"), flagValues(argv, "--gid-map")...) {
		if strings.Contains(spec, ":100000:") && testSubBase != 100000 {
			t.Errorf("spec %q carries a hardcoded 100000 base; the subordinate base must be the parsed one (%d)", spec, testSubBase)
		}
	}
}

// TestVirtiofsdIDMapArgsUnmappedSpike pins the V2a carve-out: a zero agentUID
// (the spike harness, which shares a throwaway dir and asserts nothing about
// ownership) gets NO mapping at all, so that suite keeps booting unchanged.
func TestVirtiofsdIDMapArgsUnmappedSpike(t *testing.T) {
	if argv := virtiofsdIDMapArgs(0, testSubBase); argv != nil {
		t.Fatalf("virtiofsdIDMapArgs(0) = %v, want nil (the V2a spike share stays unmapped)", argv)
	}
}

// TestVirtiofsdArgsDropsMknod pins the capability trim on the full argv: a
// workspace share has no legitimate device nodes, so CAP_MKNOD is dropped from
// the set virtiofsd retains as namespace-root — on EVERY boot, including the
// unmapped spike, since the flag costs nothing there.
func TestVirtiofsdArgsDropsMknod(t *testing.T) {
	for _, agentUID := range []uint32{0, 1000} {
		cfg := BootConfig{
			FSSocket:    "/tmp/cvm/virtiofsd.sock",
			FSSharedDir: "/tmp/cvm/share",
			AgentUID:    agentUID,
		}
		argv := virtiofsdArgs(cfg, testSubBase)
		if !slices.Contains(argv, "--modcaps=-mknod") {
			t.Errorf("virtiofsdArgs(AgentUID=%d) = %v, want it to carry --modcaps=-mknod", agentUID, argv)
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
// spike, so the restructure into a named local did not drop the id args.
func TestVirtiofsdArgsIncludesMapping(t *testing.T) {
	mapped := virtiofsdArgs(BootConfig{AgentUID: 1000}, testSubBase)
	if got := len(flagValues(mapped, "--uid-map")); got != 2 {
		t.Errorf("mapped argv %v carries %d --uid-map specs, want 2", mapped, got)
	}
	spike := virtiofsdArgs(BootConfig{AgentUID: 0}, testSubBase)
	if got := flagValues(spike, "--uid-map"); len(got) != 0 {
		t.Errorf("spike argv %v carries --uid-map specs %v, want none", spike, got)
	}
}

// TestParseSubordinateIDBase pins the subuid(5) parse: the invoking user's entry
// is matched by NAME or by uid, comments and malformed/zero-count entries are
// skipped, and a user with no range is a named error rather than a silent
// fallback to a base the user does not own.
func TestParseSubordinateIDBase(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		uid      int
		username string
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
			name:     "no range for the user names the user and the fix",
			content:  "root:100000:65536\nother:165536:65536\n",
			uid:      1000,
			username: "mattw",
			wantErr:  []string{"mattw", "/etc/subuid", "usermod --add-subuids", "newuidmap"},
		},
		{
			name:    "an empty file names the fix",
			content: "",
			uid:     1000,
			wantErr: []string{"uid 1000", "/etc/subuid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSubordinateIDBase(strings.NewReader(tt.content), tt.uid, tt.username)
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
