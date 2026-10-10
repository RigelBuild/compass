//go:build pgtest

package comms

// The Comms.OpenDM handler + OpenDMAsAccount adapter: resolve-or-create DMs
// for same-owner and mutually peered agents, with deterministic names and
// post-commit ChannelChanged on create. In-process with a real store and bus.
import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestOpenDMSameOwnerCreatesDMChannel: an agent opens a DM with a same-owner
// peer → the returned channel is a real DM (kind=DM, mandatory subscription,
// both parties members), created=true, and the deterministic sorted-handle name
// (dm:<lo>:<hi>) is used.
func TestOpenDMSameOwnerCreatesDMChannel(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	bob := mustAgent(t, st, owner.ID, "bob")

	resp, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM = %v, want success", err)
	}
	if !resp.Msg.GetCreated() {
		t.Fatalf("created = false, want true on a first open")
	}
	ch := resp.Msg.GetChannel()
	if ch.GetName() != "dm:alice:bob" {
		t.Fatalf("channel name = %q, want dm:alice:bob (deterministic sorted-handle)", ch.GetName())
	}
	if ch.GetKind() != compassv1.ChannelKind_CHANNEL_KIND_DM {
		t.Fatalf("channel kind = %v, want CHANNEL_KIND_DM", ch.GetKind())
	}
	if !ch.GetMandatorySubscription() {
		t.Fatalf("mandatory_subscription = false, want true (born-mandatory DM)")
	}
	if !containsString(ch.GetMemberAccountIds(), string(alice.ID)) || !containsString(ch.GetMemberAccountIds(), string(bob.ID)) {
		t.Fatalf("members = %v, want both alice %s and bob %s", ch.GetMemberAccountIds(), alice.ID, bob.ID)
	}
	groups, err := st.ListChannelGroups(ctx, owner.ID)
	if err != nil {
		t.Fatalf("ListChannelGroups(owner): %v", err)
	}
	if !slices.ContainsFunc(groups, func(group store.ChannelGroup) bool {
		return group.ID == store.ChannelGroupID(ch.GetGroupId()) && group.Name == "__dm__" && group.OwnerUserID == owner.ID
	}) {
		t.Fatalf("same-owner DM group = %q; want caller owner's reserved __dm__ group", ch.GetGroupId())
	}
}

// TestOpenDMRevokeWaitsForLockedPeering keeps a stale authorization from creating a DM.
func TestOpenDMRevokeWaitsForLockedPeering(t *testing.T) {
	svc, st := newHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ownerA := mustUser(t, st, "a")
	ownerB := mustUser(t, st, "b")
	agentA := mustAgent(t, st, ownerA.ID, "a")
	mustAgent(t, st, ownerA.ID, "x")
	mustAgent(t, st, ownerB.ID, "b")
	agentBY := mustAgent(t, st, ownerB.ID, "y")
	for _, edge := range [][2]store.AccountID{{ownerA.ID, ownerB.ID}, {ownerB.ID, ownerA.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	peerHandle := "b/y"
	_, unknownErr := svc.OpenDM(WithActor(ctx, agentA.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "ghost"}))
	connectNotFoundFor(t, unknownErr, "ghost", "OpenDM(unknown handle)")

	openDone := make(chan error, 1)
	lockWaitObserved := make(chan struct{})
	finishTx := make(chan bool, 1)
	finished := false
	defer func() {
		if !finished {
			select {
			case finishTx <- false:
			default:
			}
		}
	}()
	txReady := make(chan struct{})
	txDone := make(chan error, 1)
	observedWait := false
	go func() {
		txDone <- st.WithTx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM user_peers WHERE user_id = $1 AND peer_user_id = $2`, string(ownerA.ID), string(ownerB.ID))
			if err != nil {
				return fmt.Errorf("delete directed approval in test transaction: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("deleted %d directed approvals, want 1", tag.RowsAffected())
			}
			close(txReady)

			poll := time.NewTicker(10 * time.Millisecond)
			defer poll.Stop()
			for {
				var blocked bool
				if err := tx.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1
						FROM pg_locks waiting
						JOIN pg_locks deleting
						  ON deleting.pid = pg_backend_pid()
						WHERE NOT waiting.granted
						  AND waiting.locktype IN ('transactionid', 'tuple')
						  AND (waiting.locktype = 'transactionid' OR waiting.relation = 'user_peers'::regclass)
						  AND deleting.locktype = 'relation'
						  AND deleting.mode = 'RowExclusiveLock'
						  AND deleting.relation = 'user_peers'::regclass
						  AND pg_blocking_pids(waiting.pid) @> ARRAY[deleting.pid]
					)
				`).Scan(&blocked); err != nil {
					return fmt.Errorf("observe OpenDM peering lock wait: %w", err)
				}
				if blocked {
					observedWait = true
					close(lockWaitObserved)
					break
				}
				select {
				case openErr := <-openDone:
					return fmt.Errorf("OpenDM returned before waiting on the directed approval lock: %v", openErr)
				case <-poll.C:
				case <-ctx.Done():
					return fmt.Errorf("timed out waiting for OpenDM to block on the peering row lock: %w", ctx.Err())
				}
			}

			select {
			case commit := <-finishTx:
				if !commit {
					return errors.New("OpenDM peering lock test aborted")
				}
				return nil
			case <-ctx.Done():
				return fmt.Errorf("timed out waiting to finish peering lock transaction: %w", ctx.Err())
			}
		})
	}()

	select {
	case <-txReady:
	case err := <-txDone:
		t.Fatalf("begin directed-approval delete transaction: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for directed-approval delete transaction: %v", ctx.Err())
	}
	go func() {
		_, err := svc.OpenDM(WithActor(ctx, agentA.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: peerHandle}))
		openDone <- err
	}()

	select {
	case <-lockWaitObserved:
	case err := <-txDone:
		t.Fatalf("OpenDM did not wait on the directed approval row lock: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for OpenDM to observe the peering row lock: %v", ctx.Err())
	}
	finishTx <- true
	finished = true
	select {
	case err := <-txDone:
		if !observedWait {
			t.Fatalf("OpenDM did not wait on the directed approval row lock: %v", err)
		}
		if err != nil {
			t.Fatalf("peering lock transaction: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for the peering lock transaction to commit: %v", ctx.Err())
	}

	select {
	case openErr := <-openDone:
		connectNotFoundFor(t, openErr, peerHandle, "OpenDM after concurrent revoke")
		var unknownConnectErr, revokedConnectErr *connect.Error
		if !errors.As(unknownErr, &unknownConnectErr) || !errors.As(openErr, &revokedConnectErr) {
			t.Fatalf("OpenDM errors = (%v, %v), want Connect errors", unknownErr, openErr)
		}
		unknownMessage := strings.ReplaceAll(unknownConnectErr.Message(), "ghost", "<handle>")
		revokedMessage := strings.ReplaceAll(revokedConnectErr.Message(), peerHandle, "<handle>")
		if revokedMessage != unknownMessage {
			t.Fatalf("unknown error message = %q, revoked-peer message = %q; want byte-identical after handle substitution", unknownMessage, revokedMessage)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for OpenDM after peering transaction commit: %v", ctx.Err())
	}

	var dmChannels int64
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM channels WHERE name = $1", crossOwnerDMName(agentA.ID, agentBY.ID)).Scan(&dmChannels)
	}); err != nil {
		t.Fatalf("count cross-owner DM channels: %v", err)
	}
	if dmChannels != 0 {
		t.Fatalf("cross-owner DM channel count after revoked peering = %d, want 0", dmChannels)
	}
}

