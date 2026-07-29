//go:build unix

// The Server-facing command surface: the CompassService session RPCs
// (ProvisionAgentWorkspace, Start/Stop/Reload/Status) route through here. Each
// mints a request id, dispatches the command to the owning Runner over the
// Sessions relay, waits for the correlated result, and maps a RunnerError to the
// Connect status the client sees. This is where the OQ6 request-id lives on the
// Server side: a caller that supplies a client_request_id reuses it so a
// timeout-retry dedupes to the original result (no duplicate container).
package runnerhub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// Provision relays a ProvisionAgentWorkspace command to the owning Runner and
// returns the container name it created plus the id of the Runner that served
// the call. The Runner id is returned rather than looked up afterwards so the
// caller records the agent's durable placement against the Runner that ACTUALLY
// ran the provision — re-reading the registry after the round trip could name a
// different Runner if one re-enrolled in the meantime, and a placement pointing
// at the wrong Runner is worse than none (reattach would re-drive the wrong
// set). requestID is the client_request_id idempotency key: a timeout-retry with
// the same id returns the same container (no duplicate); empty mints a fresh id
// (no dedup).
//
// The client_request_id alone must NOT be the dedup key: it is a client-chosen
// string, and two provisions that reuse one value for DIFFERENT workspaces
// (distinct account, repo, or ref) are distinct operations — joining them would
// hand the second caller a container provisioned for the first's account. So a
// non-empty id is scoped to the request's workspace identity (account + repo +
// ref): a genuine retry resends the identical request and dedups, while a reused
// id with different inputs derives a distinct correlation id and does not join.
// This mirrors the comms store's (author_account_id, client_request_id)
// idempotency scoping (store/migrations/0001_init.sql), strengthened to the full
// provision identity since a provision creates a real isolated container.
func (h *Hub) Provision(ctx context.Context, requestID string, req *compassv1.ProvisionAgentWorkspaceRequest) (*compassv1.ProvisionAgentWorkspaceResponse, string, error) {
	result, runnerID, err := h.relay(ctx, "", &compassv1internal.SessionsResponse{
		RequestId: provisionDedupID(requestID, req),
		Command:   &compassv1internal.SessionsResponse_Provision{Provision: req},
	})
	if err != nil {
		return nil, "", err
	}
	resp := result.GetProvision()
	// Record which agent account this container was provisioned for, so a later
	// Start can promote it to a session binding RelayCommsCall resolves against
	// (comms-tools design T2). The Runner never asserts this account; it is the
	// Server's own record, keyed by the container name the Runner returned. This
	// binding is the LIVE comms binding only, cleared on re-enroll; the DURABLE
	// container/Runner placement is the caller's store write.
	h.bindContainer(resp.GetContainerName(), store.AccountID(req.GetAgentAccountId()))
	return resp, runnerID, nil
}

