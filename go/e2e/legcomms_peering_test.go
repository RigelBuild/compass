//go:build podman

package e2e

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

const (
	t5Owner1Handle = "t5-owner-1"
	t5Owner2Handle = "t5-owner-2"
	t5Agent1Handle = "t5-agent-1"
	t5Agent2Handle = "t5-agent-2"
	t5Agent3Handle = "t5-agent-3"
	t5Agent4Handle = "t5-agent-4"
	t5GhostHandle  = "t5-ghost-agent"

	t5SharedChannel = "t5-peer-room"
	t5Topic         = "general"
	t5EventMarker   = "t5-peer-lifecycle-event"
	t5ModelMarker   = "t5-peer-lifecycle-model"
	t5CanaryMarker  = "t5-peer-lifecycle-canary"
)

func registerPeeringFixtureOptions() {
	registerSharedFixtureOption(
		WithCannedMarkerReply(t5EventMarker, "peer lifecycle message received"),
		WithCannedMarkerReply(t5ModelMarker, "peer lifecycle message received"),
		WithCannedMarkerReply(t5CanaryMarker, "peer lifecycle canary received"),
	)
}

type commsStream = *connect.ServerStreamForClient[compassv1.SubscribeCommsResponse]

type sessionTail = *connect.ServerStreamForClient[compassv1.AgentSessionFrame]

// peerAgent is one T5 agent. Agents 1 and 2 run sessions; 3 and 4 never do.
type peerAgent struct {
	id    string
	home  string
	comms commsServiceClient
	tail  sessionTail
}

// peerOwner is one T5 owner with its observer clients and live comms stream.
type peerOwner struct {
	id      string
	compass compassServiceClient
	comms   commsServiceClient
	stream  commsStream
}

// peeringScene is the T5 cast: owner 1 holds agents 1 and 4, owner 2 holds 2 and 3.
type peeringScene struct {
	t                *testing.T
	ctx              context.Context
	f                *Fixture
	o1, o2           peerOwner
	a1, a2, a3, a4   peerAgent
	room             string
	peerHandle       string
	unknownRejection string
}

func TestCommsPeerLifecycleTransport(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}
	s := newPeeringScene(t)

	s.assertRejectedLikeUnknown("unpeered co-member", s.a1, s.peerHandle)
	s.assertNoReach("unpeered A→B", s.a4, s.room, "t5-a-unpeered-post "+t5EventMarker, s.o2)

	s.approve(s.o1, t5Owner2Handle, compassv1.PeeringState_PEERING_STATE_PENDING_OUTGOING)
	s.assertRejectedLikeUnknown("one-sided approval", s.a1, s.peerHandle)
	s.approve(s.o2, t5Owner1Handle, compassv1.PeeringState_PEERING_STATE_APPROVED)

	s.assertPeeredDM()
	dm4ID := s.openDM(s.a4)
	s.assertPeeredRoom()

	rctx, cancel := context.WithTimeout(s.ctx, rpcTimeout)
	revoked, err := s.o1.comms.RevokePeer(rctx, connect.NewRequest(&compassv1.RevokePeerRequest{PeerHandle: t5Owner2Handle}))
	cancel()
	if err != nil {
		t.Fatalf("RevokePeer(owner 2): %v", err)
	}
	if !revoked.Msg.GetDeleted() {
		t.Fatal("RevokePeer(owner 2) returned deleted=false")
	}
	s.assertRejectedLikeUnknown("revoked co-member", s.a1, s.peerHandle)
	s.assertRejectedLikeUnknown("revoked third agent", s.a1, t5Owner2Handle+"/"+t5Agent3Handle)
	s.assertNoReach("revoked A→B post", s.a4, s.room, "t5-a-revoked-post "+t5EventMarker, s.o2)
	s.assertNoReach("revoked A→B mention", s.a4, s.room, "t5-a-revoked-mention @"+s.peerHandle+" "+t5EventMarker, s.o2)
	s.assertNoReach("revoked B→A post", s.a3, s.room, "t5-b-revoked-post "+t5ModelMarker, s.o1)
	s.assertNoReach("revoked B→A mention", s.a3, s.room, "t5-b-revoked-mention @"+t5Owner1Handle+"/"+t5Agent1Handle+" "+t5ModelMarker, s.o1)
	s.assertNoReach("revoked existing DM post", s.a4, dm4ID, "t5-a-dm-revoked "+t5EventMarker, s.o2)

	s.approve(s.o1, t5Owner2Handle, compassv1.PeeringState_PEERING_STATE_APPROVED)
	restoredID := s.post("agent 1 restored shared post", s.a1.comms, s.room, "t5-a-restored-post "+t5EventMarker)
	s.observe("owner 2 restored shared post", s.o2.stream, restoredID, s.a1.id)
	s.settle("agent 1 restored shared post", s.o1, s.a1)
	s.assertDelivered("agent 2 restored shared post", s.a2.tail, restoredID)
}