// TestOpenDMRevokeWaitsForOpenDMCommit gates OpenDM after its peering check and before its FK insert.
func TestOpenDMRevokeWaitsForOpenDMCommit(t *testing.T) {
	svc, st := newHandler(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	ownerA := mustUser(t, st, "open-first-owner-a")
	ownerB := mustUser(t, st, "open-first-owner-b")
	caller := mustAgent(t, st, ownerA.ID, "open-first-caller")
	mustAgent(t, st, ownerA.ID, "open-first-peer-x")
	mustAgent(t, st, ownerB.ID, "open-first-peer-b")
	peer := mustAgent(t, st, ownerB.ID, "open-first-peer-y")
	for _, edge := range [][2]store.AccountID{{ownerA.ID, ownerB.ID}, {ownerB.ID, ownerA.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	host := ownerA.ID
	if ownerB.ID < host {
		host = ownerB.ID
	}
	var groupID store.ChannelGroupID
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := store.LockOwnerDMTx(ctx, tx, host); err != nil {
			return err
		}
		var err error
		groupID, err = st.EnsureOwnerDMGroupTx(ctx, tx, host)
		return err
	}); err != nil {
		t.Fatalf("pre-create host DM group: %v", err)
	}

	type openResult struct {
		response *connect.Response[compassv1.OpenDMResponse]
		err      error
	}
	type revokeResult struct {
		response *connect.Response[compassv1.RevokePeerResponse]
		err      error
	}
	releaseHolder := make(chan struct{})
	holderReady := make(chan int, 1)
	holderDone := make(chan error, 1)
	holderFinished := make(chan struct{})
	openDone := make(chan openResult, 1)
	openFinished := make(chan struct{})
	revokeDone := make(chan revokeResult, 1)
	revokeFinished := make(chan struct{})
	holderStarted, openStarted, revokeStarted := false, false, false
	holderReleased := false
	defer func() {
		if !holderReleased {
			close(releaseHolder)
		}
		waitForFinish := func(operation string, started bool, finished <-chan struct{}) {
			if !started {
				return
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Errorf("waiting for %s cleanup: %v", operation, ctx.Err())
			}
		}
		waitForFinish("holder transaction", holderStarted, holderFinished)
		waitForFinish("OpenDM", openStarted, openFinished)
		waitForFinish("RevokePeer", revokeStarted, revokeFinished)
	}()

	go func() {
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			var locked int
			// The channel FK takes KEY SHARE, so FOR UPDATE blocks after authorization.
			if err := tx.QueryRow(ctx, "SELECT 1 FROM channel_groups WHERE id = $1 FOR UPDATE", string(groupID)).Scan(&locked); err != nil {
				return fmt.Errorf("lock host DM group: %w", err)
			}
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return fmt.Errorf("read holder backend pid: %w", err)
			}
			holderReady <- pid
			select {
			case <-releaseHolder:
				return nil
			case <-ctx.Done():
				return fmt.Errorf("wait to release group lock: %w", ctx.Err())
			}
		})
		holderDone <- err
		close(holderFinished)
	}()
	holderStarted = true
	var holderPID int
	select {
	case holderPID = <-holderReady:
	case err := <-holderDone:
		t.Fatalf("start host group lock transaction: %v", err)
	case <-ctx.Done():
		t.Fatalf("timed out acquiring host group lock: %v", ctx.Err())
	}

	waitForBlockedBy := func(blockerPID int, operation string, finished <-chan struct{}) (int, error) {
		poll := time.NewTicker(10 * time.Millisecond)
		defer poll.Stop()
		for {
			var pid int
			err := st.WithTx(ctx, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `
					SELECT waiting.pid
					FROM pg_locks waiting
					WHERE waiting.pid <> pg_backend_pid()
					  AND NOT waiting.granted
					  AND pg_blocking_pids(waiting.pid) @> ARRAY[$1::int]
					LIMIT 1
				`, blockerPID).Scan(&pid)
			})
			if err == nil {
				return pid, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return 0, fmt.Errorf("observe %s lock wait: %w", operation, err)
			}
			select {
			case <-finished:
				return 0, fmt.Errorf("%s completed before blocking on backend %d", operation, blockerPID)
			case <-poll.C:
			case <-ctx.Done():
				return 0, fmt.Errorf("timed out waiting for %s to block on backend %d: %w", operation, blockerPID, ctx.Err())
			}
		}
	}
	go func() {
		response, err := svc.OpenDM(WithActor(ctx, caller.ID), connect.NewRequest(&compassv1.OpenDMRequest{
			PeerHandle: ownerB.Handle + "/" + peer.Handle,
		}))
		openDone <- openResult{response: response, err: err}
		close(openFinished)
	}()
	openStarted = true
	openPID, err := waitForBlockedBy(holderPID, "OpenDM", openFinished)
	if err != nil {
		t.Fatalf("OpenDM did not wait on the host group lock: %v", err)
	}

	go func() {
		response, err := svc.RevokePeer(WithActor(ctx, ownerA.ID), connect.NewRequest(&compassv1.RevokePeerRequest{
			PeerHandle: ownerB.Handle,
		}))
		revokeDone <- revokeResult{response: response, err: err}
		close(revokeFinished)
	}()
	revokeStarted = true
	if _, err := waitForBlockedBy(openPID, "RevokePeer", revokeFinished); err != nil {
		t.Fatalf("RevokePeer did not wait on OpenDM's peering lock: %v", err)
	}

	close(releaseHolder)
	holderReleased = true
	select {
	case err := <-holderDone:
		if err != nil {
			t.Fatalf("release host group lock transaction: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("timed out committing host group lock transaction: %v", ctx.Err())
	}

	var opened openResult
	select {
	case opened = <-openDone:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for OpenDM: %v", ctx.Err())
	}
	if opened.err != nil {
		t.Fatalf("OpenDM while peered: %v", opened.err)
	}
	if opened.response == nil || opened.response.Msg.GetChannel() == nil {
		t.Fatal("OpenDM returned no channel")
	}
	wantName := crossOwnerDMName(caller.ID, peer.ID)
	if !opened.response.Msg.GetCreated() || opened.response.Msg.GetChannel().GetName() != wantName {
		t.Fatalf("OpenDM = created %v name %q, want created %v name %q", opened.response.Msg.GetCreated(), opened.response.Msg.GetChannel().GetName(), true, wantName)
	}

	var revoked revokeResult
	select {
	case revoked = <-revokeDone:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for RevokePeer after OpenDM: %v", ctx.Err())
	}
	if revoked.err != nil {
		t.Fatalf("RevokePeer after OpenDM commit: %v", revoked.err)
	}
	if revoked.response == nil || !revoked.response.Msg.GetDeleted() {
		t.Fatal("RevokePeer deleted = false, want true")
	}

	peered, err := st.OwnersPeered(ctx, ownerA.ID, ownerB.ID)
	if err != nil {
		t.Fatalf("check peering after revoke: %v", err)
	}
	if peered {
		t.Fatal("owners remain peered after RevokePeer")
	}
	channel, err := st.GetChannel(ctx, store.ChannelID(opened.response.Msg.GetChannel().GetId()))
	if err != nil {
		t.Fatalf("load DM after revoke: %v", err)
	}
	if channel.Kind != store.ChannelKindDM {
		t.Fatalf("DM after revoke kind = %v, want DM", channel.Kind)
	}
	if channel.Name != wantName {
		t.Fatalf("DM after revoke name = %q, want %q", channel.Name, wantName)
	}
	if !slices.Contains(channel.MemberAccountIDs, caller.ID) || !slices.Contains(channel.MemberAccountIDs, peer.ID) {
		t.Fatalf("DM after revoke members = %v, want both %q and %q", channel.MemberAccountIDs, caller.ID, peer.ID)
	}
}

// TestOpenDMReopenResumesSameChannel: a second open of the same pair — in EITHER
// handle order — resumes the SAME channel (created=false, same id), proving the
// deterministic name is order-independent and the upsert resolves the existing row.
func TestOpenDMReopenResumesSameChannel(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	bob := mustAgent(t, st, owner.ID, "bob")

	first, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM(alice->bob) = %v, want success", err)
	}
	if !first.Msg.GetCreated() {
		t.Fatalf("first open created = false, want true")
	}

	// Reverse order (bob opens with alice): same deterministic name, so it must
	// resume the same channel, not create a second.
	second, err := svc.OpenDM(WithActor(ctx, bob.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "alice"}))
	if err != nil {
		t.Fatalf("OpenDM(bob->alice) = %v, want success", err)
	}
	if second.Msg.GetCreated() {
		t.Fatalf("reopen created = true, want false (resume)")
	}
	if second.Msg.GetChannel().GetId() != first.Msg.GetChannel().GetId() {
		t.Fatalf("reopen channel id = %q, want the first open's %q", second.Msg.GetChannel().GetId(), first.Msg.GetChannel().GetId())
	}
}