// Start relays a StartAgentSession command to the owning Runner.
func (h *Hub) Start(ctx context.Context, requestID string, req *compassv1.StartAgentSessionRequest) (*compassv1.StartAgentSessionResponse, error) {
	result, _, err := h.relay(ctx, req.GetContainerName(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Start{Start: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetStart()
	// Promote the container's provisioned account binding onto the live session
	// id the Runner minted, so RelayCommsCall for this session resolves the
	// agent account (comms-tools design T2). A container with no recorded
	// account leaves no session binding, and its comms calls fail closed.
	h.promoteSession(req.GetContainerName(), resp.GetSessionId())
	return resp, nil
}

// Stop relays a StopAgentSession command to the owning Runner.
func (h *Hub) Stop(ctx context.Context, requestID string, req *compassv1.StopAgentSessionRequest) (*compassv1.StopAgentSessionResponse, error) {
	result, _, err := h.relay(ctx, req.GetSessionId(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Stop{Stop: req},
	})
	if err != nil {
		return nil, err
	}
	// Drop the session's account binding: a RelayCommsCall for a stopped session
	// fails closed CodeNotFound, the same answer as a never-seen session — never
	// a stale reuse.
	h.unbindSession(req.GetSessionId())
	return result.GetStop(), nil
}

// Reload relays a ReloadAgentSession command to the owning Runner.
func (h *Hub) Reload(ctx context.Context, requestID string, req *compassv1.ReloadAgentSessionRequest) (*compassv1.ReloadAgentSessionResponse, error) {
	result, _, err := h.relay(ctx, req.GetSessionId(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Reload{Reload: req},
	})
	if err != nil {
		return nil, err
	}
	return result.GetReload(), nil
}

// Status relays a GetAgentStatus command to the owning Runner — the Runner is
// authoritative for live session truth, so the Server reconciles to its answer.
func (h *Hub) Status(ctx context.Context, requestID string, req *compassv1.GetAgentStatusRequest) (*compassv1.GetAgentStatusResponse, error) {
	result, _, err := h.relay(ctx, req.GetSessionId(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Status{Status: req},
	})
	if err != nil {
		return nil, err
	}
	return result.GetStatus(), nil
}

// relay dispatches one built command through the owning Runner's router and maps
// the outcome to a Connect status: a RunnerError result becomes the mapped
// Connect code; a transport failure (no Runner, stream drop) becomes
// Unavailable. sessionKey selects the owning Runner (single-Runner MVP: any
// non-empty key resolves the one Runner). The served Runner's id is returned
// alongside the result for the one caller that must attribute the command to a
// Runner (Provision, recording a durable placement); the rest discard it.
func (h *Hub) relay(ctx context.Context, sessionKey string, cmd *compassv1internal.SessionsResponse) (*compassv1internal.SessionsRequest, string, error) {
	router, runnerID, err := h.routerFor(sessionKey)
	if err != nil {
		return nil, "", connect.NewError(connect.CodeUnavailable, err)
	}
	result, err := router.dispatch(ctx, cmd)
	if err != nil {
		return nil, "", connect.NewError(connect.CodeUnavailable, err)
	}
	if runnerErr := result.GetError(); runnerErr != nil {
		return nil, "", runnerErrorToConnect(runnerErr)
	}
	return result, runnerID, nil
}

// runnerErrorToConnect maps a RunnerError to the Connect status the client sees.
func runnerErrorToConnect(e *compassv1internal.RunnerError) error {
	var code connect.Code
	switch e.GetCode() {
	case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING:
		code = connect.CodeAlreadyExists
	case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND:
		code = connect.CodeNotFound
	default:
		code = connect.CodeInternal
	}
	return connect.NewError(code, fmt.Errorf("runner: %s", e.GetMessage()))
}

// orNewRequestID returns id when non-empty, else a fresh random correlation id.
func orNewRequestID(id string) string {
	if id != "" {
		return id
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// provisionDedupID derives the correlation/dedup id for a provision. An empty
// client_request_id mints a fresh random id (no dedup, per the contract). A
// non-empty id is bound to the workspace identity — the agent account, the repo
// source (variant + value), and the ref — so a retry of the SAME provision
// dedups while the same client_request_id reused for a DIFFERENT workspace
// derives a distinct id and does not join. The derivation is a domain-separated
// SHA-256 over length-prefixed fields, so no field's value can be shifted into
// another to forge a collision.
func provisionDedupID(clientRequestID string, req *compassv1.ProvisionAgentWorkspaceRequest) string {
	if clientRequestID == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
	repoKind, repoValue := provisionRepoKey(req)
	h := sha256.New()
	for _, field := range []string{
		"compass.provision.v1", // domain separator
		clientRequestID,
		req.GetAgentAccountId(),
		repoKind,
		repoValue,
		req.GetRef(),
	} {
		var lp [8]byte
		binary.BigEndian.PutUint64(lp[:], uint64(len(field)))
		h.Write(lp[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// provisionRepoKey returns a stable (kind, value) pair for the request's repo
// oneof, so the dedup id binds to which repo source was requested. An unset
// oneof yields empty strings (BuildSpec rejects it downstream); the kind tag
// keeps a remote_url and a local_path of the same string distinct.
func provisionRepoKey(req *compassv1.ProvisionAgentWorkspaceRequest) (kind, value string) {
	switch r := req.GetRepo().(type) {
	case *compassv1.ProvisionAgentWorkspaceRequest_RemoteUrl:
		return "remote_url", r.RemoteUrl
	case *compassv1.ProvisionAgentWorkspaceRequest_LocalPath:
		return "local_path", r.LocalPath
	default:
		return "", ""
	}
}