func newPeeringScene(t *testing.T) *peeringScene {
	t.Helper()
	s := &peeringScene{t: t, ctx: context.Background(), f: sharedFixture(t), peerHandle: t5Owner2Handle + "/" + t5Agent2Handle}
	s.o1 = s.newOwner(t5Owner1Handle, "T5 Owner One")
	s.o2 = s.newOwner(t5Owner2Handle, "T5 Owner Two")
	s.a1 = s.newAgent(s.o1, t5Owner1Handle, t5Agent1Handle, "T5 Agent One")
	s.a2 = s.newAgent(s.o2, t5Owner2Handle, t5Agent2Handle, "T5 Agent Two")
	s.a3 = s.newAgent(s.o2, t5Owner2Handle, t5Agent3Handle, "T5 Agent Three")
	s.a4 = s.newAgent(s.o1, t5Owner1Handle, t5Agent4Handle, "T5 Agent Four")

	room, err := s.f.CreateChannel(s.ctx, s.o1.id, t5SharedChannel, false)
	if err != nil {
		t.Fatalf("CreateChannel(shared room): %v", err)
	}
	s.room = room
	s.addMember(s.o1.comms, t5Owner2Handle)
	s.addMember(s.o1.comms, t5Owner1Handle+"/"+t5Agent1Handle)
	s.addMember(s.o2.comms, t5Owner2Handle+"/"+t5Agent2Handle)
	s.addMember(s.o1.comms, t5Owner1Handle+"/"+t5Agent4Handle)
	s.addMember(s.o2.comms, t5Owner2Handle+"/"+t5Agent3Handle)

	s.o1.stream = s.subscribe(s.o1, "owner 1")
	s.o2.stream = s.subscribe(s.o2, "owner 2")
	s.a1.tail = s.startSession(s.o1, s.a1.id, "t5-agent-1")
	s.a2.tail = s.startSession(s.o2, s.a2.id, "t5-agent-2")

	code, msg := openDMRejection(s.ctx, t, s.a1.comms, t5GhostHandle)
	if code != connect.CodeNotFound {
		t.Fatalf("OpenDM(unknown %q) = %v, want NOT_FOUND", t5GhostHandle, code)
	}
	s.unknownRejection = strings.ReplaceAll(msg, t5GhostHandle, "<peer>")
	return s
}

func (s *peeringScene) newOwner(handle, name string) peerOwner {
	s.t.Helper()
	id, err := s.f.CreateUser(s.ctx, handle, name)
	if err != nil {
		s.t.Fatalf("CreateUser(%s): %v", handle, err)
	}
	compass, comms, err := s.f.AsObserver(s.ctx, handle)
	if err != nil {
		s.t.Fatalf("AsObserver(%s): %v", handle, err)
	}
	return peerOwner{id: id, compass: compass, comms: comms}
}

func (s *peeringScene) newAgent(owner peerOwner, ownerHandle, handle, name string) peerAgent {
	s.t.Helper()
	id, gotOwner, home := createAgentAs(s.ctx, s.t, owner.comms, handle, name)
	if gotOwner != owner.id {
		s.t.Fatalf("agent %s landed under owner %q, want %q", handle, gotOwner, owner.id)
	}
	_, comms, err := s.f.AsObserver(s.ctx, ownerHandle+"/"+handle)
	if err != nil {
		s.t.Fatalf("AsObserver(%s/%s): %v", ownerHandle, handle, err)
	}
	return peerAgent{id: id, home: home, comms: comms}
}

func (s *peeringScene) addMember(client commsServiceClient, handle string) {
	s.t.Helper()
	rctx, cancel := context.WithTimeout(s.ctx, rpcTimeout)
	defer cancel()
	_, err := client.UpdateChannelMembers(rctx, connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId: s.room, AddMemberHandles: []string{handle}, SubscribeHandles: []string{handle},
	}))
	if err != nil {
		s.t.Fatalf("UpdateChannelMembers(add %s): %v", handle, err)
	}
}

func (s *peeringScene) subscribe(owner peerOwner, name string) commsStream {
	s.t.Helper()
	stream, err := s.f.SubscribeCommsAsObserver(s.ctx, owner.comms, 0)
	if err != nil {
		s.t.Fatalf("SubscribeCommsAsObserver(%s): %v", name, err)
	}
	s.t.Cleanup(func() { _ = stream.Close() })
	awaitSubscriptionLive(s.ctx, s.t, stream)
	return stream
}