// TestOpenDMUnknownHandleIsNotFound: an unknown peer handle collapses to
// CodeNotFound naming the submitted handle — the oracle-safe resolve miss.
func TestOpenDMUnknownHandleIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")

	_, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "ghost"}))
	connectNotFoundFor(t, err, "ghost", "OpenDM(unknown handle)")
}

// TestOpenDMCrossOwnerIsIndistinguishableNotFound: unpeered foreign handles get
// the exact message an unknown handle of the same shape gets, even with one approval.
func TestOpenDMCrossOwnerIsIndistinguishableNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	other := mustUser(t, st, "other")
	mustAgent(t, st, other.ID, "foreign")

	assertSameMiss := func() {
		t.Helper()
		for _, peer := range []string{"other/foreign", "other/ghost"} {
			_, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: peer}))
			connectNotFoundFor(t, err, peer, "OpenDM("+peer+")")
		}
	}
	assertSameMiss()
	if _, err := st.ApprovePeer(ctx, owner.ID, other.ID); err != nil {
		t.Fatalf("ApprovePeer(owner->other): %v", err)
	}
	assertSameMiss()
}

func TestOpenDMCrossOwnerMutualPeeringUsesStableNameAndHome(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	ownerA := mustUser(t, st, "dm-owner-a")
	ownerB := mustUser(t, st, "dm-owner-b")
	agentA := mustAgent(t, st, ownerA.ID, "agent-a")
	agentB := mustAgent(t, st, ownerB.ID, "agent-b")
	for _, edge := range [][2]store.AccountID{{ownerA.ID, ownerB.ID}, {ownerB.ID, ownerA.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	first, err := svc.OpenDM(WithActor(ctx, agentA.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "dm-owner-b/agent-b"}))
	if err != nil {
		t.Fatalf("OpenDM(agent-a->agent-b): %v", err)
	}
	channel := first.Msg.GetChannel()
	lo, hi := string(agentA.ID), string(agentB.ID)
	if lo > hi {
		lo, hi = hi, lo
	}
	if got, want := channel.GetName(), "xdm:"+lo+":"+hi; got != want {
		t.Fatalf("cross-owner DM name = %q, want %q", got, want)
	}
	if !first.Msg.GetCreated() || channel.GetKind() != compassv1.ChannelKind_CHANNEL_KIND_DM {
		t.Fatalf("first open = created %v kind %v, want created DM", first.Msg.GetCreated(), channel.GetKind())
	}
	host := ownerA.ID
	if ownerB.ID < host {
		host = ownerB.ID
	}
	groups, err := st.ListChannelGroups(ctx, host)
	if err != nil {
		t.Fatalf("ListChannelGroups(host): %v", err)
	}
	var dmGroup store.ChannelGroup
	for _, group := range groups {
		if group.Name == "__dm__" && group.OwnerUserID == host {
			dmGroup = group
			break
		}
	}
	if dmGroup.ID == "" || channel.GetGroupId() != string(dmGroup.ID) {
		t.Fatalf("cross-owner DM group = %q, want lower-owner %q __dm__ group %q", channel.GetGroupId(), host, dmGroup.ID)
	}
	for _, member := range []store.AccountID{agentA.ID, ownerA.ID, agentB.ID, ownerB.ID} {
		if !containsString(channel.GetMemberAccountIds(), string(member)) {
			t.Errorf("cross-owner DM members %v omit %q", channel.GetMemberAccountIds(), member)
		}
	}

	second, err := svc.OpenDM(WithActor(ctx, agentB.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "dm-owner-a/agent-a"}))
	if err != nil {
		t.Fatalf("OpenDM(agent-b->agent-a): %v", err)
	}
	if second.Msg.GetCreated() || second.Msg.GetChannel().GetId() != channel.GetId() {
		t.Fatalf("reverse open = created %v channel %q, want resume %q", second.Msg.GetCreated(), second.Msg.GetChannel().GetId(), channel.GetId())
	}
}

