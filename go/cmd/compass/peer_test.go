//go:build unix

package main

import (
	"context"
	"strings"
	"testing"
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
	root := newRootCmd()
	var found bool
	for _, cmd := range root.Commands() {
		if cmd.Name() == "peer" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("newRootCmd does not register peer")
	}
	cmd, _, err := newPeerCmd().Find([]string{"approve"})
	if err != nil || cmd == nil || cmd.Use != "approve <handle>" {
		t.Fatalf("peer approve command = %v, %v, want approve <handle>", cmd, err)
	}
}
