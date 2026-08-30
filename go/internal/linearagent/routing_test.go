package linearagent

// Unit tests for ResolveResponder (RIG-2717 T4). Table-driven over fakes for
// the two seams (OwnershipIndex + ManagerResolver), asserting the four routing
// outcomes of design §Part 2: a recorded coordinate whose authoring agent IS a
// Manager, a recorded coordinate whose authoring agent is a PEER (the walk must
// resolve peer -> owning Manager, never peer -> peer), an unknown coordinate
// (store.ErrNotFound -> supervisor + routing channel), and a bare @mention with
// no issue coordinate (-> supervisor + routing channel).
//
// context.Background() here is the test root — the sanctioned exemption to the
// thread-ctx rule.

import (
	"context"
	"errors"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	testForgeHost   = "linear.app"
	testSupervisor  = store.AccountID("acct-supervisor")
	testRoutingChan = "chan-routing"
)

// fakeOwnershipIndex scripts AuthoredArtifactByCoordinate: a single recorded
// artifact keyed by the (repo, number) coordinate, else store.ErrNotFound. It
// records the coordinate it was queried on so a test can assert the extraction.
type fakeOwnershipIndex struct {
	// row is returned when the queried number matches wantNumber (and wantRepo);
	// any other coordinate is a miss.
	wantRepo   string
	wantNumber uint64
	row        store.AuthoredArtifact

	gotProvider store.ForgeProvider
	gotHost     string
	gotRepo     string
	gotKind     store.ForgeArtifactKind
	gotNumber   uint64
	calls       int
}

func (f *fakeOwnershipIndex) AuthoredArtifactByCoordinate(_ context.Context, provider store.ForgeProvider, host, repo string, kind store.ForgeArtifactKind, number uint64) (store.AuthoredArtifact, error) {
	f.calls++
	f.gotProvider, f.gotHost, f.gotRepo, f.gotKind, f.gotNumber = provider, host, repo, kind, number
	if repo == f.wantRepo && number == f.wantNumber {
		return f.row, nil
	}
	return store.AuthoredArtifact{}, store.ErrNotFound
}

// fakeManagerResolver models the agent-tree walk: a map from a recorded
// authoring agent to its owning Manager + that Manager's home channel. A miss
// is store.ErrNotFound. It records the agent it was walked from so a test can
// assert the walk was invoked on the recorded authoring agent (not skipped).
type fakeManagerResolver struct {
	owners  map[store.AccountID]managerHome
	gotFrom store.AccountID
	calls   int
}

type managerHome struct {
	manager     store.AccountID
	homeChannel string
}

func (f *fakeManagerResolver) OwningManager(_ context.Context, agent store.AccountID) (store.AccountID, string, error) {
	f.calls++
	f.gotFrom = agent
	mh, ok := f.owners[agent]
	if !ok {
		return "", "", store.ErrNotFound
	}
	return mh.manager, mh.homeChannel, nil
}

func sessionEvent(identifier string) *SessionEvent {
	ev := &SessionEvent{Type: "AgentSessionEvent", Action: "created"}
	ev.AgentSession.Issue.Identifier = identifier
	return ev
}