func TestOpenDMUserCallerMayAddressPeeredOwner(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	ownerA := mustUser(t, st, "dm-human-owner-a")
	ownerB := mustUser(t, st, "dm-human-owner-b")
	peer := mustAgent(t, st, ownerB.ID, "dm-human-peer")
	for _, edge := range [][2]store.AccountID{{ownerA.ID, ownerB.ID}, {ownerB.ID, ownerA.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	resp, err := svc.OpenDM(WithActor(ctx, ownerA.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "dm-human-owner-b/dm-human-peer"}))
	if err != nil {
		t.Fatalf("OpenDM(user->peered agent): %v", err)
	}
	if !resp.Msg.GetCreated() {
		t.Fatal("OpenDM(user->peered agent) created = false, want true")
	}
	for _, member := range []store.AccountID{ownerA.ID, ownerB.ID, peer.ID} {
		if !containsString(resp.Msg.GetChannel().GetMemberAccountIds(), string(member)) {
			t.Errorf("DM members %v omit %q", resp.Msg.GetChannel().GetMemberAccountIds(), member)
		}
	}
}

func TestOpenDMRejectsUnpeeredCoMemberAndRevokedPeer(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	ownerA := mustUser(t, st, "dm-revoke-owner-a")
	ownerB := mustUser(t, st, "dm-revoke-owner-b")
	agentA := mustAgent(t, st, ownerA.ID, "dm-revoke-agent-a")
	agentB := mustAgent(t, st, ownerB.ID, "dm-revoke-agent-b")
	agentB2 := mustAgent(t, st, ownerB.ID, "dm-revoke-agent-b2")
	shared, err := st.CreateChannel(ctx, ownerA.ID, store.NewChannel{
		Name: "dm-shared-room", Kind: store.ChannelKindGroupDM, MemberAccountIDs: []store.AccountID{agentA.ID, agentB.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(shared room): %v", err)
	}
	assertOpenNotFound := func(handle string) {
		t.Helper()
		_, err := svc.OpenDM(WithActor(ctx, agentA.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: handle}))
		connectCodeIs(t, err, connect.CodeNotFound, "OpenDM("+handle+")")
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Message() != notFoundFor(handle) {
			t.Fatalf("OpenDM(%q) error = %v, want %q", handle, err, notFoundFor(handle))
		}
	}
	resolved, err := svc.resolveAddressableAgent(ctx, agentA.ID, ownerB.Handle+"/"+agentB.Handle)
	if err != nil || resolved.ID != agentB.ID {
		t.Fatalf("resolveAddressableAgent(co-member) = (%q, %v); want %q", resolved.ID, err, agentB.ID)
	}

	assertOpenNotFound("dm-revoke-owner-b/dm-revoke-agent-b")

	for _, edge := range [][2]store.AccountID{{ownerA.ID, ownerB.ID}, {ownerB.ID, ownerA.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}
	opened, err := svc.OpenDM(WithActor(ctx, agentA.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "dm-revoke-owner-b/dm-revoke-agent-b"}))
	if err != nil {
		t.Fatalf("OpenDM after peering: %v", err)
	}
	if _, err := st.RevokePeer(ctx, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("RevokePeer: %v", err)
	}
	assertOpenNotFound(ownerB.Handle + "/" + agentB.Handle)
	assertOpenNotFound(ownerB.Handle + "/" + agentB2.Handle)

	for _, viewer := range []store.AccountID{ownerA.ID, ownerB.ID} {
		channels, err := st.ListChannels(ctx, viewer)
		if err != nil {
			t.Fatalf("ListChannels(%q): %v", viewer, err)
		}
		found := false
		for _, channel := range channels {
			if channel.ID == store.ChannelID(opened.Msg.GetChannel().GetId()) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ListChannels(%q) omits existing DM %q after revoke", viewer, opened.Msg.GetChannel().GetId())
		}
	}
	if !containsAccount(shared.MemberAccountIDs, agentB.ID) {
		t.Fatal("shared channel fixture does not contain agent B")
	}
}

func TestOpenDMCrossOwnerNameSurvivesHandleReclaim(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	ownerA := mustUser(t, st, "dm-reclaim-owner-a")
	ownerB := mustUser(t, st, "dm-reclaim-owner-b")
	agentA := mustAgent(t, st, ownerA.ID, "dm-reclaim-agent-a")
	agentB := mustAgent(t, st, ownerB.ID, "dm-reclaim-agent-b")
	for _, edge := range [][2]store.AccountID{{ownerA.ID, ownerB.ID}, {ownerB.ID, ownerA.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}
	oldDM, err := svc.OpenDM(WithActor(ctx, agentA.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "dm-reclaim-owner-b/dm-reclaim-agent-b"}))
	if err != nil {
		t.Fatalf("OpenDM before reclaim: %v", err)
	}
	oldMembers := append([]string(nil), oldDM.Msg.GetChannel().GetMemberAccountIds()...)

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(ownerA.ID), "dm-reclaim-owner-a-old")
		return err
	}); err != nil {
		t.Fatalf("rename owner handle: %v", err)
	}
	reclaimer, err := st.CreateUser(ctx, store.NewUser{Handle: "dm-reclaim-owner-a", DisplayName: "Reclaimed"})
	if err != nil {
		t.Fatalf("reclaim owner handle: %v", err)
	}
	reclaimedAgent := mustAgent(t, st, reclaimer.ID, "dm-reclaim-agent-a")
	for _, edge := range [][2]store.AccountID{{reclaimer.ID, ownerB.ID}, {ownerB.ID, reclaimer.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	newDM, err := svc.OpenDM(WithActor(ctx, reclaimedAgent.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "dm-reclaim-owner-b/dm-reclaim-agent-b"}))
	if err != nil {
		t.Fatalf("OpenDM after reclaim: %v", err)
	}
	if !newDM.Msg.GetCreated() || newDM.Msg.GetChannel().GetId() == oldDM.Msg.GetChannel().GetId() {
		t.Fatalf("reclaimed owner's open = created %v channel %q; want a new channel, not old %q", newDM.Msg.GetCreated(), newDM.Msg.GetChannel().GetId(), oldDM.Msg.GetChannel().GetId())
	}
	newMembers := newDM.Msg.GetChannel().GetMemberAccountIds()
	wantMembers := []string{string(reclaimedAgent.ID), string(reclaimer.ID), string(agentB.ID), string(ownerB.ID)}
	if !sameStringSet(newMembers, wantMembers) {
		t.Fatalf("new DM members = %v, want %v", newMembers, wantMembers)
	}
	after, err := st.GetChannel(ctx, store.ChannelID(oldDM.Msg.GetChannel().GetId()))
	if err != nil {
		t.Fatalf("GetChannel(old DM): %v", err)
	}
	if !sameStringSet(accountIDsToStrings(after.MemberAccountIDs), oldMembers) {
		t.Fatalf("old DM members after reclaim = %v, want unchanged %v", after.MemberAccountIDs, oldMembers)
	}
}

