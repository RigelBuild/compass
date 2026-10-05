//go:build unix

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRunPeerCommands(t *testing.T) {
	fake := &fakeComms{}
	client := startFakeCommsServer(t, fake)

	var approveOut strings.Builder
	if err := runPeerApprove(context.Background(), client, "alice", &approveOut); err != nil {
		t.Fatalf("runPeerApprove: %v", err)
	}
	if fake.peerApprove == nil || fake.peerApprove.GetPeerHandle() != "alice" {
		t.Fatalf("ApprovePeer request = %+v, want peer_handle alice", fake.peerApprove)
	}
	if got, want := approveOut.String(), "alice\tpending_outgoing\n"; got != want {
		t.Errorf("approve output = %q, want %q", got, want)
	}

	var revokeOut strings.Builder
	if err := runPeerRevoke(context.Background(), client, "carol", &revokeOut); err != nil {
		t.Fatalf("runPeerRevoke: %v", err)
	}
	if fake.peerRevoke == nil || fake.peerRevoke.GetPeerHandle() != "carol" {
		t.Fatalf("RevokePeer request = %+v, want peer_handle carol", fake.peerRevoke)
	}
	if got, want := revokeOut.String(), "no approval for carol\n"; got != want {
		t.Errorf("revoke output = %q, want %q", got, want)
	}

	fake.revokeHit = true
	revokeOut.Reset()
	if err := runPeerRevoke(context.Background(), client, "carol", &revokeOut); err != nil {
		t.Fatalf("runPeerRevoke: %v", err)
	}
	if got, want := revokeOut.String(), "revoked carol\n"; got != want {
		t.Errorf("revoke output = %q, want %q", got, want)
	}

	var listOut strings.Builder
	if err := runPeerList(context.Background(), client, &listOut); err != nil {
		t.Fatalf("runPeerList: %v", err)
	}
	if fake.peerList == nil {
		t.Fatal("ListPeers was not called")
	}
	if got, want := listOut.String(), "alice\tpending_outgoing\nbob\tapproved\n"; got != want {
		t.Errorf("list output = %q, want %q", got, want)
	}
	if len(fake.peerAuth) != 4 {
		t.Fatalf("authorization headers = %v, want one per RPC", fake.peerAuth)
	}
	for _, got := range fake.peerAuth {
		if got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
	}
}

func TestPeerCommandRegistered(t *testing.T) {
	var peer *cobra.Command
	for _, cmd := range newRootCmd().Commands() {
		if cmd.Name() == "peer" {
			peer = cmd
		}
	}
	if peer == nil {
		t.Fatal("newRootCmd does not register peer")
	}
	for _, tc := range []struct {
		verb    string
		okArgs  []string
		badArgs []string
	}{
		{"approve", []string{"alice"}, []string{}},
		{"revoke", []string{"alice"}, []string{"alice", "bob"}},
		{"list", []string{}, []string{"alice"}},
	} {
		cmd, _, err := peer.Find([]string{tc.verb})
		if err != nil || cmd.Name() != tc.verb {
			t.Fatalf("peer %s: Find = %v, %v", tc.verb, cmd, err)
		}
		if err := cmd.Args(cmd, tc.okArgs); err != nil {
			t.Errorf("peer %s %v rejected: %v", tc.verb, tc.okArgs, err)
		}
		if err := cmd.Args(cmd, tc.badArgs); err == nil {
			t.Errorf("peer %s %v accepted, want an argument error", tc.verb, tc.badArgs)
		}
	}
}
