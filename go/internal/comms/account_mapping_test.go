// The account kind discriminant at the store <-> proto edge (mapping.go
// accountToWire): a store Account carries exactly one of User / Agent / System,
// and the wire Account.kind oneof must reflect that side. accountToWire is a
// pure function of its argument, so the contract is fully observable with NO
// database — this file is untagged and runs on the default `go test` lane.
//
// The system arm is the T2 addition: a System-subtype account must emit the
// Account_System oneof case, never fall through to an unset kind (which the UI
// documents as the malformed-row fallback, so a system account landing there
// would silently render as a plain user).

package comms

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestAccountToWireEmitsKindPerSubtype(t *testing.T) {
	tests := []struct {
		name string
		acc  store.Account
		want func(*compassv1.Account) bool
	}{
		{
			name: "user",
			acc:  store.Account{ID: "u1", Handle: "matt", User: &store.UserAccount{Role: store.UserRoleMember}},
			want: func(a *compassv1.Account) bool { _, ok := a.GetKind().(*compassv1.Account_User); return ok },
		},
		{
			name: "agent",
			acc:  store.Account{ID: "a1", Handle: "cook", Agent: &store.AgentAccount{OwnerUserID: "u1"}},
			want: func(a *compassv1.Account) bool { _, ok := a.GetKind().(*compassv1.Account_Agent); return ok },
		},
		{
			name: "system",
			acc:  store.Account{ID: "s1", Handle: store.SystemAccountHandle, System: &store.SystemAccount{}},
			want: func(a *compassv1.Account) bool { _, ok := a.GetKind().(*compassv1.Account_System); return ok },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := accountToWire(tt.acc)
			if out.GetId() != string(tt.acc.ID) || out.GetHandle() != tt.acc.Handle {
				t.Fatalf("scalar fields not carried: got id=%q handle=%q", out.GetId(), out.GetHandle())
			}
			if !tt.want(out) {
				t.Fatalf("wire kind = %T, want the %s oneof case", out.GetKind(), tt.name)
			}
		})
	}
}