func accountIDsToStrings(ids []store.AccountID) []string {
	strings := make([]string, len(ids))
	for i, id := range ids {
		strings[i] = string(id)
	}
	return strings
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[string]int, len(want))
	for _, value := range want {
		counts[value]++
	}
	for _, value := range got {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

// TestOpenDMMalformedQualifierIsNotFound (OQ-7 grammar): a leading '/' or a
// nested '/' is a NOT_FOUND naming the submitted handle. "/bob" must never
// resolve as the bare same-owner peer bob.
//
// Mutation: branching on a non-empty Owner instead of the separator opens a DM
// with bob for "/bob", reddening the nil-error check.
func TestOpenDMMalformedQualifierIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	mustAgent(t, st, owner.ID, "bob")

	for _, peer := range []string{"/bob", "owner/bob/x", "owner/", "/"} {
		_, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: peer}))
		connectCodeIs(t, err, connect.CodeNotFound, "OpenDM("+peer+")")
		if ce, ok := errors.AsType[*connect.Error](err); !ok || ce.Message() != notFoundFor(peer) {
			t.Fatalf("OpenDM(%q) error = %v, want message %q naming the submitted handle", peer, err, notFoundFor(peer))
		}
	}
}

// TestOpenDMSelfIsInvalidArgument: a handle that resolves to the caller itself is
// not a peer — CodeInvalidArgument.
func TestOpenDMSelfIsInvalidArgument(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")

	_, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "alice"}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "OpenDM(self handle)")
}

