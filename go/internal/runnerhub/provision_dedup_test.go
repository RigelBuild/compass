//go:build unix

package runnerhub

// Hub-level Provision idempotency (OQ6/SEA-1243): Hub.Provision derives the
// command router's dedup id from the caller's client_request_id, scoped to the
// workspace identity (agent account + repo + ref). Two Provisions with the SAME
// id AND the same workspace must JOIN one in-flight command (a timeout-retry
// returns the one container, never a duplicate); the SAME id reused for a
// DIFFERENT workspace must NOT join (each provisions its own container — the
// cross-account boundary); an EMPTY id must mint a fresh id per call (no dedup).
//
// These drive Hub.Provision (the real requestID→provisionDedupID→dispatch path),
// with a recording send standing in for the Runner's Sessions stream. Under
// testing/synctest, synctest.Wait() blocks until every dispatch goroutine is
// DURABLY blocked in waitCall — so "the second caller joined before we
// completed" is observed state, never a sleep and never a retry.

import (
	"context"
	"testing"
	"testing/synctest"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// provisionCommands returns the Provision commands the recording send captured,
// so a test asserts exactly how many Provision commands reached the Runner (and
// that they are Provisions, not some other command variant).
func provisionCommands(s *recordingSend) []*compassv1internal.SessionsResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*compassv1internal.SessionsResponse
	for _, c := range s.sent {
		if c.GetProvision() != nil {
			out = append(out, c)
		}
	}
	return out
}

// provisionOutcome is one caller's Hub.Provision result.
type provisionOutcome struct {
	resp *compassv1.ProvisionAgentWorkspaceResponse
	err  error
}

// enrollAttached enrolls a Runner, binds the given send as its live Sessions
// stream, and returns the live router — the minimal harness to drive
// Hub.Provision through the real router.
func enrollAttached(t *testing.T, hub *Hub, send *recordingSend) *commandRouter {
	t.Helper()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want the live router", err)
	}
	router.attach(send.send)
	return router
}

// A retry with the SAME non-empty client_request_id joins the one in-flight
// Provision: exactly ONE Provision command reaches the Runner and BOTH callers
// return the SAME container name. A bug that dropped the id from the dispatch key
// (or failed to thread requestID) would push a second Provision — a duplicate
// container — reddening the count assertion.
func TestProvisionSameClientRequestIdDedups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub := newHubOnly()
		send := newRecordingSend()
		router := enrollAttached(t, hub, send)
		defer router.detach(errStreamClosed)

		const id = "req-1"
		// A fully-specified workspace: the dedup id now binds to the agent
		// account, so both callers must send the identical request for the retry
		// to join. Same id + same account = one derived dedup id = one command.
		req := &compassv1.ProvisionAgentWorkspaceRequest{ClientRequestId: id, AgentHandle: "0123456789abcdef0123456789abcdef"}
		outcomes := make(chan provisionOutcome, 2)
		call := func() {
			resp, _, err := hub.Provision(context.Background(), id, req)
			outcomes <- provisionOutcome{resp, err}
		}

		// First caller: now durably blocked in waitCall, its Provision pushed once.
		go call()
		synctest.Wait()
		cmds := provisionCommands(send)
		if len(cmds) != 1 {
			t.Fatalf("after first Provision, Provision commands = %d, want 1", len(cmds))
		}

		// Second caller, same id + same workspace: durably blocked too — it must
		// have JOINED the in-flight call, not pushed a second Provision.
		go call()
		synctest.Wait()
		if got := len(provisionCommands(send)); got != 1 {
			t.Fatalf("second same-id Provision pushed another command (Provision commands = %d, want 1); idempotent join broken — a duplicate container", got)
		}

		// Answer the one in-flight command by its DERIVED dedup id (the scoped id
		// the hub actually sent, not the raw client_request_id); both callers
		// unblock with it.
		router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmds[0].GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Provision{
				Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: "container-42"},
			},
		})
		synctest.Wait()

		for i := range 2 {
			o := <-outcomes
			if o.err != nil {
				t.Fatalf("caller %d Provision err = %v, want nil", i, o.err)
			}
			if got := o.resp.GetContainerName(); got != "container-42" {
				t.Fatalf("caller %d container name = %q, want container-42 (both same-id callers see the one container)", i, got)
			}
		}
	})
}