func (s *peeringScene) startSession(owner peerOwner, agentID, name string) sessionTail {
	s.t.Helper()
	container, err := s.f.Provision(s.ctx, agentID, name+"-provision")
	if err != nil {
		s.t.Fatalf("Provision(%s): %v", name, err)
	}
	s.t.Cleanup(func() {
		if err := s.f.RemoveWorkspace(s.ctx, container, name+"-teardown"); err != nil {
			s.t.Errorf("RemoveWorkspace(%s): %v", name, err)
		}
	})
	session, err := s.f.StartSession(s.ctx, container)
	if err != nil {
		s.t.Fatalf("StartSession(%s): %v", name, err)
	}
	tail, err := s.f.OpenSessionTailAs(s.ctx, owner.compass, session)
	if err != nil {
		s.t.Fatalf("OpenSessionTail(%s): %v", name, err)
	}
	s.t.Cleanup(func() { _ = tail.Close() })
	return tail
}

func (s *peeringScene) approve(owner peerOwner, peer string, want compassv1.PeeringState) {
	s.t.Helper()
	rctx, cancel := context.WithTimeout(s.ctx, rpcTimeout)
	defer cancel()
	resp, err := owner.comms.ApprovePeer(rctx, connect.NewRequest(&compassv1.ApprovePeerRequest{PeerHandle: peer}))
	if err != nil {
		s.t.Fatalf("ApprovePeer(%s): %v", peer, err)
	}
	if got := resp.Msg.GetPeering().GetState(); got != want {
		s.t.Fatalf("ApprovePeer(%s) state = %v, want %v", peer, got, want)
	}
}

func (s *peeringScene) openDM(a peerAgent) string {
	s.t.Helper()
	dm, err := a.comms.OpenDM(s.ctx, connect.NewRequest(&compassv1.OpenDMRequest{PeerHandle: s.peerHandle}))
	if err != nil {
		s.t.Fatalf("OpenDM(%s, mutually peered): %v", s.peerHandle, err)
	}
	return dm.Msg.GetChannel().GetId()
}

// assertPeeredDM opens agent 1's DM with agent 2 and proves delivery and history.
func (s *peeringScene) assertPeeredDM() {
	s.t.Helper()
	dmID := s.openDM(s.a1)
	body := "t5-a-dm-positive " + t5EventMarker
	msgID := s.post("agent 1 DM", s.a1.comms, dmID, body)
	s.observe("owner 2 DM", s.o2.stream, msgID, s.a1.id)
	s.settle("agent 1 DM", s.o1, s.a1)
	s.assertDelivered("agent 2 DM", s.a2.tail, msgID)
	list, err := s.a2.comms.ListMessages(s.ctx, connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: dmID},
		Limit:     20,
	}))
	if err != nil {
		s.t.Fatalf("agent 2 ListMessages(DM): %v", err)
	}
	for _, msg := range list.Msg.GetMessages() {
		if msg.GetId() == msgID && firstBlockText(msg) == body {
			return
		}
	}
	s.t.Fatalf("agent 2's DM history does not contain message %s", msgID)
}

// assertPeeredRoom proves room posts deliver and qualified mentions steer, both ways.
func (s *peeringScene) assertPeeredRoom() {
	s.t.Helper()
	aPostID := s.post("agent 1 shared post", s.a1.comms, s.room, "t5-a-approved-post "+t5EventMarker)
	s.observe("owner 2 shared post", s.o2.stream, aPostID, s.a1.id)
	s.settle("agent 1 shared post", s.o1, s.a1)
	s.assertDelivered("agent 2 shared post", s.a2.tail, aPostID)
	bPostID := s.post("agent 2 shared post", s.a2.comms, s.room, "t5-b-approved-post "+t5ModelMarker)
	s.observe("owner 1 shared post", s.o1.stream, bPostID, s.a2.id)
	s.settle("agent 2 shared post", s.o2, s.a2)
	s.assertDelivered("agent 1 shared post", s.a1.tail, bPostID)
	aMentionID := s.post("agent 1 qualified mention", s.a1.comms, s.room, "t5-a-approved-mention @"+s.peerHandle+" "+t5EventMarker)
	s.observe("owner 2 qualified mention", s.o2.stream, aMentionID, s.a1.id)
	s.settle("agent 1 qualified mention", s.o1, s.a1)
	s.assertMentioned("agent 2", s.a2.tail, aMentionID)
	bMentionID := s.post("agent 2 qualified mention", s.a2.comms, s.room,
		"t5-b-approved-mention @"+t5Owner1Handle+"/"+t5Agent1Handle+" "+t5ModelMarker)
	s.observe("owner 1 qualified mention", s.o1.stream, bMentionID, s.a2.id)
	s.settle("agent 2 qualified mention", s.o2, s.a2)
	s.assertMentioned("agent 1", s.a1.tail, bMentionID)
}