// TestOpenDMAsAccountEmptyAccountIsNoActor: an empty account short-circuits to
// errNoActor (CodeInvalidArgument) before any handler work — the fail-closed
// guard mirroring the other AsAccount adapters.
func TestOpenDMAsAccountEmptyAccountIsNoActor(t *testing.T) {
	svc, _ := newHandler(t)
	_, err := svc.OpenDMAsAccount(context.Background(), "", &compassv1.OpenDMRequest{PeerHandle: "bob"})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "OpenDMAsAccount(empty account)")
}

// TestOpenDMEmitsChannelChangedOnCreate: a create fans a post-commit
// ChannelChanged carrying the new DM channel to a member's stream; a resume emits
// nothing new. Driven over the real stream (newStreamHarness), subscribing before
// the mutation. The caller (alice) is a member of the DM, so it drains the event.
func TestOpenDMEmitsChannelChangedOnCreate(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	alice := mustAgent(t, h.store, owner.ID, "alice")
	mustAgent(t, h.store, owner.ID, "bob")

	events := firstEventAfterBoundary(t, h, alice.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	resp, err := h.svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM: %v", err)
	}
	wantID := resp.Msg.GetChannel().GetId()

	got := awaitFirst(t, events)
	cc := got.GetChannelChanged()
	if cc == nil {
		t.Fatalf("event payload = %T, want ChannelChanged", got.GetPayload())
	}
	if cc.GetChannel().GetId() != wantID {
		t.Fatalf("ChannelChanged id = %q, want the created DM %q", cc.GetChannel().GetId(), wantID)
	}
	if cc.GetChannel().GetKind() != compassv1.ChannelKind_CHANNEL_KIND_DM {
		t.Fatalf("ChannelChanged kind = %v, want CHANNEL_KIND_DM", cc.GetChannel().GetKind())
	}
}

// TestOpenDMAsAccountResolvesOrCreates: the agent-tool adapter runs the same
// handler path under the bound account — a first call creates, a second resumes
// the same channel — parity with the direct handler.
func TestOpenDMAsAccountResolvesOrCreates(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	mustAgent(t, st, owner.ID, "bob")

	first, err := svc.OpenDMAsAccount(ctx, alice.ID, &compassv1.OpenDMRequest{PeerHandle: "bob"})
	if err != nil {
		t.Fatalf("OpenDMAsAccount(first) = %v, want success", err)
	}
	if !first.GetCreated() {
		t.Fatalf("first created = false, want true")
	}
	second, err := svc.OpenDMAsAccount(ctx, alice.ID, &compassv1.OpenDMRequest{PeerHandle: "bob"})
	if err != nil {
		t.Fatalf("OpenDMAsAccount(second) = %v, want success", err)
	}
	if second.GetCreated() {
		t.Fatalf("second created = true, want false (resume)")
	}
	if second.GetChannel().GetId() != first.GetChannel().GetId() {
		t.Fatalf("resume id = %q, want first %q", second.GetChannel().GetId(), first.GetChannel().GetId())
	}
}

// TestOpenDMDoubleHyphenHandlesResolveDistinctChannels pins the name-injectivity
// fix: with a `-`-delimited name, pair {a, b--c} and pair {a--b, c} would both
// derive the byte-identical `dm--a--b--c` and the second open would RESUME onto
// the first's channel, cross-adding members (a same-owner private-DM
// confidentiality break). The `:` separator is excluded from the handle grammar,
// so the two pairs map to distinct names (dm:a:b--c vs dm:a--b:c) and must
// resolve to DISTINCT channels. RED if dmChannelName reverts to a `-` delimiter.
func TestOpenDMDoubleHyphenHandlesResolveDistinctChannels(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	a := mustAgent(t, st, owner.ID, "a")
	bc := mustAgent(t, st, owner.ID, "b--c")
	ab := mustAgent(t, st, owner.ID, "a--b")
	c := mustAgent(t, st, owner.ID, "c")

	// Pair {a, b--c}.
	first, err := svc.OpenDM(WithActor(ctx, a.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "b--c"}))
	if err != nil {
		t.Fatalf("OpenDM(a->b--c) = %v, want success", err)
	}
	// Pair {a--b, c} — a DIFFERENT unordered pair. Must mint its OWN channel, not
	// resume the first pair's.
	second, err := svc.OpenDM(WithActor(ctx, ab.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "c"}))
	if err != nil {
		t.Fatalf("OpenDM(a--b->c) = %v, want success", err)
	}
	if !second.Msg.GetCreated() {
		t.Fatalf("second pair created = false, want true (a distinct pair must mint its own DM, not resume)")
	}
	if second.Msg.GetChannel().GetId() == first.Msg.GetChannel().GetId() {
		t.Fatalf("the two distinct pairs collided onto one channel %q — dmChannelName is not injective", first.Msg.GetChannel().GetId())
	}
	// And the first DM's membership is uncorrupted: exactly a + b--c (+ their
	// owner), never the second pair's ab or c.
	m := first.Msg.GetChannel().GetMemberAccountIds()
	if !containsString(m, string(a.ID)) || !containsString(m, string(bc.ID)) {
		t.Fatalf("first DM members = %v, want its own pair a=%s and b--c=%s", m, a.ID, bc.ID)
	}
	if containsString(m, string(ab.ID)) || containsString(m, string(c.ID)) {
		t.Fatalf("first DM members = %v, leaked the second pair's parties (ab=%s c=%s)", m, ab.ID, c.ID)
	}
}