func TestResolveResponder(t *testing.T) {
	ctx := context.Background()

	const (
		managerAgent = store.AccountID("acct-manager")
		peerAgent    = store.AccountID("acct-peer")
		mgrHome      = "chan-manager-home"
	)

	// A recorded row whose authoring agent is a Manager: the walk resolves the
	// Manager to itself (an agent whose owning Manager is itself).
	rowByManager := store.AuthoredArtifact{
		Provider: store.ForgeProviderLinear, Host: testForgeHost, Repo: "RIG",
		Kind: store.ForgeArtifactKindIssue, Number: 2717, AgentAccountID: managerAgent,
	}
	// A recorded row whose authoring agent is a transient peer: the walk must
	// climb to the peer's OWNING Manager, not return the peer.
	rowByPeer := store.AuthoredArtifact{
		Provider: store.ForgeProviderLinear, Host: testForgeHost, Repo: "RIG",
		Kind: store.ForgeArtifactKindIssue, Number: 2717, AgentAccountID: peerAgent,
	}

	tests := []struct {
		name        string
		identifier  string
		ownership   *fakeOwnershipIndex
		managers    *fakeManagerResolver
		wantManager store.AccountID
		wantChannel string
		wantWalk    store.AccountID // "" => the walk must NOT be invoked
	}{
		{
			name:       "recorded coordinate, authoring agent is a Manager",
			identifier: "RIG-2717",
			ownership:  &fakeOwnershipIndex{wantRepo: "RIG", wantNumber: 2717, row: rowByManager},
			managers: &fakeManagerResolver{owners: map[store.AccountID]managerHome{
				managerAgent: {manager: managerAgent, homeChannel: mgrHome},
			}},
			wantManager: managerAgent,
			wantChannel: mgrHome,
			wantWalk:    managerAgent,
		},
		{
			name:       "recorded coordinate, authoring agent is a PEER (the walk)",
			identifier: "RIG-2717",
			ownership:  &fakeOwnershipIndex{wantRepo: "RIG", wantNumber: 2717, row: rowByPeer},
			managers: &fakeManagerResolver{owners: map[store.AccountID]managerHome{
				peerAgent: {manager: managerAgent, homeChannel: mgrHome},
			}},
			wantManager: managerAgent, // peer -> owning Manager, NOT the peer
			wantChannel: mgrHome,
			wantWalk:    peerAgent,
		},
		{
			name:        "unknown coordinate -> supervisor + routing channel",
			identifier:  "RIG-9999",
			ownership:   &fakeOwnershipIndex{wantRepo: "RIG", wantNumber: 2717, row: rowByManager},
			managers:    &fakeManagerResolver{owners: map[store.AccountID]managerHome{}},
			wantManager: testSupervisor,
			wantChannel: testRoutingChan,
			wantWalk:    "",
		},
		{
			name:        "bare @mention, no issue -> supervisor + routing channel",
			identifier:  "",
			ownership:   &fakeOwnershipIndex{wantRepo: "RIG", wantNumber: 2717, row: rowByManager},
			managers:    &fakeManagerResolver{owners: map[store.AccountID]managerHome{}},
			wantManager: testSupervisor,
			wantChannel: testRoutingChan,
			wantWalk:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver(tc.ownership, tc.managers, testForgeHost, testSupervisor, testRoutingChan)

			gotManager, gotChannel, err := r.ResolveResponder(ctx, sessionEvent(tc.identifier))
			if err != nil {
				t.Fatalf("ResolveResponder: unexpected error: %v", err)
			}
			if gotManager != tc.wantManager {
				t.Errorf("manager = %q, want %q", gotManager, tc.wantManager)
			}
			if gotChannel != tc.wantChannel {
				t.Errorf("home channel = %q, want %q", gotChannel, tc.wantChannel)
			}

			// The walk must be invoked exactly when a row is recorded, and on the
			// RECORDED authoring agent — the peer case proves peer -> Manager, not
			// peer -> peer (a walk skipped or walked from the wrong agent reddens).
			if tc.wantWalk == "" {
				if tc.managers.calls != 0 {
					t.Errorf("manager walk invoked %d times, want 0 (fallback path)", tc.managers.calls)
				}
			} else {
				if tc.managers.calls != 1 {
					t.Fatalf("manager walk invoked %d times, want 1", tc.managers.calls)
				}
				if tc.managers.gotFrom != tc.wantWalk {
					t.Errorf("walk started from %q, want %q (must walk the RECORDED authoring agent)", tc.managers.gotFrom, tc.wantWalk)
				}
			}
		})
	}
}

// TestResolveResponderExtractsLinearCoordinate pins the coordinate extraction:
// a "TEAM-NUMBER" identifier maps to (Linear, config host, team=repo, number)
// and queries the issue kind. A regression that swaps team/number or drops the
// host would reroute or miss the ownership row.
func TestResolveResponderExtractsLinearCoordinate(t *testing.T) {
	ctx := context.Background()
	own := &fakeOwnershipIndex{wantRepo: "RIG", wantNumber: 2717, row: store.AuthoredArtifact{AgentAccountID: "acct-x"}}
	mgr := &fakeManagerResolver{owners: map[store.AccountID]managerHome{"acct-x": {manager: "m", homeChannel: "c"}}}
	r := NewResolver(own, mgr, testForgeHost, testSupervisor, testRoutingChan)

	if _, _, err := r.ResolveResponder(ctx, sessionEvent("RIG-2717")); err != nil {
		t.Fatalf("ResolveResponder: %v", err)
	}
	if own.gotProvider != store.ForgeProviderLinear {
		t.Errorf("queried provider = %d, want Linear (%d)", own.gotProvider, store.ForgeProviderLinear)
	}
	if own.gotHost != testForgeHost {
		t.Errorf("queried host = %q, want %q", own.gotHost, testForgeHost)
	}
	if own.gotRepo != "RIG" {
		t.Errorf("queried repo = %q, want team key %q", own.gotRepo, "RIG")
	}
	if own.gotNumber != 2717 {
		t.Errorf("queried number = %d, want 2717", own.gotNumber)
	}
	if own.gotKind != store.ForgeArtifactKindIssue {
		t.Errorf("queried kind = %d, want issue (%d)", own.gotKind, store.ForgeArtifactKindIssue)
	}
}

// TestResolveResponderPropagatesOwnershipError proves a non-NotFound store
// failure is surfaced, not swallowed into the supervisor fallback (which would
// silently misroute on a transient DB fault).
func TestResolveResponderPropagatesOwnershipError(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("store: connection reset")
	own := &erroringOwnershipIndex{err: boom}
	mgr := &fakeManagerResolver{owners: map[store.AccountID]managerHome{}}
	r := NewResolver(own, mgr, testForgeHost, testSupervisor, testRoutingChan)

	_, _, err := r.ResolveResponder(ctx, sessionEvent("RIG-2717"))
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the store failure propagated", err)
	}
	if mgr.calls != 0 {
		t.Errorf("manager walk invoked %d times on ownership error, want 0", mgr.calls)
	}
}

type erroringOwnershipIndex struct{ err error }

func (e *erroringOwnershipIndex) AuthoredArtifactByCoordinate(_ context.Context, _ store.ForgeProvider, _, _ string, _ store.ForgeArtifactKind, _ uint64) (store.AuthoredArtifact, error) {
	return store.AuthoredArtifact{}, e.err
}