func (s *peeringScene) assertRejectedLikeUnknown(name string, a peerAgent, handle string) {
	s.t.Helper()
	code, msg := openDMRejection(s.ctx, s.t, a.comms, handle)
	if code != connect.CodeNotFound {
		s.t.Fatalf("%s OpenDM(%q) = %v, want NOT_FOUND", name, handle, code)
	}
	if got := strings.ReplaceAll(msg, handle, "<peer>"); got != s.unknownRejection {
		s.t.Fatalf("%s rejection %q is not redacted-identical to unknown rejection %q", name, got, s.unknownRejection)
	}
}

func (s *peeringScene) post(name string, client commsServiceClient, channelID, body string) string {
	s.t.Helper()
	id, err := s.f.PostMessageAsObserver(s.ctx, client, channelID, t5Topic, body)
	if err != nil {
		s.t.Fatalf("PostMessageAsObserver(%s): %v", name, err)
	}
	if id == "" {
		s.t.Fatalf("PostMessageAsObserver(%s) returned an empty id", name)
	}
	return id
}

func (s *peeringScene) observe(name string, stream commsStream, id, author string) {
	s.t.Helper()
	msg, err := s.f.AwaitDelivery(s.ctx, stream, func(m *compassv1.Message) bool { return m.GetId() == id })
	if err != nil {
		s.t.Fatalf("%s observer did not receive message %s: %v", name, id, err)
	}
	if msg.GetAuthorAccountId() != author {
		s.t.Fatalf("%s message author = %q, want %q", name, msg.GetAuthorAccountId(), author)
	}
}

// settle drives one owner-triggered turn of a live author, which fires its held posts.
func (s *peeringScene) settle(name string, owner peerOwner, author peerAgent) {
	s.t.Helper()
	s.post(name+" settle trigger", owner.comms, author.home, name+" "+t5EventMarker)
	if err := s.f.AwaitTurnSettled(s.ctx, author.tail); err != nil {
		s.t.Fatalf("%s author did not settle: %v", name, err)
	}
}

func (s *peeringScene) assertDelivered(name string, tail sessionTail, id string) {
	s.t.Helper()
	if _, err := s.f.AwaitControlDispatchOn(s.ctx, tail, func(kind, got string) bool {
		return got == id && strings.Contains(kind, "DELIVER")
	}); err != nil {
		s.t.Fatalf("%s did not receive DELIVER for %s: %v", name, id, err)
	}
	if err := s.f.AwaitTurnSettled(s.ctx, tail); err != nil {
		s.t.Fatalf("%s did not settle after %s: %v", name, id, err)
	}
}

func (s *peeringScene) assertMentioned(name string, tail sessionTail, id string) {
	s.t.Helper()
	kind, err := s.f.AwaitControlDispatchOn(s.ctx, tail, func(_, got string) bool { return got == id })
	if err != nil {
		s.t.Fatalf("%s received no session injection for qualified mention %s: %v", name, id, err)
	}
	if !strings.Contains(kind, "STEER") {
		s.t.Errorf("%s received %s for qualified mention %s, want STEER", name, kind, id)
	}
	if err := s.f.AwaitTurnSettled(s.ctx, tail); err != nil {
		s.t.Fatalf("%s did not settle after mention %s: %v", name, id, err)
	}
}

// assertNoReach posts as a sessionless author, then a canary from the recipient's owner.
// Sessionless posts fan out in fabric stream order, so a leak reaches the recipient first.
func (s *peeringScene) assertNoReach(name string, author peerAgent, channelID, body string, recipientOwner peerOwner) {
	s.t.Helper()
	recipient := s.a2.tail
	if recipientOwner.id == s.o1.id {
		recipient = s.a1.tail
	}
	deniedID := s.post(name+" denied", author.comms, channelID, body)
	s.observe(name+" denied event", recipientOwner.stream, deniedID, author.id)
	canaryID := s.post(name+" canary", recipientOwner.comms, s.room, name+" "+t5CanaryMarker)
	s.observe(name+" canary event", recipientOwner.stream, canaryID, recipientOwner.id)
	var reachedID string
	_, err := s.f.AwaitControlDispatchOn(s.ctx, recipient, func(kind, id string) bool {
		if id == deniedID || (id == canaryID && strings.Contains(kind, "DELIVER")) {
			reachedID = id
			return true
		}
		return false
	})
	if err != nil {
		s.t.Fatalf("%s target session received neither the denied post nor its canary: %v", name, err)
	}
	if reachedID != canaryID {
		s.t.Fatalf("%s target session received denied message %s before canary %s", name, deniedID, canaryID)
	}
	if err := s.f.AwaitTurnSettled(s.ctx, recipient); err != nil {
		s.t.Fatalf("%s canary did not settle: %v", name, err)
	}
}