// TestOpenDMResumeEmitsNoChannelChanged pins the paired half of the emit
// contract (emitDMCreated's created-only guard): a create fans a ChannelChanged,
// but a RESUME emits nothing new. Event-gated via a canary — a globally-visible
// AccountChanged published AFTER the resume; draining the caller's replay up to
// the canary must surface NO ChannelChanged for the DM (in-order per-subscriber
// delivery guarantees a resume event, if any, would precede the canary). RED if
// the created-only guard is dropped (a spurious per-resume re-publish).
func TestOpenDMResumeEmitsNoChannelChanged(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "owner")
	alice := mustAgent(t, h.store, owner.ID, "alice")
	mustAgent(t, h.store, owner.ID, "bob")

	// Create the DM first (drains the create event out of the way).
	created, err := h.svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM(create): %v", err)
	}
	dmID := created.Msg.GetChannel().GetId()

	// Resume the SAME pair — must emit nothing.
	if _, err := h.svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"})); err != nil {
		t.Fatalf("OpenDM(resume): %v", err)
	}

	// Canary published last; drain alice's replay up to it and count every
	// ChannelChanged for the DM across the whole replay. The create contributes
	// exactly 1, so a total >1 is a spurious per-resume re-publish (in-order
	// per-subscriber delivery guarantees a resume event, if any, precedes the canary).
	canary := mkCanary(t, h, "canary")
	evts := drainReplayAsActor(t, h, alice.ID, canary)
	var dmChannelChanges int
	for _, e := range evts {
		if cc := e.GetChannelChanged(); cc != nil && cc.GetChannel().GetId() == dmID {
			dmChannelChanges++
		}
	}
	if dmChannelChanges != 1 {
		t.Fatalf("ChannelChanged events for the DM = %d, want exactly 1 (the create only; a resume must emit nothing)", dmChannelChanges)
	}
}

// Each party posts under its own actor, and each read must return BOTH posts —
// red if DM membership fails to grant a party read on the peer's turn.
func TestOpenDMPostAndReadBothParties(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	bob := mustAgent(t, st, owner.ID, "bob")

	opened, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM(alice->bob) = %v, want success", err)
	}
	dmID := opened.Msg.GetChannel().GetId()

	post := func(actor store.AccountID, text string) string {
		t.Helper()
		resp, err := svc.PostMessage(WithActor(ctx, actor), connect.NewRequest(&compassv1.PostMessageRequest{
			Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: dmID},
			Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
			CreateTopic: true,
			Blocks:      textBlocks(text),
		}))
		if err != nil {
			t.Fatalf("PostMessage(%s): %v", text, err)
		}
		return resp.Msg.GetMessage().GetId()
	}
	aliceMsg := post(alice.ID, "from alice")
	bobMsg := post(bob.ID, "from bob")

	// Each party's read of the DM must surface BOTH posts — its own and the peer's.
	readSees := func(reader store.AccountID) map[string]bool {
		t.Helper()
		listed, err := svc.ListMessages(WithActor(ctx, reader), connect.NewRequest(&compassv1.ListMessagesRequest{
			Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: dmID},
		}))
		if err != nil {
			t.Fatalf("ListMessages(%s): %v", reader, err)
		}
		ids := map[string]bool{}
		for _, m := range listed.Msg.GetMessages() {
			ids[m.GetId()] = true
		}
		return ids
	}
	for _, party := range []struct {
		name string
		id   store.AccountID
	}{{"alice", alice.ID}, {"bob", bob.ID}} {
		got := readSees(party.id)
		if !got[aliceMsg] || !got[bobMsg] {
			t.Fatalf("%s reads DM = %v, want both alice %q and bob %q posts", party.name, got, aliceMsg, bobMsg)
		}
	}
}

// Same-owner is not membership: a third agent under the same owner reads
// nothing from a two-party DM. Canary-ordered, so an undelivered post cannot
// pass it vacuously. Red if read-scoping falls back to owner scope.
func TestOpenDMThirdPartySameOwnerCannotSee(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	alice := mustAgent(t, st, owner.ID, "alice")
	bob := mustAgent(t, st, owner.ID, "bob")
	carol := mustAgent(t, st, owner.ID, "carol")

	opened, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "bob"}))
	if err != nil {
		t.Fatalf("OpenDM(alice->bob) = %v, want success", err)
	}
	dmID := opened.Msg.GetChannel().GetId()

	posted, err := svc.PostMessage(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: dmID},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      textBlocks("private to alice and bob"),
	}))
	if err != nil {
		t.Fatalf("PostMessage(alice): %v", err)
	}
	msgID := posted.Msg.GetMessage().GetId()

	list := func(reader store.AccountID) []*compassv1.Message {
		t.Helper()
		listed, err := svc.ListMessages(WithActor(ctx, reader), connect.NewRequest(&compassv1.ListMessagesRequest{
			Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: dmID},
		}))
		if err != nil {
			t.Fatalf("ListMessages(%s): %v", reader, err)
		}
		return listed.Msg.GetMessages()
	}

	// Canary: bob, a real party, DOES read the post — proves it was delivered,
	// so the third-party emptiness below is a real negative, not a vacuous one.
	var bobSees bool
	for _, m := range list(bob.ID) {
		if m.GetId() == msgID {
			bobSees = true
		}
	}
	if !bobSees {
		t.Fatalf("party bob cannot read the DM post %q — canary failed, negative would be vacuous", msgID)
	}

	// carol, same owner but not a DM member, reads nothing.
	if got := list(carol.ID); len(got) != 0 {
		t.Fatalf("third same-owner agent carol read %d messages from a DM she is not a member of, want 0", len(got))
	}
}