// The contrast: an EMPTY client_request_id does NOT dedup. Two Provisions with
// an empty id each mint a fresh correlation id and each dispatch, so TWO
// Provision commands reach the Runner. This proves dedup is keyed on the id
// rather than blanket-collapsing every Provision — the guard that a stable id is
// what buys idempotency, not the command type.
func TestProvisionEmptyClientRequestIdDoesNotDedup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub := newHubOnly()
		send := newRecordingSend()
		router := enrollAttached(t, hub, send)
		defer router.detach(errStreamClosed)

		req := &compassv1.ProvisionAgentWorkspaceRequest{}
		outcomes := make(chan provisionOutcome, 2)
		call := func() {
			resp, _, err := hub.Provision(context.Background(), "", req)
			outcomes <- provisionOutcome{resp, err}
		}

		go call()
		go call()
		// Both callers durably blocked in waitCall — with empty ids neither
		// joined the other, so both Provisions were pushed.
		synctest.Wait()

		cmds := provisionCommands(send)
		if len(cmds) != 2 {
			t.Fatalf("two empty-id Provisions pushed %d commands, want 2 (empty id must mint a fresh id per call, not dedup)", len(cmds))
		}
		ids := send.ids()
		if len(ids) != 2 {
			t.Fatalf("two empty-id Provisions used %d distinct request ids, want 2 (each empty id mints a fresh correlation id)", len(ids))
		}

		// Complete each by its own minted id so both callers unblock.
		for id := range ids {
			router.complete(&compassv1internal.SessionsRequest{
				RequestId: id,
				Result: &compassv1internal.SessionsRequest_Provision{
					Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: "container-" + id},
				},
			})
		}
		synctest.Wait()

		for i := range 2 {
			o := <-outcomes
			if o.err != nil {
				t.Fatalf("caller %d Provision err = %v, want nil", i, o.err)
			}
			if o.resp.GetContainerName() == "" {
				t.Fatalf("caller %d container name empty, want a per-call container", i)
			}
		}
	})
}

// The security regression (SEA-1243): a client_request_id reused across DIFFERENT
// workspaces must NOT dedup. client_request_id is a client-chosen string; if it
// were the sole dedup key, two provisions sharing one value for different agent
// accounts would join — the second caller would be handed a container
// provisioned for the FIRST caller's account (a cross-account boundary break).
// The dedup id binds to the agent account, so a
// reused id with a different account derives a distinct id, dispatches its own
// Provision, and each caller gets its own container. A regression that keyed on
// the raw client_request_id alone would push ONE command and hand both callers
// the same container — reddening the count and the per-caller container asserts.
func TestProvisionSameIdDifferentAccountDoesNotDedup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub := newHubOnly()
		send := newRecordingSend()
		router := enrollAttached(t, hub, send)
		defer router.detach(errStreamClosed)

		const id = "dup" // the SAME client_request_id for both callers
		reqA := &compassv1.ProvisionAgentWorkspaceRequest{ClientRequestId: id, AgentHandle: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		// different account, same id:
		reqB := &compassv1.ProvisionAgentWorkspaceRequest{ClientRequestId: id, AgentHandle: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
		outcomes := make(chan provisionOutcome, 2)
		call := func(req *compassv1.ProvisionAgentWorkspaceRequest) {
			resp, _, err := hub.Provision(context.Background(), id, req)
			outcomes <- provisionOutcome{resp, err}
		}

		go call(reqA)
		go call(reqB)
		// Both durably blocked: same id but different accounts must derive
		// distinct dedup ids, so NEITHER joined the other.
		synctest.Wait()

		cmds := provisionCommands(send)
		if len(cmds) != 2 {
			t.Fatalf("same id + different account pushed %d Provision commands, want 2 (a reused id must NOT dedup across accounts — the cross-account join is the bug)", len(cmds))
		}
		ids := send.ids()
		if len(ids) != 2 {
			t.Fatalf("same id + different account used %d distinct dedup ids, want 2 (the id must be scoped by workspace identity, not the raw client_request_id)", len(ids))
		}

		// Answer each by its own derived id with an account-distinct container.
		containers := map[string]string{}
		for _, c := range cmds {
			rid := c.GetRequestId()
			container := "container-for-" + c.GetProvision().GetAgentHandle()
			containers[rid] = container
			router.complete(&compassv1internal.SessionsRequest{
				RequestId: rid,
				Result: &compassv1internal.SessionsRequest_Provision{
					Provision: &compassv1.ProvisionAgentWorkspaceResponse{ContainerName: container},
				},
			})
		}
		synctest.Wait()

		// Each caller must receive its OWN account's container, never the other's.
		got := map[string]bool{}
		for range 2 {
			o := <-outcomes
			if o.err != nil {
				t.Fatalf("Provision err = %v, want nil", o.err)
			}
			got[o.resp.GetContainerName()] = true
		}
		if !got["container-for-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] || !got["container-for-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"] {
			t.Fatalf("callers received containers %v, want both container-for-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa and container-for-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb (a cross-account join would hand both the same container)", got)
		}
	})
}