// TestConvertBareThirdPartyAddOnDMIsInvalidArgument: adding a third party to a
// kind=DM channel WITHOUT convert_channel_name is INVALID_ARGUMENT — the wire
// contract's bare-add rejection, pinned at the handler tier.
func TestConvertBareThirdPartyAddOnDMIsInvalidArgument(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "t15owner")
	alice := mustAgent(t, st, owner.ID, "t15alice")
	mustAgent(t, st, owner.ID, "t15bob")
	mustAgent(t, st, owner.ID, "t15carol")

	opened, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "t15bob"}))
	if err != nil {
		t.Fatalf("OpenDM(t15alice->t15bob) = %v, want success", err)
	}

	_, addErr := svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:        opened.Msg.GetChannel().GetId(),
		AddMemberHandles: []string{"t15carol"},
	}))
	connectCodeIs(t, addErr, connect.CodeInvalidArgument, "bare third-party add on a DM without a convert name")
}

// TestConvertThirdPartyAddSucceedsWithAllEffects: the same add WITH
// convert_channel_name succeeds and the response channel carries every effect —
// kind flips to CHANNEL, name is the supplied one, the group is detached, the
// third party is a member, mandatory_subscription is off, and both original DM
// parties are subscribed.
func TestConvertThirdPartyAddSucceedsWithAllEffects(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "t15owner")
	alice := mustAgent(t, st, owner.ID, "t15alice")
	bob := mustAgent(t, st, owner.ID, "t15bob")
	carol := mustAgent(t, st, owner.ID, "t15carol")

	opened, err := svc.OpenDM(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: "t15bob"}))
	if err != nil {
		t.Fatalf("OpenDM(t15alice->t15bob) = %v, want success", err)
	}

	resp, err := svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:          opened.Msg.GetChannel().GetId(),
		AddMemberHandles:   []string{"t15carol"},
		ConvertChannelName: "t15-war-room",
	}))
	if err != nil {
		t.Fatalf("UpdateChannelMembers(convert) = %v, want success", err)
	}
	ch := resp.Msg.GetChannel()
	if ch.GetKind() != compassv1.ChannelKind_CHANNEL_KIND_CHANNEL {
		t.Fatalf("kind = %v, want CHANNEL_KIND_CHANNEL after convert", ch.GetKind())
	}
	if ch.GetName() != "t15-war-room" {
		t.Fatalf("name = %q, want t15-war-room after convert", ch.GetName())
	}
	if ch.GetGroupId() != "" {
		t.Fatalf("group_id = %q, want empty (detached from reserved DM group)", ch.GetGroupId())
	}
	if !containsString(ch.GetMemberAccountIds(), string(carol.ID)) {
		t.Fatalf("members = %v, want the added third party t15carol %s", ch.GetMemberAccountIds(), carol.ID)
	}
	if ch.GetMandatorySubscription() {
		t.Fatalf("mandatory_subscription = true, want false (a converted DM is an opt-in channel)")
	}
	if !containsString(ch.GetSubscriberAccountIds(), string(alice.ID)) || !containsString(ch.GetSubscriberAccountIds(), string(bob.ID)) {
		t.Fatalf("subscribers = %v, want both original DM parties t15alice %s and t15bob %s", ch.GetSubscriberAccountIds(), alice.ID, bob.ID)
	}
}

// TestConvertChannelNameIsNoOpOnNonDM: convert_channel_name on a kind=CHANNEL
// channel is ignored — the add succeeds and neither name nor kind change; it is
// not an error.
func TestConvertChannelNameIsNoOpOnNonDM(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "t15owner")
	newcomer := mustUser(t, st, "t15newcomer")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "t15-room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	}))
	if err != nil {
		t.Fatalf("CreateChannel = %v, want success", err)
	}

	resp, err := svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:          created.Msg.GetChannel().GetId(),
		AddMemberHandles:   []string{"t15newcomer"},
		ConvertChannelName: "t15-ignored",
	}))
	if err != nil {
		t.Fatalf("UpdateChannelMembers(add + convert name on non-DM) = %v, want success (name ignored, not an error)", err)
	}
	ch := resp.Msg.GetChannel()
	if ch.GetName() != "t15-room" {
		t.Fatalf("name = %q, want t15-room unchanged (convert_channel_name is a no-op on a non-DM)", ch.GetName())
	}
	if ch.GetKind() != compassv1.ChannelKind_CHANNEL_KIND_CHANNEL {
		t.Fatalf("kind = %v, want CHANNEL_KIND_CHANNEL unchanged", ch.GetKind())
	}
	if !containsString(ch.GetMemberAccountIds(), string(newcomer.ID)) {
		t.Fatalf("members = %v, want the added t15newcomer %s", ch.GetMemberAccountIds(), newcomer.ID)
	}
}
